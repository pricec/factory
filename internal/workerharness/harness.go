package workerharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pricec/factory/internal/claudecli"
	"github.com/pricec/factory/internal/ocusage"

	"github.com/pricec/factory/internal/wire"
)

// Pinned harness constants (spec §8).
const (
	// jobsDirName is the per-job directory root under WorkDir (§8.2).
	jobsDirName = "jobs"
	// configFileName is the generated opencode config in the job dir
	// (§8.4). It must be the V2 project-config name `opencode.json`:
	// V2 discovers config from the working directory, and the V1-era
	// OPENCODE_CONFIG env var and .opencode.json dotfile are both
	// ignored by V2 (upstream regression, umbrella issue #36990) - the
	// child would silently lose MCP injection, permissions, and the
	// model pin.
	configFileName = "opencode.json"
	// flagFileName is the report_result flag file written by the worker MCP
	// surface (§8.5).
	flagFileName = ".result_posted"
	// envWorkerToken carries the per-job token into the child so the
	// generated config can reference it via {env:WORKER_TOKEN} without
	// writing the secret to disk.
	envWorkerToken = "WORKER_TOKEN"
	// noOutputSummary is the fallback summary when stdout is empty (§8.5).
	noOutputSummary = "(no output)"
	// crashMessageLimit caps the crash error message length.
	crashMessageLimit = 512
)

// Child runner names (§8.4): which CLI runs the job's agent child.
const (
	// RunnerOpencode is the opencode CLI (the GLM tiers, §6.5).
	RunnerOpencode = "opencode"
	// RunnerClaude is the Claude Code CLI (claude-* models, §8.4).
	RunnerClaude = "claude"
)

// Config configures a single harness run. JobPath, ResultURL, MCPURL, Token,
// and WorkDir mirror the worker process interface (§8.1); the remaining
// fields are optional seams, mainly for tests.
type Config struct {
	// JobPath is the path to the dispatched job JSON.
	JobPath string
	// ResultURL is the inbox endpoint that receives the fallback JobResult.
	ResultURL string
	// MCPURL is the foreman's worker MCP endpoint injected into the child.
	MCPURL string
	// Token is the per-job worker bearer token.
	Token string
	// WorkDir is the root for per-job directories (§8.2).
	WorkDir string
	// Model optionally pins the agent model as "provider/model" (§8.4):
	// a claude-* model routes the run to the Claude Code CLI, everything
	// else to opencode.
	Model string

	// OpenCODEBin is the opencode binary; "" means "opencode" on PATH.
	OpenCODEBin string
	// ReadUsage reads the sessions opencode recorded for a dir since a
	// ms timestamp: the opencode-runner usage capture (opencode prints no
	// usage). Production wires ocusage.ReaderFor; nil skips the capture
	// (tests whose fake OpenCODEBin must not be re-invoked for discovery).
	ReadUsage ocusage.Reader
	// ClaudeBin is the Claude Code CLI binary; "" means "claude" on
	// PATH. Used only by runs routed to the claude runner (§8.4).
	ClaudeBin string
	// Env is the child environment; nil means the worker process
	// environment. WORKER_TOKEN is always overridden.
	Env []string
	// SecretEnv holds credential env vars (name -> value) merged into the
	// runner child's environment ONLY (spec §13.2). Values live in memory
	// for the run; they are never written to job.json, config files, logs
	// or posted envelopes (child output is redacted of them).
	SecretEnv map[string]string
	// Cancel, when closed, stops the worker: SIGTERM to the child's process
	// group, SIGKILL after CancelGrace. Run then returns ErrCancelled and
	// posts NO result (the job is already terminal at the factory).
	Cancel <-chan struct{}
	// CancelGrace is the SIGTERM->SIGKILL grace for Cancel; zero means 10s.
	CancelGrace time.Duration
	// Post delivers the fallback JobResult; nil means HTTP with Bearer
	// auth. Retried at-least-once (§8.5).
	Post PostFunc
	// PostUsage delivers the captured usage event; nil means HTTP with
	// Bearer auth. Best effort: failures are logged, never fatal.
	PostUsage UsagePostFunc
	// PostRetryBackoff is the delay between post attempts; zero means 1s.
	PostRetryBackoff time.Duration
	// Timeout overrides the spec's timeout_s (the wall-clock ceiling). A
	// test seam for sub-second timeouts; zero means timeout_s applies.
	Timeout time.Duration
	// IdleTimeout overrides the spec's idle_timeout_s; same seam role.
	IdleTimeout time.Duration
	// LoopLimit is how many back-to-back tool calls with identical name,
	// input and result trip the loop guard (kill, reason=loop); zero means
	// DefaultLoopLimit, negative disables it. JobSpec.loop_limit, when
	// non-zero, overrides it per job.
	LoopLimit int
	// TermGrace is how long a budget kill waits after SIGTERM before
	// SIGKILL; zero means 5s.
	TermGrace time.Duration
}

