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

	"github.com/pricec/factory/internal/claudecli"
	"github.com/pricec/factory/internal/llmctx"
	"github.com/pricec/factory/internal/occonfig"
	"github.com/pricec/factory/internal/ocusage"
	"github.com/pricec/factory/internal/routing"
	"github.com/pricec/factory/internal/wire"
)

// Brain session constants (spec §9.1, §9.3, §9.4 harness pattern).
const (
	// brainTimeout bounds one brain session (either runner): 30m, headroom
	// for long multi-tool foreman-brain turns (2026-11-15 operator
	// decision; was 300s/5m).
	brainTimeout = 1800 * time.Second
	// brainConfigName is the generated opencode config written into
	// WorkDir (spec §8.4): the V2 project-config name, discovered from
	// the working directory. The V1-era OPENCODE_CONFIG env var is
	// ignored by V2 (upstream regression, umbrella issue #36990). Written
	// only by opencode-runner sessions (§9.3 runner routing).
	brainConfigName = "opencode.json"
	// brainClaudeServer is the MCP server name in the generated Claude
	// Code config (spec §9.3): Claude Code prefixes its tools with it, so
	// the foreman surface arrives as mcp__foreman__<tool>.
	brainClaudeServer = "foreman"
	// envBrainToken carries the brain bearer into the child so the
	// generated config can reference it without the secret landing in
	// the file at rest: {env:FOREMAN_BRAIN_TOKEN} on the opencode runner,
	// ${FOREMAN_BRAIN_TOKEN} on the claude runner.
	envBrainToken = "FOREMAN_BRAIN_TOKEN"
)

// BrainRunner runs one-shot foreman-brain LLM sessions (spec §9.3): a CLI
// child configured with the foreman MCP surface, woken with a prompt built
// from the triggering envelope (spec §6.6). The runner routes on the
// session model (spec §9.3, the §8.4 worker twin): a claude-* model runs
// the Claude Code CLI, everything else opencode. It implements
// routing.Brain.
type BrainRunner struct {
	// Model is the child CLI model string; "" -> the CLI's default. A
	// claude-* model selects the claude runner (FACTORY_FOREMAN_BRAIN_MODEL).
	Model string
	// ModelFn, when set, resolves the model per session - the settings
	// panel's live override path (spec §6.5, 2026-11-15 amendment):
	// ModelFn wins over the static Model field so panel changes apply to
	// the next brain session without a restart. nil -> the static field.
	// It is read once per session so the runner route and the model the
	// child runs never disagree.
	ModelFn func() string
	// ClaudeBin is the Claude Code CLI binary; "" means "claude" on PATH.
	// Used only by sessions routed to the claude runner.
	ClaudeBin string
	// ExtraEnv, when set, supplies env for claude sessions (spec §13.3:
	// the factory's CLAUDE_CODE_OAUTH_TOKEN).
	ExtraEnv func() map[string]string
	// MCPURL is the foreman brain MCP endpoint (/v1/mcp/foreman).
	MCPURL string
	// Bearer is the brain token (a local foreman:{id} credential).
	Bearer string
	// WorkDir is where the generated config temp file lives.
	WorkDir string
	// Exec runs one brain session for a prompt and returns its stdout.
	// nil means the real child CLI, routed on the session model (§9.3).
	// Tests replace it with a recorder; no test ever invokes a real CLI.
	Exec func(ctx context.Context, prompt string) (string, error)
	// OnUsage, when set, receives the usage of each run: parsed from the
	// claude CLI's output, or read per session from the opencode database
	// after an opencode run (internal/ocusage).
	OnUsage func(ctx context.Context, u claudecli.Usage)
	// ReadUsage reads the sessions opencode recorded for a dir since a ms
	// timestamp; nil means the host's opencode database. Test seam.
	ReadUsage ocusage.Reader
	// OpenCODEVersion pins the opencode config schema; the zero value
	// detects it from `opencode --version` (V1 if that fails).
	OpenCODEVersion occonfig.Version
}

// Handle wakes the brain for env (spec §3.3): it builds the prompt via
// llmctx (payloads hard-capped at 2KB, spec §6.6), runs one session, and
// logs the stdout tail. Errors are returned for the router to log - the
// brain never panics.
func (b *BrainRunner) Handle(ctx context.Context, env wire.MessageEnvelope) error {
	prompt := llmctx.BuildWakePrompt("New messages need decisions.", []wire.MessageEnvelope{env})
	s := b.session()
	exec := s.Exec
	if exec == nil {
		if claudecli.IsModel(s.Model) {
			exec = s.runClaude
		} else {
			exec = s.runOpencode
		}
	}
	stdout, err := exec(ctx, prompt)
	if err != nil {
		return fmt.Errorf("brain session for %s %s: %w", env.Type, env.ID, err)
	}
	log.Printf("foremanapp: brain handled %s %s; stdout: %s", env.Type, env.ID, snippet(stdout))
	return nil
}

// currentModel resolves the session model: ModelFn when wired (the
// settings pull path), else the static Model field.
func (b *BrainRunner) currentModel() string {
	if b.ModelFn != nil {
		return b.ModelFn()
	}
	return b.Model
}

// session returns a copy of b with the model resolved once for one
// session (spec §6.5).
func (b *BrainRunner) session() *BrainRunner {
	c := *b
	c.Model = b.currentModel()
	c.ModelFn = nil
	return &c
}

