package factoryapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pricec/factory/internal/llmctx"
	"github.com/pricec/factory/internal/wire"
)

// Planner brain session constants (spec §9.4 harness pattern, §9.5).
const (
	// plannerBrainTimeout bounds one planner opencode session.
	plannerBrainTimeout = 300 * time.Second
	// plannerConfigName is the generated opencode config written into
	// WorkDir (spec §8.4). It must be the V2 project-config name
	// `opencode.json`: V2 discovers config from the working directory,
	// and the V1-era OPENCODE_CONFIG env var is ignored by V2 (upstream
	// regression, umbrella issue #36990) - pointing the child at a temp
	// file silently lost MCP injection, permissions, and the model pin.
	plannerConfigName = "opencode.json"
	// envPlannerToken carries the planner bearer into the child so the
	// generated config can reference it via {env:FACTORY_PLANNER_TOKEN}
	// without the secret landing in the file at rest.
	envPlannerToken = "FACTORY_PLANNER_TOKEN"
)

// PlannerBrain runs one-shot factory-planner LLM sessions (spec §9.5): an
// opencode child configured with the factory's own MCP surface
// (/v1/mcp/factory, planner bearer), woken with a prompt built from the
// operator inbox history (spec §6.6: per-payload 2KB caps). The agent drives
// the factory with the MCP tools (create_job, list_jobs, read_result,
// message_foreman, list_foremen) and replies to the operator with
// notify_operator. It implements routing.Brain semantics for planner:main.
type PlannerBrain struct {
	// Model is the opencode model string; "" -> opencode default.
	Model string
	// MCPURL is the factory planner MCP endpoint (/v1/mcp/factory).
	MCPURL string
	// Bearer is the factory planner token.
	Bearer string
	// WorkDir is where the generated config temp file lives.
	WorkDir string
	// Exec runs one brain session for a prompt and returns its stdout.
	// nil means the real opencode child. Tests replace it with a
	// recorder; no test ever invokes a real opencode.
	Exec func(ctx context.Context, prompt string) (string, error)
}

// Handle wakes the planner for envs (spec §9.5): prompt = wake reason +
// recent operator-inbox history + the triggering envelopes, all
// payload-capped via llmctx (spec §6.6). Errors are returned for the loop
// to log and surface to the operator - the brain never panics.
func (b *PlannerBrain) Handle(ctx context.Context, reason string, history, envs []wire.MessageEnvelope) error {
	prompt := llmctx.BuildWakePrompt(reason, append(history, envs...))
	exec := b.Exec
	if exec == nil {
		exec = b.runOpencode
	}
	stdout, err := exec(ctx, prompt)
	if err != nil {
		return fmt.Errorf("planner session for %d envelope(s): %w", len(envs), err)
	}
	log.Printf("factoryapp: planner handled %d envelope(s); stdout: %s", len(envs), snippet(stdout))
	return nil
}

// runOpencode is the default Exec: it regenerates the opencode project
// config in WorkDir (mcp.servers.factory -> MCPURL, bearer via
// {env:FACTORY_PLANNER_TOKEN}, oauth off, pinned model, allow-all
// permissions), then runs `opencode run <prompt>` non-interactively in
// its own process group with a 300s timeout. The child discovers the
// config from its working directory (V2 project config; OPENCODE_CONFIG
// is ignored by V2). Mirrors foremanapp's BrainRunner (#9.3 pattern).
func (b *PlannerBrain) runOpencode(ctx context.Context, prompt string) (string, error) {
	if _, err := b.writeConfig(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, plannerBrainTimeout)
	defer cancel()
	// CommandContext (not Command): cmd.Cancel below requires it.
	// --standalone: a private server inside this child process. Without
	// it the shared background service loads the generated config and
	// substitutes the {env:FACTORY_PLANNER_TOKEN} header against its own
	// environment - which lacks the token - and the MCP connect dies
	// with 401 "missing bearer token", stripping the session of every
	// factory tool.
	cmd := exec.CommandContext(ctx, "opencode", "run", "--standalone", prompt)
	cmd.Dir = b.WorkDir
	cmd.Env = withEnvValue(os.Environ(), envPlannerToken, b.Bearer)
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
			return stdout.String(), fmt.Errorf("planner session exceeded %s", plannerBrainTimeout)
		}
		return stdout.String(), fmt.Errorf("opencode run: %w; stderr: %s", err, snippet(stderr.String()))
	}
	return stdout.String(), nil
}

// writeConfig regenerates the opencode project config at
// WorkDir/opencode.json, 0600 (the §8.4 pattern; foremanapp.BrainRunner
// twin). The file persists across runs - each run rewrites it, so the
// child always starts from the freshly generated config.
func (b *PlannerBrain) writeConfig() (string, error) {
	if err := os.MkdirAll(b.WorkDir, 0o755); err != nil {
		return "", fmt.Errorf("planner work dir: %w", err)
	}
	path := filepath.Join(b.WorkDir, plannerConfigName)
	cfg := plannerOpenCODEConfig{
		Model: b.Model,
		MCP: plannerMCPBlock{Servers: map[string]*plannerRemoteMCPServer{
			"factory": {
				Type:    "remote",
				URL:     b.MCPURL,
				Headers: map[string]string{"Authorization": "Bearer {env:" + envPlannerToken + "}"},
				OAuth:   false,
			},
		}},
		Permissions: plannerPermissions,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	return path, os.WriteFile(path, data, 0o600)
}

// plannerOpenCODEConfig and friends mirror the opencode V2 config shape
// (docs/mcp-servers) used by workerharness.occonfig / foremanapp.brain.
type plannerRemoteMCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	OAuth   bool              `json:"oauth"`
}

type plannerMCPBlock struct {
	Servers map[string]*plannerRemoteMCPServer `json:"servers"`
}

// plannerPermissionRule is one opencode V2 permission rule (docs/
// permissions); plannerPermissions pins allow-all for the headless child.
type plannerPermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// plannerPermissions is the planner child's permission policy: a headless
// `opencode run` has no client to answer an "ask", so any prompt
// auto-declines and aborts the whole session mid-flight (exit status 1,
// work already done discarded - observed live). The child already runs
// with the service user's authority; an explicit allow-all keeps it alive.
var plannerPermissions = []plannerPermissionRule{
	{Action: "*", Resource: "*", Effect: "allow"},
}

type plannerOpenCODEConfig struct {
	Model       string                  `json:"model,omitempty"`
	MCP         plannerMCPBlock         `json:"mcp"`
	Permissions []plannerPermissionRule `json:"permissions"`
}

// withEnvValue returns base with key set to value, replacing any earlier
// assignment (foremanapp twin).
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