// DefaultLoopLimit is the loop guard's default threshold.
const DefaultLoopLimit = wire.DefaultLoopLimit

// defaults validates the required fields and fills the optional seams.
func (c *Config) defaults() error {
	if c.JobPath == "" {
		return errors.New("job path: required")
	}
	if c.ResultURL == "" {
		return errors.New("result url: required")
	}
	if c.MCPURL == "" {
		return errors.New("mcp url: required")
	}
	if c.Token == "" {
		return errors.New("token: required")
	}
	if c.WorkDir == "" {
		return errors.New("work dir: required")
	}
	if c.OpenCODEBin == "" {
		c.OpenCODEBin = "opencode"
	}
	if c.ClaudeBin == "" {
		c.ClaudeBin = "claude"
	}
	if c.Post == nil {
		c.Post = httpPost
	}
	if c.PostUsage == nil {
		c.PostUsage = httpPostUsage
	}
	if c.PostRetryBackoff <= 0 {
		c.PostRetryBackoff = defaultPostRetryBackoff
	}
	return nil
}

// Run executes one job (spec §8): it loads the JobSpec, prepares the fresh
// per-job directory with the routed runner's generated config, runs the
// agent child non-interactively (opencode, or the Claude Code CLI for
// claude-* models - §8.4), and reports the result. The primary report is
// the agent calling report_result, detected via the flag file; otherwise
// Run posts the fallback JobResult to ResultURL. It returns nil when the
// agent reported or the fallback was posted, and an error otherwise.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.defaults(); err != nil {
		return err
	}
	spec, err := loadSpec(cfg.JobPath)
	if err != nil {
		return err
	}
	claude := isClaudeModel(cfg.Model)

	jobDir := filepath.Join(cfg.WorkDir, jobsDirName, spec.ID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return fmt.Errorf("job dir: %w", err)
	}
	if err := os.Remove(filepath.Join(jobDir, flagFileName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear report flag: %w", err)
	}
	// The generated config lands in the child's working directory (§8.4):
	// the V2 project-config path opencode.json for the opencode runner
	// (the child discovers it from cwd), the --mcp-config file for the
	// claude runner (passed explicitly). Each runner gets only its own
	// file - the other would be dead weight.
	name := configFileName
	if claude {
		name = claudeConfigFileName
		if err := writeConfig(filepath.Join(jobDir, name), newClaudeConfig(cfg)); err != nil {
			return fmt.Errorf("generated config: %w", err)
		}
	} else if err := writeConfig(filepath.Join(jobDir, name), newOpenCODEConfig(cfg)); err != nil {
		return fmt.Errorf("generated config: %w", err)
	}

	stdout, stderr := newSyncBuffer(), newSyncBuffer()
	lim := limits{
		ceiling: jobTimeout(cfg, spec), idle: jobIdleTimeout(cfg, spec),
		maxCostUSD: spec.Budget.MaxCostUSD, loopLimit: cfg.LoopLimit,
	}
	if spec.LoopLimit != 0 { // per-job override (negative disables)
		lim.loopLimit = spec.LoopLimit
	}
	if lim.loopLimit == 0 {
		lim.loopLimit = wire.DefaultLoopLimit
	}
	started := time.Now()
	runErr, kill, harnessErr := execChild(ctx, cfg, lim, jobDir, buildPrompt(spec, runnerFor(cfg.Model)), stdout, stderr)
	if harnessErr != nil {
		return harnessErr
	}
	out, errOut := stdout.String(), stderr.String()
	// Best effort (spec §13.2): scrub secret values the agent echoed to its
	// output before anything is logged or posted.
	out, errOut = redactSecrets(cfg.SecretEnv, out), redactSecrets(cfg.SecretEnv, errOut)
	if runErr != nil {
		runErr = redactedError{redactSecrets(cfg.SecretEnv, runErr.Error())}
	}
	if claude {
		// Claude runs emit one JSON result object (--output-format json):
		// the harness ALWAYS records the usage it parsed, before anything
		// else, and it overrides whatever the agent reported. Text falls
		// back to raw stdout when unparseable.
		parsed := claudecli.ParseOutput(out)
		out = parsed.Text
		usage := claudecli.UsageWithStderr(parsed.Usage, errOut)
		if usage.IsZero() && kill.reason != "" && kill.costUSD > 0 {
			// Killed before the CLI printed its result object: record the
			// live estimate so the spend is not lost.
			usage = claudecli.Usage{CostUSD: kill.costUSD, Model: cfg.Model}
		}
		recordUsage(ctx, cfg, spec, usage)
	} else {
		// opencode prints no usage: read the sessions it recorded for
		// this job dir since the child started and post each as a
		// job.usage event. Synchronous on purpose: the per-job bearer is
		// revoked when the harness returns (spec §7.4 usage-401), so a
		// background post would race the revocation. Bounded by
		// ocusage.UsageReadTimeout; failures log, never fail the job.
		durationMs := time.Since(started).Milliseconds()
		for _, s := range readRunUsage(cfg, jobDir, started.UnixMilli()) {
			recordUsage(ctx, cfg, spec, s.Usage(cfg.Model, durationMs))
		}
	}

	if kill.reason == reasonCancelled {
		log.Printf("worker harness: job %s: cancelled by the factory; no result posted", spec.ID)
		return ErrCancelled
	}

	if _, err := os.Stat(filepath.Join(jobDir, flagFileName)); err == nil {
		log.Printf("worker harness: job %s: agent reported via report_result; stdout: %s", spec.ID, snippet(out))
		return nil
	}

	var res wire.JobResult
	if kill.reason != "" {
		res = killResult(spec, out, kill, lim)
	} else {
		res = fallbackResult(spec, out, errOut, runErr, claude)
	}
	if err := res.Validate(nil); err != nil {
		return fmt.Errorf("fallback result for %s: %w", spec.ID, err)
	}
	if err := postResult(ctx, cfg, res); err != nil {
		return fmt.Errorf("job %s: %w", spec.ID, err)
	}
	log.Printf("worker harness: job %s: posted fallback result status=%s; stderr: %s", spec.ID, res.Status, snippet(errOut))
	return nil
}