// CurrentModel returns the brain's current effective model (the
// settings-synced value when wired; spec §6.5).
func (b *BrainRunner) CurrentModel() string { return b.currentModel() }

// runClaude is the claude-runner Exec (spec §9.3, 2026-11-16
// foreman-brain-claude amendment): it regenerates the Claude Code MCP
// config in WorkDir (mcpServers.foreman -> MCPURL, bearer via
// ${FOREMAN_BRAIN_TOKEN}) and runs the Claude Code CLI through the shared
// claudecli runner under the 30m brain bound - the same flags and error
// semantics as the planner's claude runner (spec §9.4). Missing CLI and
// missing auth fail the session loudly; the router surfaces the error.
func (b *BrainRunner) runClaude(ctx context.Context, prompt string) (string, error) {
	configPath, err := b.writeClaudeConfig()
	if err != nil {
		return "", err
	}
	text, usage, err := claudecli.Exec(ctx, claudecli.Session{
		Bin:        b.ClaudeBin,
		Model:      b.Model,
		ConfigPath: configPath,
		Dir:        b.WorkDir,
		TokenEnv:   envBrainToken,
		Token:      b.Bearer,
		Prompt:     prompt,
		Timeout:    brainTimeout,
		Label:      "brain session",
		ExtraEnv:   b.extraEnv(),
	})
	if b.OnUsage != nil && !usage.IsZero() {
		b.OnUsage(ctx, usage)
	}
	return text, err
}

func (b *BrainRunner) extraEnv() map[string]string {
	if b.ExtraEnv == nil {
		return nil
	}
	return b.ExtraEnv()
}

// writeClaudeConfig regenerates the Claude Code MCP config at
// WorkDir/claude-mcp.json, 0600 (spec §9.3). The model rides --model and
// the headless permission policy rides --permission-mode; the file carries
// only the MCP injection.
func (b *BrainRunner) writeClaudeConfig() (string, error) {
	return claudecli.WriteConfig(b.WorkDir, brainClaudeServer, b.MCPURL, envBrainToken)
}

// runOpencode is the opencode-runner Exec (the non-claude route, spec
// §9.3): it regenerates the opencode project config in WorkDir
// (mcp.servers.foreman -> MCPURL, bearer via {env:FOREMAN_BRAIN_TOKEN},
// oauth off, pinned model, allow-all permissions), then runs
// `opencode run <prompt>` non-interactively in its own process group with
// a 30m timeout. The child discovers the config from its working directory
// (V2 project config; the V1-era OPENCODE_CONFIG env var is ignored by V2).
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
	args := []string{"run", "--standalone"}
	if b.Model != "" {
		args = append(args, "-m", b.Model)
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
	started := time.Now()
	err := cmd.Run()
	b.recordOpencodeUsage(ctx, started)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return stdout.String(), fmt.Errorf("brain session exceeded %s", brainTimeout)
		}
		return stdout.String(), fmt.Errorf("opencode run: %w; stderr: %s", err, snippet(stderr.String()))
	}
	return stdout.String(), nil
}

// recordOpencodeUsage reports the sessions opencode recorded for WorkDir
// since started through OnUsage - opencode prints no usage, so this is the
// opencode route's capture (the claude route parses stdout). It runs on
// success and failure alike (a failed session still spent tokens), under a
// detached context bounded by ocusage.UsageReadTimeout so an expired
// session context cannot drop the report. Read errors log.
func (b *BrainRunner) recordOpencodeUsage(ctx context.Context, started time.Time) {
	if b.OnUsage == nil {
		return
	}
	durationMs := time.Since(started).Milliseconds()
	read := b.ReadUsage
	if read == nil {
		read = ocusage.ReaderFor("")
	}
	sessions, err := read(b.WorkDir, started.UnixMilli())
	if err != nil {
		log.Printf("foremanapp: brain opencode usage read: %v", err)
		return
	}
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ocusage.UsageReadTimeout)
	defer cancel()
	for _, s := range sessions {
		b.OnUsage(uctx, s.Usage(b.Model, durationMs))
	}
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
	cfg := occonfig.Build(occonfig.Resolve(b.OpenCODEVersion, "opencode"), b.Model, occonfig.Remote{
		Name:     "foreman",
		URL:      b.MCPURL,
		TokenEnv: envBrainToken,
	})
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	return path, os.WriteFile(path, data, 0o600)
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

// BrainFailedType is the envelope a foreman posts upstream when a brain
// session fails on a delegated envelope (spec §3.2). Without it the
// router logs the failure and acks, and the operator waits forever on a
// delegation that died silently.
const BrainFailedType = "brain.failed"

// brainReporter wraps the brain for the router (spec §3.3): a failed
// non-question session posts a brain.failed envelope to the factory in
// addition to the router's log, so the failure reaches the planner loop
// (which relays it to the operator). job.question failures are exempt:
// ask_parent's default answer is the pinned fallback there (spec §9.2),
// so a report would only add noise.
type brainReporter struct {
	inner  routing.Brain
	poster routing.Poster
	from   string
}

// Handle runs the wrapped brain and reports non-question failures
// upstream. The report is best-effort: a posting error logs and never
// masks the brain's own error.
func (b *brainReporter) Handle(ctx context.Context, env wire.MessageEnvelope) error {
	err := b.inner.Handle(ctx, env)
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
