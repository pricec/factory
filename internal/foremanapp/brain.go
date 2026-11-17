package foremanapp

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
	"github.com/pricec/factory/internal/ocusage"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/routing"
	"github.com/pricec/factory/internal/wire"
)

// Brain session constants (spec §9.1, §9.3, §9.4 harness pattern).
const (
	// brainTimeout bounds one brain opencode session: 30m, headroom for
	// long multi-tool foreman-brain turns (2026-11-15 operator decision;
	// was 300s/5m).
	brainTimeout = 1800 * time.Second
	// brainConfigName is the generated opencode config written into
	// WorkDir (spec §8.4): the V2 project-config name, discovered from
	// the working directory. The V1-era OPENCODE_CONFIG env var is
	// ignored by V2 (upstream regression, umbrella issue #36990).
	brainConfigName = "opencode.json"
	// envBrainToken carries the brain bearer into the child so the
	// generated config can reference it via {env:FOREMAN_BRAIN_TOKEN}
	// without the secret landing in the file at rest.
	envBrainToken = "FOREMAN_BRAIN_TOKEN"
)

// BrainRunner runs one-shot foreman-brain LLM sessions (spec §9.3): an
// opencode child configured with the foreman MCP surface, woken with a
// prompt built from the triggering envelope (spec §6.6). It implements
// routing.Brain.
type BrainRunner struct {
	// Model is the opencode model string; "" -> opencode default.
	Model string
	// ModelFn, when set, resolves the model per session - the settings
	// panel's live override path (spec §6.5, 2026-11-15 amendment):
	// ModelFn wins over the static Model field so panel changes apply to
	// the next brain session without a restart. nil -> the static field.
	ModelFn func() string
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

// currentModel resolves the session model: ModelFn when wired (the
// settings pull path), else the static Model field.
func (b *BrainRunner) currentModel() string {
	if b.ModelFn != nil {
		return b.ModelFn()
	}
	return b.Model
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

// runOpencode is the default Exec: it regenerates the opencode project
// config in WorkDir (mcp.servers.foreman -> MCPURL, bearer via
// {env:FOREMAN_BRAIN_TOKEN}, oauth off, pinned model, allow-all
// permissions), then runs `opencode run <prompt>` non-interactively in
// its own process group with a 30m timeout. The child discovers the
// config from its working directory (V2 project config; the V1-era
// OPENCODE_CONFIG env var is ignored by V2).
func (b *BrainRunner) runOpencode(ctx context.Context, prompt string) (string, error) {
	if _, err := b.writeConfig(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, brainTimeout)
	defer cancel()
	// CommandContext (not Command): cmd.Cancel below requires it.
	// --standalone: a private server inside this child process, so the
	// generated config's {env:FOREMAN_BRAIN_TOKEN} header substitution
	// sees this child's environment (the shared background service's
	// env lacks the token and would connect with an empty bearer). The
	// model rides -m, not the config's model field: V2 drops `#variant`
	// suffixes from the config field, and overrides may carry one.
	model := b.currentModel()
	args := []string{"run", "--standalone"}
	if model != "" {
		args = append(args, "-m", model)
	}
	cmd := exec.CommandContext(ctx, "opencode", append(args, prompt)...)
	cmd.Dir = b.WorkDir
	cmd.Env = withEnvValue(os.Environ(), envBrainToken, b.Bearer)
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

// writeConfig regenerates the opencode project config (the §8.4 pattern,
// ~20 lines reimplemented here: workerharness's generator is unexported
// and pins the worker server name/token env) at WorkDir/opencode.json,
// 0600. The file persists across runs - each run rewrites it.
func (b *BrainRunner) writeConfig() (string, error) {
	if err := os.MkdirAll(b.WorkDir, 0o755); err != nil {
		return "", fmt.Errorf("brain work dir: %w", err)
	}
	path := filepath.Join(b.WorkDir, brainConfigName)
	cfg := brainOpenCODEConfig{
		Model: b.currentModel(),
		MCP: brainMCPBlock{Servers: map[string]*brainRemoteMCPServer{
			"foreman": {
				Type:    "remote",
				URL:     b.MCPURL,
				Headers: map[string]string{"Authorization": "Bearer {env:" + envBrainToken + "}"},
				OAuth:   false,
			},
		}},
		Permissions: brainPermissions,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	return path, os.WriteFile(path, data, 0o600)
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
	Model       string                `json:"model,omitempty"`
	MCP         brainMCPBlock         `json:"mcp"`
	Permissions []brainPermissionRule `json:"permissions"`
}

// brainPermissionRule is one opencode V2 permission rule (docs/
// permissions); brainPermissions pins allow-all for the headless child.
type brainPermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// brainPermissions is the brain child's permission policy: a headless
// `opencode run` has no client to answer an "ask", so any prompt
// auto-declines and aborts the whole session mid-flight (exit status 1,
// work already done discarded). The child already runs with the foreman
// user's authority; an explicit allow-all keeps it alive.
var brainPermissions = []brainPermissionRule{
	{Action: "*", Resource: "*", Effect: "allow"},
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

// BrainFailedType is the envelope type a foreman posts to the factory
// when a brain session failed (spec §3.2, 2026-11-15 amendment): without
// it the routing layer logs the failure, the envelope acks, and the
// factory-side planner waits forever on a delegation that died -
// observed live when a brain session spent its whole 5m bound reading
// code and never acted, silently.
const BrainFailedType = "brain.failed"

// brainReporter wraps the brain for the router (spec §3.3): a failed
// non-question session posts a brain.failed envelope to the factory in
// addition to the router's log, so the failure reaches the planner
// loop (which can fall back - e.g. create_job - and tell the operator).
// job.question failures are exempt: ask_parent's default answer is the
// pinned fallback there (#17), so a report would only add noise.
//
// It also reports session usage for EVERY brain session - success,
// failure, or question (spec §6.5 reporting amendment): brain sessions
// carry no JobResult, so without this post their spend is invisible to
// the factory's ledger.
type brainReporter struct {
	inner  routing.Brain
	poster routing.Poster
	from   string
	// workDir is the brain child's working directory - the key the
	// session-accounting read matches sessions by.
	workDir string
	// usage overrides the session read (test seam); nil means the real
	// reader: the host's opencode database via ocusage.
	usage func(dir string, sinceMs int64) ([]ocusage.Session, error)
}

// Handle runs the wrapped brain, reports the session's usage, and
// reports non-question failures upstream. Reports are best-effort:
// posting errors log and never mask the brain's own error.
func (b *brainReporter) Handle(ctx context.Context, env wire.MessageEnvelope) error {
	start := time.Now()
	err := b.inner.Handle(ctx, env)
	b.reportUsage(ctx, env, start)
	if err == nil || env.Type == "job.question" {
		return err
	}
	payload, merr := json.Marshal(map[string]string{
		"envelope_id":   env.ID,
		"envelope_type": env.Type,
		"error":         snippet(err.Error()),
	})
	if merr != nil {
		log.Printf("foremanapp: report brain failure for %s %s: %v", env.Type, env.ID, merr)
		return err
	}
	if perr := b.poster.PostToParent(ctx, wire.MessageEnvelope{
		Type:    BrainFailedType,
		JobID:   env.JobID,
		From:    b.from,
		Payload: payload,
	}); perr != nil {
		log.Printf("foremanapp: report brain failure for %s %s: %v", env.Type, env.ID, perr)
	}
	return err
}

// reportUsage posts one session.usage envelope per session opencode
// recorded for the brain's working directory since the session started
// (spec §3.2, §6.5 reporting amendment). Read failures log - usage is
// best-effort accounting, never a session failure.
func (b *brainReporter) reportUsage(ctx context.Context, env wire.MessageEnvelope, start time.Time) {
	if b.workDir == "" {
		// No work dir wired (unit stubs): nothing to read.
		return
	}
	read := b.usage
	if read == nil {
		read = func(dir string, sinceMs int64) ([]ocusage.Session, error) {
			ctx, cancel := context.WithTimeout(context.Background(), ocusage.UsageReadTimeout)
			defer cancel()
			dbPath, err := ocusage.DBPathCtx(ctx, "")
			if err != nil {
				return nil, err
			}
			return ocusage.ReadSince(dbPath, dir, sinceMs)
		}
	}
	sessions, err := read(b.workDir, start.UnixMilli())
	if err != nil {
		log.Printf("foremanapp: brain session usage read: %v", err)
		return
	}
	durationMs := time.Since(start).Milliseconds()
	// Post on a detached, bounded context: a job.question session
	// answers under the 60s SLA, and accounting must not race the
	// remainder of it - usage is reported even when the caller's
	// context is about to expire.
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ocusage.UsageReadTimeout)
	defer cancel()
	for _, s := range sessions {
		u := wire.SessionUsage{
			SessionID:        s.ID,
			Role:             wire.SessionRoleForemanBrain,
			ForemanID:        registry.BareID(b.from),
			JobID:            env.JobID,
			Model:            s.Model,
			InputTokens:      s.InputTokens,
			OutputTokens:     s.OutputTokens,
			ReasoningTokens:  s.ReasoningTokens,
			CacheReadTokens:  s.CacheReadTokens,
			CacheWriteTokens: s.CacheWriteTokens,
			CostUSD:          s.CostUSD,
			DurationMs:       durationMs,
		}
		payload, err := json.Marshal(u)
		if err != nil {
			log.Printf("foremanapp: brain session usage %s: %v", s.ID, err)
			continue
		}
		if err := b.poster.PostToParent(postCtx, wire.MessageEnvelope{
			Type:    SessionUsageType,
			JobID:   u.JobID,
			From:    b.from,
			Payload: payload,
		}); err != nil {
			log.Printf("foremanapp: brain session usage %s: %v", s.ID, err)
		}
	}
}