// recordUsage posts the parsed CLI usage as a job.usage envelope. Best
// effort: a failed post is logged and never fails the job.
func recordUsage(ctx context.Context, cfg Config, spec wire.JobSpec, u claudecli.Usage) {
	if u.IsZero() {
		return
	}
	ev := wire.UsageEvent{
		Kind: wire.UsageKindWorker, JobID: spec.ID, Model: u.Model, Tier: spec.Tier,
		Input: u.Input, Output: u.Output, CacheCreate: u.CacheCreate, CacheRead: u.CacheRead,
		CostUSD: u.CostUSD, SessionID: u.SessionID, Limit: u.Limit, RateLimit: u.RateLimit,
		DurationMs: u.DurationMs,
	}
	if ev.Model == "" {
		ev.Model = cfg.Model
	}
	if err := cfg.PostUsage(ctx, cfg.ResultURL, cfg.Token, ev); err != nil {
		log.Printf("worker harness: job %s: post usage: %v", spec.ID, err)
	}
}

// readRunUsage reads the opencode sessions recorded for the run's dir
// since sinceMs via cfg.ReadUsage (nil: none). Errors log and degrade to
// none.
func readRunUsage(cfg Config, dir string, sinceMs int64) []ocusage.Session {
	if cfg.ReadUsage == nil {
		return nil
	}
	sessions, err := cfg.ReadUsage(dir, sinceMs)
	if err != nil {
		log.Printf("worker harness: opencode usage read for %s: %v", filepath.Base(dir), err)
		return nil
	}
	return sessions
}

