// Package claudecli runs the Claude Code CLI as a one-shot coordinator
// child: the factory planner (spec §9.4) and the foreman brain (spec
// §9.3). It owns the claude-* model routing, the argument vector, the
// generated MCP config, and the bounded run with the loud-failure error
// semantics, so the two coordinator roles share one code path. It is the
// brain-shaped twin of the worker harness's claude runner (spec §8.4),
// which keeps its own copy: the worker's per-job config carries more
// than MCP, and that package is out of this change's scope.
package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ConfigName is the generated Claude Code MCP config file name, written
// into the child's WorkDir (spec §9.3/§9.4). It is passed explicitly via
// --mcp-config, so the name is never discovered by claude's project scan;
// a non-`.mcp.json` name additionally keeps that scan from ever
// double-loading it.
const ConfigName = "claude-mcp.json"

// defaultBin is the Claude Code CLI binary resolved on PATH when a
// session names none.
const defaultBin = "claude"

// IsModel reports whether a session model routes to the Claude Code CLI
// (spec §9.3/§9.4, the §8.4 worker routing twin): bare `claude-*` ids (or
// the bare `claude` alias), optionally behind the `anthropic/` provider
// prefix. OpenRouter-hosted claude models (`openrouter/anthropic/claude-...`)
// stay on opencode - a different credential path (the OpenRouter key), not
// the Anthropic CLI's own auth.
func IsModel(model string) bool {
	m := strings.TrimPrefix(model, "anthropic/")
	return m == "claude" || strings.HasPrefix(m, "claude-")
}

// ModelArg maps the session model onto the `claude --model` flag value:
// the Anthropic CLI takes bare model ids and aliases, so the `anthropic/`
// provider prefix (accepted on the model string for uniformity with the
// opencode shape) is stripped. The rest passes through verbatim - operators
// pin exact claude ids or aliases.
func ModelArg(model string) string {
	return strings.TrimPrefix(model, "anthropic/")
}

// Args builds the claude-runner argument vector (spec §8.4/§9.4), verified
// against the installed CLI (2.1.325): `-p` non-interactive print mode; the
// pinned model on --model (omitted when unset - the CLI default); the
// generated config as the ONLY MCP source (--strict-mcp-config - the
// isolation analog of opencode --standalone, so no user or project servers
// leak into a one-shot child); bypassed permissions (a headless run has no
// one to answer a permission prompt - the opencode allow-all analog); no
// persisted session (one-shot, nothing to resume); and the prompt as the
// FINAL positional argument - --mcp-config is variadic and would otherwise
// swallow it.
func Args(model, configPath, prompt string) []string {
	args := []string{"-p"}
	if model != "" {
		args = append(args, "--model", ModelArg(model))
	}
	return append(args,
		"--mcp-config", configPath,
		"--strict-mcp-config",
		"--permission-mode", "bypassPermissions",
		"--no-session-persistence",
		prompt,
	)
}

// HTTPServer is one generated Claude Code MCP server entry: type "http" is
// the streamable-HTTP transport (the opencode `remote` analog), and headers
// ride the server.
type HTTPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// Config is the generated Claude Code MCP config. It carries only the MCP
// injection: no model field (the model rides --model) and no permissions
// block (the headless policy rides --permission-mode).
type Config struct {
	MCPServers map[string]HTTPServer `json:"mcpServers"`
}

// WriteConfig regenerates <dir>/claude-mcp.json, 0600, before every run.
// It carries one streamable-HTTP MCP server named server, pointing at url,
// with the bearer referenced as ${tokenEnv}: Claude Code expands that
// against the child's environment, so the secret never lands in the file
// at rest. The server name is what claude prefixes its tools with
// (`mcp__<server>__<tool>`).
func WriteConfig(dir, server, url, tokenEnv string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("claude work dir: %w", err)
	}
	path := filepath.Join(dir, ConfigName)
	cfg := Config{MCPServers: map[string]HTTPServer{
		server: {
			Type:    "http",
			URL:     url,
			Headers: map[string]string{"Authorization": "Bearer ${" + tokenEnv + "}"},
		},
	}}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	return path, os.WriteFile(path, data, 0o600)
}

// Session is one claude-runner invocation.
type Session struct {
	// Bin is the Claude Code CLI binary; "" means "claude" on PATH.
	Bin string
	// Model is the session model; "" -> the CLI default.
	Model string
	// ConfigPath is the generated claude-mcp.json (see WriteConfig).
	ConfigPath string
	// Dir is the child's working directory (the role's WorkDir).
	Dir string
	// TokenEnv names the env var carrying the bearer; Token is its value.
	TokenEnv string
	Token    string
	// Prompt is the wake prompt, passed as the final argument.
	Prompt string
	// Timeout bounds the session.
	Timeout time.Duration
	// Label names the session in timeout errors, e.g. "brain session".
	Label string
}

// Exec runs one session non-interactively in its own process group under
// Timeout and returns the child's stdout. Errors keep the claude-runner
// semantics (spec §8.5/§9.4): a missing CLI fails loudly by name; a
// non-zero exit with stderr surfaces stderr; a non-zero exit with EMPTY
// stderr surfaces stdout, because Claude Code reports in-run failures -
// missing auth among them - as the result on stdout (observed live on
// 2.1.325). Nothing here is silent: every failure returns an error.
func Exec(parent context.Context, s Session) (string, error) {
	ctx, cancel := context.WithTimeout(parent, s.Timeout)
	defer cancel()
	bin := s.Bin
	if bin == "" {
		bin = defaultBin
	}
	// CommandContext (not Command): cmd.Cancel below requires it.
	cmd := exec.CommandContext(ctx, bin, Args(s.Model, s.ConfigPath, s.Prompt)...)
	cmd.Dir = s.Dir
	cmd.Env = withEnvValue(os.Environ(), s.TokenEnv, s.Token)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Our own bound names the session; a caller's deadline (e.g. the
		// router's job.question SLA) is reported as the caller's, not ours.
		if parent.Err() != nil {
			return stdout.String(), fmt.Errorf("claude run cancelled: %w", parent.Err())
		}
		if ctx.Err() == context.DeadlineExceeded {
			return stdout.String(), fmt.Errorf("%s exceeded %s", s.Label, s.Timeout)
		}
		// A bare name missing from PATH is exec.ErrNotFound; an explicit
		// path that does not exist is fs.ErrNotExist.
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("claude CLI not found (%s): install the Claude Code CLI on this host: %w", bin, err)
		}
		if detail := snippet(stderr.String()); detail != "" {
			return stdout.String(), fmt.Errorf("claude run: %w; stderr: %s", err, detail)
		}
		return stdout.String(), fmt.Errorf("claude run: %w; stdout: %s", err, snippet(stdout.String()))
	}
	return stdout.String(), nil
}

// withEnvValue returns base with key set to value, replacing any earlier
// assignment.
func withEnvValue(base []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return append(out, prefix+value)
}

// snippet returns the first 200 chars of s for error text.
func snippet(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}
