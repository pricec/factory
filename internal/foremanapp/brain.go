package foremanapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/pricec/factory/internal/llmctx"
	"github.com/pricec/factory/internal/wire"
)

// Brain session constants (spec §9.1, §9.3, §9.4 harness pattern).
const (
	// brainTimeout bounds one brain opencode session.
	brainTimeout = 300 * time.Second
	// envBrainToken carries the brain bearer into the child so the
	// generated config can reference it via {env:FOREMAN_BRAIN_TOKEN}
	// without the secret landing in the file at rest.
	envBrainToken = "FOREMAN_BRAIN_TOKEN"
	// envOpenCODEConfig points the child at the generated config.
	envOpenCODEConfig = "OPENCODE_CONFIG"
)

// BrainRunner runs one-shot foreman-brain LLM sessions (spec §9.3): an
// opencode child configured with the foreman MCP surface, woken with a
// prompt built from the triggering envelope (spec §6.6). It implements
// routing.Brain.
type BrainRunner struct {
	// Model is the opencode model string; "" -> opencode default.
	Model string
	// MCPURL is the foreman brain MCP endpoint (/v1/mcp/foreman).
	MCPURL string
	// Bearer is the brain token (a local foreman:{id} credential).
	Bearer string
	// WorkDir is where the generated config temp file lives.
	WorkDir string
	// Exec runs one brain session for a prompt and returns its stdout.
	// nil means the real opencode child. Tests replace it with a
	// recorder; no test ever invokes a real opencode.
	Exec func(ctx context.Context, prompt string) (string, error)
}

// Handle wakes the brain for env (spec §3.3): it builds the prompt via
// llmctx (payloads hard-capped at 2KB, spec §6.6), runs one session, and
// logs the stdout tail. Errors are returned for the router to log - the
// brain never panics.
func (b *BrainRunner) Handle(ctx context.Context, env wire.MessageEnvelope) error {
	prompt := llmctx.BuildWakePrompt("New messages need decisions.", []wire.MessageEnvelope{env})
	exec := b.Exec
	if exec == nil {
		exec = b.runOpencode
	}
	stdout, err := exec(ctx, prompt)
	if err != nil {
		return fmt.Errorf("brain session for %s %s: %w", env.Type, env.ID, err)
	}
	log.Printf("foremanapp: brain handled %s %s; stdout: %s", env.Type, env.ID, snippet(stdout))
	return nil
}

// runOpencode is the default Exec: it generates the opencode config
// in a temp file under WorkDir (mcp.servers.foreman -> MCPURL, bearer via
// {env:FOREMAN_BRAIN_TOKEN}, oauth off, optional model), then runs
// `opencode run <prompt>` non-interactively in its own process group with
// a 300s timeout.
func (b *BrainRunner) runOpencode(ctx context.Context, prompt string) (string, error) {
	configPath, err := b.writeConfig()
	if err != nil {
		return "", err
	}
	defer os.Remove(configPath)

	ctx, cancel := context.WithTimeout(ctx, brainTimeout)
	defer cancel()
	// CommandContext (not Command): cmd.Cancel below requires it.
	cmd := exec.CommandContext(ctx, "opencode", "run", prompt)
	cmd.Dir = b.WorkDir
	env := withEnvValue(os.Environ(), envBrainToken, b.Bearer)
	cmd.Env = withEnvValue(env, envOpenCODEConfig, configPath)
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
		if ctx.Err() == context.DeadlineExceeded {
			return stdout.String(), fmt.Errorf("brain session exceeded %s", brainTimeout)
		}
		return stdout.String(), fmt.Errorf("opencode run: %w; stderr: %s", err, snippet(stderr.String()))
	}
	return stdout.String(), nil
}

// writeConfig generates the opencode config (the §8.4 pattern, ~20 lines
// reimplemented here: workerharness's generator is unexported and pins the
// worker server name/token env) as a temp file under WorkDir, 0600.
func (b *BrainRunner) writeConfig() (string, error) {
	if err := os.MkdirAll(b.WorkDir, 0o755); err != nil {
		return "", fmt.Errorf("brain work dir: %w", err)
	}
	f, err := os.CreateTemp(b.WorkDir, ".brain-opencode-*.json")
	if err != nil {
		return "", err
	}
	cfg := brainOpenCODEConfig{
		Model: b.Model,
		MCP: brainMCPBlock{Servers: map[string]*brainRemoteMCPServer{
			"foreman": {
				Type:    "remote",
				URL:     b.MCPURL,
				Headers: map[string]string{"Authorization": "Bearer {env:" + envBrainToken + "}"},
				OAuth:   false,
			},
		}},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	data = append(data, '\n')
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// brainOpenCODEConfig and friends mirror the opencode V2 config shape
// (docs/mcp-servers) used by workerharness.occonfig.
type brainRemoteMCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	OAuth   bool              `json:"oauth"`
}

type brainMCPBlock struct {
	Servers map[string]*brainRemoteMCPServer `json:"servers"`
}

type brainOpenCODEConfig struct {
	Model string        `json:"model,omitempty"`
	MCP   brainMCPBlock `json:"mcp"`
}

// withEnvValue returns base with key set to value, replacing any earlier
// assignment (workerharness twin).
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

// snippet returns the first 200 chars of s for logging.
func snippet(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}