// loadSpec reads the job JSON, applies the pinned defaults, and validates
// it: a dispatched job must carry a job_-prefixed id (§5.1, §8.1).
func loadSpec(path string) (wire.JobSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return wire.JobSpec{}, err
	}
	var spec wire.JobSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return wire.JobSpec{}, fmt.Errorf("job spec %s: %w", path, err)
	}
	spec.Normalize()
	if err := spec.Validate(); err != nil {
		return wire.JobSpec{}, fmt.Errorf("job spec %s: %w", path, err)
	}
	return spec, nil
}

// jobTimeout resolves the run timeout: cfg.Timeout when set, otherwise the
// spec's timeout_s (§8.6).
func jobTimeout(cfg Config, spec wire.JobSpec) time.Duration {
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return time.Duration(spec.TimeoutS) * time.Second
}

// jobIdleTimeout resolves the idle limit: cfg.IdleTimeout when set,
// otherwise the spec's idle_timeout_s (default 900s).
func jobIdleTimeout(cfg Config, spec wire.JobSpec) time.Duration {
	if cfg.IdleTimeout > 0 {
		return cfg.IdleTimeout
	}
	return time.Duration(spec.IdleTimeoutS) * time.Second
}

// isClaudeModel reports whether the pinned model routes to the Claude
// Code CLI (§8.4): bare `claude-*` ids (or the bare `claude` alias),
// optionally behind the `anthropic/` provider prefix. OpenRouter-hosted
// claude models (`openrouter/anthropic/claude-...`) stay on opencode -
// they are a different credential path (the OpenRouter key), not the
// Anthropic CLI's own auth.
func isClaudeModel(model string) bool {
	m := strings.TrimPrefix(model, "anthropic/")
	return m == "claude" || strings.HasPrefix(m, "claude-")
}

// runnerFor returns the child runner a job pinned to model runs under
// (§8.4): the Claude Code CLI for claude-* models, opencode otherwise.
func runnerFor(model string) string {
	if isClaudeModel(model) {
		return RunnerClaude
	}
	return RunnerOpencode
}

// RunnerFor reports the child runner a job pinned to model runs under
// (§8.4): RunnerClaude for claude-* models (Claude Code CLI),
// RunnerOpencode for everything else. The spawner logs it next to the
// pinned model; the routing decision itself is enforced by Run.
func RunnerFor(model string) string { return runnerFor(model) }

// claudeModelArg maps the pinned model onto the `claude --model` flag
// value: the Anthropic CLI takes bare model ids and aliases, so the
// `anthropic/` provider prefix (accepted on the env override for
// uniformity with the opencode model-string shape) is stripped. The rest
// passes through verbatim - operators pin exact claude ids or aliases.
func claudeModelArg(model string) string {
	return strings.TrimPrefix(model, "anthropic/")
}

// runArgs builds the `opencode run` argument vector: --standalone (spec
// §8.4) and, when a model is pinned, `-m <model>` - reasoning-effort
// variants like `...#max` do NOT survive the generated config's `model`
// field (V2 drops the #variant there), so the flag is the only reliable
// injection point.
func runArgs(cfg Config, prompt string) []string {
	args := []string{"run", "--standalone"}
	if cfg.Model != "" {
		args = append(args, "-m", cfg.Model)
	}
	return append(args, prompt)
}

