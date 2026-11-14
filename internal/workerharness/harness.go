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
	// Model optionally sets the opencode model as "provider/model".
	Model string

	// OpenCODEBin is the opencode binary; "" means "opencode" on PATH.
	OpenCODEBin string
	// Env is the child environment; nil means the worker process
	// environment. WORKER_TOKEN is always overridden.
	Env []string
	// Post delivers the fallback JobResult; nil means HTTP with Bearer
	// auth. Retried at-least-once (§8.5).
	Post PostFunc
	// PostRetryBackoff is the delay between post attempts; zero means 1s.
	PostRetryBackoff time.Duration
	// Timeout overrides the spec's timeout_s. A test seam for sub-second
	// timeouts; zero means timeout_s applies (§8.6).
	Timeout time.Duration
}

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
	if c.Post == nil {
		c.Post = httpPost
	}
	if c.PostRetryBackoff <= 0 {
		c.PostRetryBackoff = defaultPostRetryBackoff
	}
	return nil
}

// Run executes one job (spec §8): it loads the JobSpec, prepares the fresh
// per-job directory with a generated opencode config, runs the opencode
// child non-interactively, and reports the result. The primary report is
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

	jobDir := filepath.Join(cfg.WorkDir, jobsDirName, spec.ID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return fmt.Errorf("job dir: %w", err)
	}
	if err := os.Remove(filepath.Join(jobDir, flagFileName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear report flag: %w", err)
	}
	// The generated config lands at the V2 project-config path in the
	// child's working directory; the child discovers it from cwd (§8.4).
	if err := writeConfig(filepath.Join(jobDir, configFileName), newOpenCODEConfig(cfg)); err != nil {
		return fmt.Errorf("generated config: %w", err)
	}

	stdout, stderr := newSyncBuffer(), newSyncBuffer()
	runErr, timedOut, harnessErr := execChild(ctx, cfg, jobTimeout(cfg, spec), jobDir, buildPrompt(spec), stdout, stderr)
	if harnessErr != nil {
		return harnessErr
	}
	out, errOut := stdout.String(), stderr.String()

	if _, err := os.Stat(filepath.Join(jobDir, flagFileName)); err == nil {
		log.Printf("worker harness: job %s: agent reported via report_result; stdout: %s", spec.ID, snippet(out))
		return nil
	}

	var res wire.JobResult
	if timedOut {
		res = timeoutResult(spec, out)
	} else {
		res = fallbackResult(spec, out, errOut, runErr)
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

// execChild runs `opencode run --standalone <prompt>` with cwd jobDir
// (§8.4): the generated opencode.json there is the V2 project config the
// child discovers from its working directory, and --standalone boots a
// private server inside the child process - the shared background service
// would substitute the config's {env:WORKER_TOKEN} header against its own
// environment (no per-job tokens there) and connect with an empty bearer,
// silently stripping the child of its MCP tools. The child runs in its
// own process group so the entire group can be killed at the timeout
// (§8.6). It returns the child's wait error and whether the timeout
// fired. A non-nil third return aborts the run without posting (parent
// context canceled).
func execChild(ctx context.Context, cfg Config, timeout time.Duration, jobDir, prompt string, stdout, stderr *syncBuffer) (runErr error, timedOut bool, harnessErr error) {
	env := cfg.Env
	if env == nil {
		env = os.Environ()
	}
	cmd := exec.Command(cfg.OpenCODEBin, "run", "--standalone", prompt)
	cmd.Dir = jobDir
	// The generated config references the token via {env:WORKER_TOKEN}
	// (never at rest); guarantee it is present in the child environment.
	cmd.Env = withEnvValue(env, envWorkerToken, cfg.Token)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return err, false, nil
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case err := <-done:
		return err, false, nil
	case <-tctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		err := <-done
		if errors.Is(tctx.Err(), context.DeadlineExceeded) {
			return err, true, nil
		}
		return err, false, tctx.Err()
	}
}

// fallbackResult maps the child's exit to a JobResult (§8.5): done when the
// exit is 0, otherwise failed with error kind crash; the summary is the
// first 240 chars of stdout.
func fallbackResult(spec wire.JobSpec, stdout, stderr string, runErr error) wire.JobResult {
	res := wire.JobResult{
		JobID:   spec.ID,
		Status:  wire.StatusDone,
		Summary: summaryFrom(stdout),
	}
	if runErr == nil {
		return res
	}
	res.Status = wire.StatusFailed
	res.Error = &wire.JobError{Kind: wire.KindCrash, Message: crashMessage(stderr, runErr)}
	return res
}

// timeoutResult is the JobResult posted when the timeout killed the process
// group (§8.6).
func timeoutResult(spec wire.JobSpec, stdout string) wire.JobResult {
	return wire.JobResult{
		JobID:   spec.ID,
		Status:  wire.StatusFailed,
		Summary: summaryFrom(stdout),
		Error: &wire.JobError{
			Kind:    wire.KindTimeout,
			Message: fmt.Sprintf("job exceeded timeout_s=%d; process group killed", spec.TimeoutS),
		},
	}
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
// (§8.5), capped at crashMessageLimit chars.
func crashMessage(stderr string, runErr error) string {
	msg := strings.TrimSpace(stderr)
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