// claudeArgs builds the Claude Code CLI argument vector (§8.4), verified
// against the installed CLI (2.1.325): `-p` non-interactive print mode
// with the pinned model; the generated MCP config as the ONLY MCP source
// (--strict-mcp-config - the isolation analog of opencode --standalone:
// no user/project servers leak into a per-job child); bypassed
// permissions (a headless run has no one to answer a permission prompt -
// the opencode allow-all analog, §8.4); no persisted session (one-shot
// worker, nothing to resume); and the prompt as the FINAL positional
// argument - `--mcp-config` is variadic and would otherwise swallow it.
func claudeArgs(cfg Config, mcpFile, prompt string) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if cfg.Model != "" {
		args = append(args, "--model", claudeModelArg(cfg.Model))
	}
	return append(args,
		"--mcp-config", mcpFile,
		"--strict-mcp-config",
		"--permission-mode", "bypassPermissions",
		"--no-session-persistence",
		prompt,
	)
}

// childCommand picks the child CLI and its argument vector for the job's
// routed runner (§8.4): claude-* models run the Claude Code CLI with the
// per-job generated MCP config; everything else runs `opencode run
// --standalone` with the per-job generated opencode.json.
func childCommand(cfg Config, jobDir, prompt string) (string, []string) {
	if isClaudeModel(cfg.Model) {
		return cfg.ClaudeBin, claudeArgs(cfg, filepath.Join(jobDir, claudeConfigFileName), prompt)
	}
	return cfg.OpenCODEBin, runArgs(cfg, prompt)
}

// execChild runs the routed agent child (§8.4) with cwd jobDir:
// `opencode run --standalone [-m model] <prompt>` for opencode models -
// the generated opencode.json there is the V2 project config the child
// discovers from its working directory, and --standalone boots a private
// server inside the child process - the shared background service would
// substitute the config's {env:WORKER_TOKEN} header against its own
// environment (no per-job tokens there) and connect with an empty bearer,
// silently stripping the child of its MCP tools - or the Claude Code CLI
// in print mode for claude-* models, with the generated MCP config passed
// explicitly (--mcp-config + --strict-mcp-config). The child runs in its
// own process group so the entire group can be killed at the timeout
// (§8.6). It returns the child's wait error and whether the timeout
// fired. A non-nil third return aborts the run without posting (parent
// context canceled).
func execChild(ctx context.Context, cfg Config, lim limits, jobDir, prompt string, stdout, stderr *syncBuffer) (runErr error, kill killInfo, harnessErr error) {
	env := cfg.Env
	if env == nil {
		env = os.Environ()
	}
	bin, args := childCommand(cfg, jobDir, prompt)
	cmd := exec.Command(bin, args...)
	cmd.Dir = jobDir
	// The generated config references the token via {env:WORKER_TOKEN}
	// (never at rest); guarantee it is present in the child environment.
	cmd.Env = withEnvValue(env, envWorkerToken, cfg.Token)
	for k, v := range cfg.SecretEnv {
		cmd.Env = withEnvValue(cmd.Env, k, v)
	}

	// Any byte on stdout or stderr is a heartbeat. The claude runner's
	// stdout is also parsed live (stream-json) for spend and tool loops;
	// other runners expose no usage data, so only idle + ceiling apply.
	hb := newHeartbeat()
	var stream *claudecli.Stream
	if isClaudeModel(cfg.Model) {
		stream = claudecli.NewStream(cfg.Model)
	}
	cmd.Stdout = &beatWriter{w: stdout, hb: hb, stream: stream}
	cmd.Stderr = &beatWriter{w: stderr, hb: hb}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return err, killInfo{}, nil
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	killGroup := func(sig syscall.Signal) {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, sig)
		}
	}
	finish := func(k killInfo) (error, killInfo, error) {
		if stream != nil {
			k.costUSD, _ = stream.Cost()
		}
		var waitErr error
		exited := false
		if k.reason == wire.ReasonBudget || k.reason == wire.ReasonLoop || k.reason == reasonCancelled {
			// Clean stop: let the CLI flush its result object first.
			killGroup(syscall.SIGTERM)
			grace := cfg.TermGrace
			if grace <= 0 {
				grace = 5 * time.Second
			}
			if k.reason == reasonCancelled {
				grace = cfg.CancelGrace
				if grace <= 0 {
					grace = 10 * time.Second
				}
			}
			select {
			case waitErr = <-done:
				exited = true
			case <-time.After(grace):
			}
		}
		// Always SIGKILL the whole process group once the grace ends, even
		// if the leader already exited: grandchildren that ignore SIGTERM
		// must not survive.
		killGroup(syscall.SIGKILL)
		if !exited {
			waitErr = <-done
		}
		return waitErr, k, nil
	}

	started := time.Now()
	poll := pollInterval(lim)
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return err, killInfo{}, nil
		case <-ctx.Done():
			killGroup(syscall.SIGKILL)
			err := <-done
			return err, killInfo{}, ctx.Err()
		case <-cfg.Cancel: // nil channel never fires
			return finish(killInfo{reason: reasonCancelled})
		case <-tick.C:
		}
		now := time.Now()
		switch {
		case now.Sub(started) >= lim.ceiling:
			return finish(killInfo{reason: wire.ReasonCeiling})
		case now.Sub(hb.last(started)) >= lim.idle:
			return finish(killInfo{reason: wire.ReasonIdle})
		}
		if stream == nil {
			continue
		}
		if cost, ok := stream.Cost(); ok && lim.maxCostUSD > 0 && cost >= lim.maxCostUSD {
			return finish(killInfo{reason: wire.ReasonBudget})
		}
		if n, tool := stream.Repeats(); lim.loopLimit > 0 && n >= lim.loopLimit {
			return finish(killInfo{reason: wire.ReasonLoop, tool: tool, repeats: n})
		}
	}
}

// limits are the run's enforced bounds.
type limits struct {
	ceiling    time.Duration // hard wall clock (timeout_s)
	idle       time.Duration // max silence (idle_timeout_s)
	maxCostUSD float64       // live spend cap; 0 disables
	loopLimit  int           // identical tool calls in a row; <=0 disables
}

// reasonCancelled marks a kill requested via Config.Cancel. It is internal:
// it never reaches a posted result.
const reasonCancelled = "cancelled"

// ErrCancelled is returned by Run when the worker was stopped through
// Config.Cancel. Callers must not report a failure for it.
var ErrCancelled = errors.New("workerharness: worker cancelled")

// killInfo records why the harness terminated the child ("" = it did not).
type killInfo struct {
	reason  string
	costUSD float64
	tool    string
	repeats int
}

// pollInterval is how often limits are checked: fine enough for the
// shortest configured limit, capped at 1s in production.
func pollInterval(l limits) time.Duration {
	d := time.Second
	for _, v := range []time.Duration{l.idle / 10, l.ceiling / 10} {
		if v > 0 && v < d {
			d = v
		}
	}
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

// heartbeat is the time of the child's last output.
type heartbeat struct {
	mu sync.Mutex
	t  time.Time
}

func newHeartbeat() *heartbeat { return &heartbeat{} }

func (h *heartbeat) beat() {
	h.mu.Lock()
	h.t = time.Now()
	h.mu.Unlock()
}

// last returns the last beat, or since when the child never wrote.
func (h *heartbeat) last(since time.Time) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.t.After(since) {
		return h.t
	}
	return since
}

// beatWriter forwards child output to its buffer, counting every write as
// a heartbeat and feeding the live stream parser when present.
type beatWriter struct {
	w      *syncBuffer
	hb     *heartbeat
	stream *claudecli.Stream
}

func (b *beatWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		b.hb.beat()
		if b.stream != nil {
			b.stream.Feed(p)
		}
	}
	return b.w.Write(p)
}

// fallbackResult maps the child's exit to a JobResult (§8.5): done when the
// exit is 0, otherwise failed with error kind crash; the summary is the
// first 240 chars of stdout. The claude runner widens the crash-message
// sources (see crashMessage) because Claude Code reports in-run failures
// on stdout.
func fallbackResult(spec wire.JobSpec, stdout, stderr string, runErr error, claude bool) wire.JobResult {
	res := wire.JobResult{
		JobID:   spec.ID,
		Status:  wire.StatusDone,
		Summary: summaryFrom(stdout),
	}
	if runErr == nil {
		return res
	}
	res.Status = wire.StatusFailed
	msg := crashMessage(stderr, stdout, runErr, claude)
	kind := wire.KindCrash
	if k := wire.ClassifyMessage(msg); k != "" {
		kind = k // deterministic (auth, bad model/route): not a generic crash
	}
	res.Error = &wire.JobError{Kind: kind, Message: msg}
	return res
}

// killResult is the JobResult posted when the harness killed the child
// (§8.6): idle and ceiling are kind=timeout, budget and loop kind=other;
// error.reason carries which limit fired.
func killResult(spec wire.JobSpec, stdout string, k killInfo, lim limits) wire.JobResult {
	e := &wire.JobError{Kind: wire.KindTimeout, Reason: k.reason}
	summary := summaryFrom(stdout)
	switch k.reason {
	case wire.ReasonIdle:
		e.Message = fmt.Sprintf("no output for idle_timeout_s=%d; process group killed", int(lim.idle/time.Second))
	case wire.ReasonCeiling:
		e.Message = fmt.Sprintf("job exceeded timeout_s=%d ceiling; process group killed", int(lim.ceiling/time.Second))
	case wire.ReasonBudget:
		e.Kind = wire.KindOther
		e.Message = fmt.Sprintf("spend cap reached: ~$%.2f >= max_cost_usd=$%.2f; job terminated, partial output kept", k.costUSD, lim.maxCostUSD)
		summary = truncRunes(fmt.Sprintf("Stopped at spend cap ($%.2f of $%.2f). Partial: %s", k.costUSD, lim.maxCostUSD, summary), wire.MaxSummaryChars)
	case wire.ReasonLoop:
		e.Kind = wire.KindOther
		e.Message = fmt.Sprintf("loop guard: tool %q called with identical input %d times in a row; process group killed", k.tool, k.repeats)
	}
	return wire.JobResult{
		JobID: spec.ID, Status: wire.StatusFailed, Summary: summary, Error: e,
		Usage: &wire.Usage{CostUSD: k.costUSD},
	}
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// summaryFrom returns the first 240 chars of stdout, or "(no output)" when
// stdout is empty (§8.5, §11).
func summaryFrom(stdout string) string {
	if stdout == "" {
		return noOutputSummary
	}
	if r := []rune(stdout); len(r) > wire.MaxSummaryChars {
		return string(r[:wire.MaxSummaryChars])
	}
	return stdout
}

// crashMessage prefers stderr over the exit status as the crash explanation
// (§8.5), capped at crashMessageLimit chars. The claude runner extends the
// preference with stdout: Claude Code reports in-run failures - missing
// auth among them - as the result on STDOUT with empty stderr (observed on
// 2.1.325: `Not logged in · Please run /login`, exit 1), so a stdout-only
// failure must still name its cause; the opencode runner keeps the
// stderr-or-exit-status semantics untouched.
func crashMessage(stderr, stdout string, runErr error, claude bool) string {
	msg := strings.TrimSpace(stderr)
	if msg == "" && claude {
		msg = strings.TrimSpace(stdout)
	}
	if msg == "" {
		msg = runErr.Error()
	}
	if r := []rune(msg); len(r) > crashMessageLimit {
		return string(r[:crashMessageLimit])
	}
	return msg
}

// snippet returns the first 200 chars of s for logging.
func snippet(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
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

// syncBuffer is a goroutine-safe byte buffer: exec copies child output on
// its own goroutines while the harness may read at any time.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{} }

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
