package workerharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// writeExecutable writes an executable fake-binary script. Tests run in
// parallel and each Run forks a child, so a fork in one test can inherit the
// write fd another test has open on its script; exec'ing that script then
// fails with ETXTBSY ("text file busy"), which Run reports as a crash. Go
// takes syscall.ForkLock exclusively around every fork, so holding the read
// side across create+write+close guarantees no fork can overlap an open
// write fd.
func writeExecutable(path, content string) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(path, []byte(content), 0o755)
}

// fakeOpencode writes a fake `opencode` shell script named "opencode" into a
// fresh temp dir and returns the dir, for PATH injection. The fake never
// invokes a real model.
func fakeOpencode(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := writeExecutable(filepath.Join(dir, "opencode"), script); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakePathEnv returns the worker environment with binDir first on PATH.
func fakePathEnv(t *testing.T, binDir string) []string {
	t.Helper()
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PATH=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// recordingPost captures every fallback result it is asked to deliver.
type recordingPost struct {
	mu      sync.Mutex
	results []wire.JobResult
}

func (p *recordingPost) post(_ context.Context, _, _ string, res wire.JobResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.results = append(p.results, res)
	return nil
}

func (p *recordingPost) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.results)
}

func (p *recordingPost) last() wire.JobResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.results[len(p.results)-1]
}

// writeSpec writes a valid dispatched JobSpec into dir and returns it with
// its file path.
func writeSpec(t *testing.T, dir string, mutate func(*wire.JobSpec)) (wire.JobSpec, string) {
	t.Helper()
	spec := wire.NewJobSpec("Summarize the repo and report back.", "factory")
	if mutate != nil {
		mutate(&spec)
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "job.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return spec, path
}

// testConfig builds a Run config that runs the fake opencode binary in
// binDir (by absolute path — exec.Command resolves the binary with the
// parent's PATH, so PATH injection would hit the real opencode) and records
// fallback posts.
func testConfig(t *testing.T, specPath, binDir string, post PostFunc) Config {
	t.Helper()
	return Config{
		JobPath:          specPath,
		ResultURL:        "http://foreman.invalid/v1/inboxes/foreman:alpha/messages",
		MCPURL:           "http://foreman.invalid/v1/mcp/worker",
		Token:            "tok-abc",
		WorkDir:          t.TempDir(),
		OpenCODEBin:      filepath.Join(binDir, "opencode"),
		Post:             post,
		PostRetryBackoff: time.Millisecond,
	}
}

func TestRunChildEnvironmentAndArgs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec, specPath := writeSpec(t, dir, nil)
	binDir := fakeOpencode(t, `pwd > pwd.txt
cat opencode.json > cfg.txt
printf '%s' "$2" > arg2.txt
printf '%s' "$3" > arg3.txt
printf '%s' "$4" > arg4.txt
printf '%s' "$5" > arg5.txt
printf '%s' "$4" > prompt.txt
printf 'all good'`)
	rec := &recordingPost{}
	cfg := testConfig(t, specPath, binDir, rec.post)

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	jobDir := filepath.Join(cfg.WorkDir, jobsDirName, spec.ID)
	if info, err := os.Stat(jobDir); err != nil || !info.IsDir() {
		t.Errorf("job dir %s missing: %v", jobDir, err)
	}
	// No model pinned: run --standalone -- <prompt>.
	assertArg(t, jobDir, 2, "--standalone")
	assertArg(t, jobDir, 3, "--")
	assertArg(t, jobDir, 4, buildPrompt(spec, RunnerOpencode))
	assertArg(t, jobDir, 5, "")
	// macOS resolves /var to /private/var in getcwd(); compare the resolved form.
	wantPwd, err := filepath.EvalSymlinks(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	// The child finds the generated config by reading opencode.json from
	// its working directory (V2 project-config discovery; no env pointer).
	wantCfg, err := os.ReadFile(filepath.Join(jobDir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"pwd.txt":    wantPwd,
		"cfg.txt":    strings.TrimRight(string(wantCfg), "\n"),
		"prompt.txt": buildPrompt(spec, RunnerOpencode),
	} {
		data, err := os.ReadFile(filepath.Join(jobDir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := strings.TrimRight(string(data), "\n"); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	info, err := os.Stat(filepath.Join(jobDir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("generated config perms = %o, want 0600", perm)
	}

	if rec.calls() != 1 {
		t.Fatalf("fallback posts = %d, want 1", rec.calls())
	}
	res := rec.last()
	if res.Status != wire.StatusDone || res.Error != nil {
		t.Errorf("result = %+v, want done without error", res)
	}
	if res.Summary != "all good" || res.JobID != spec.ID {
		t.Errorf("result = %+v, want job %s summary %q", res, spec.ID, "all good")
	}
}

// assertArg reads the fake opencode's arg<N>.txt (one argument per
// positional file) and compares it with want.
func assertArg(t *testing.T, jobDir string, n int, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(jobDir, fmt.Sprintf("arg%d.txt", n)))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != want {
		t.Errorf("arg%d = %q, want %q", n, got, want)
	}
}

// TestRunPinsModelViaMFlag: the pinned model rides the -m flag - V2
// drops #variant suffixes from the config's model field, so reasoning
// tiers like glm-5.3-flash#max only reach the child via the CLI flag.
func TestRunPinsModelViaMFlag(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec, specPath := writeSpec(t, dir, nil)
	binDir := fakeOpencode(t, `printf '%s' "$2" > arg2.txt
printf '%s' "$3" > arg3.txt
printf '%s' "$4" > arg4.txt
printf '%s' "$5" > arg5.txt
printf '%s' "$6" > arg6.txt
printf 'ok'`)
	rec := &recordingPost{}
	cfg := testConfig(t, specPath, binDir, rec.post)
	cfg.Model = "openrouter/z-ai/glm-5.3-flash#max"

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	jobDir := filepath.Join(cfg.WorkDir, jobsDirName, spec.ID)
	assertArg(t, jobDir, 2, "--standalone")
	assertArg(t, jobDir, 3, "-m")
	assertArg(t, jobDir, 4, "openrouter/z-ai/glm-5.3-flash#max")
	assertArg(t, jobDir, 5, "--")
	assertArg(t, jobDir, 6, buildPrompt(spec, RunnerOpencode))
}

func TestRunExitCodeMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		body  string
		check func(*testing.T, wire.JobResult)
	}{
		{
			"exit 0 maps to done",
			`printf 'finished the task'`,
			func(t *testing.T, res wire.JobResult) {
				if res.Status != wire.StatusDone || res.Error != nil {
					t.Errorf("result = %+v, want done without error", res)
				}
				if res.Summary != "finished the task" {
					t.Errorf("summary = %q", res.Summary)
				}
			},
		},
		{
			"exit 7 maps to failed crash with stderr message",
			`printf 'partial work'; printf 'boom happened' >&2; exit 7`,
			func(t *testing.T, res wire.JobResult) {
				if res.Status != wire.StatusFailed {
					t.Errorf("status = %q, want failed", res.Status)
				}
				if res.Error == nil || res.Error.Kind != wire.KindCrash {
					t.Errorf("error = %+v, want crash kind", res.Error)
				}
				if res.Error != nil && res.Error.Message != "boom happened" {
					t.Errorf("crash message = %q, want stderr", res.Error.Message)
				}
				if res.Summary != "partial work" {
					t.Errorf("summary = %q", res.Summary)
				}
			},
		},
		{
			"exit 1 with no output falls back to exit status",
			`exit 1`,
			func(t *testing.T, res wire.JobResult) {
				if res.Status != wire.StatusFailed || res.Error == nil || res.Error.Kind != wire.KindCrash {
					t.Fatalf("result = %+v, want failed crash", res)
				}
				if res.Error.Message != "exit status 1" {
					t.Errorf("crash message = %q, want exit status", res.Error.Message)
				}
				if res.Summary != noOutputSummary {
					t.Errorf("summary = %q, want %q", res.Summary, noOutputSummary)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			_, specPath := writeSpec(t, dir, nil)
			binDir := fakeOpencode(t, tc.body)
			rec := &recordingPost{}
			cfg := testConfig(t, specPath, binDir, rec.post)

			if err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
			if rec.calls() != 1 {
				t.Fatalf("fallback posts = %d, want 1", rec.calls())
			}
			res := rec.last()
			if err := res.Validate(nil); err != nil {
				t.Errorf("fallback result invalid: %v", err)
			}
			tc.check(t, res)
		})
	}
}

func TestRunSummaryTruncation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		output  string
		wantSum string
	}{
		{"empty stdout", "", noOutputSummary},
		{"short stdout", "hi", "hi"},
		{"exactly 240 chars", strings.Repeat("x", wire.MaxSummaryChars), strings.Repeat("x", wire.MaxSummaryChars)},
		{"300 ascii chars truncate to 240", strings.Repeat("A", 300), strings.Repeat("A", 240)},
		{
			"300 multibyte chars truncate to 240 runes",
			strings.Repeat("é", 300),
			strings.Repeat("é", 240),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			_, specPath := writeSpec(t, dir, nil)
			binDir := fakeOpencode(t, fmt.Sprintf("printf '%s'", tc.output))
			rec := &recordingPost{}
			cfg := testConfig(t, specPath, binDir, rec.post)

			if err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
			if rec.calls() != 1 {
				t.Fatalf("fallback posts = %d, want 1", rec.calls())
			}
			res := rec.last()
			if res.Summary != tc.wantSum {
				t.Errorf("summary = %q, want %q", res.Summary, tc.wantSum)
			}
			if err := res.Validate(nil); err != nil {
				t.Errorf("fallback result invalid: %v", err)
			}
		})
	}
}

func TestRunAgentReportedSkipsFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"flag file and exit 0", `touch .result_posted; printf 'reported via tool'`},
		{"flag file and crash exit", `touch .result_posted; printf 'agent bug' >&2; exit 9`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			_, specPath := writeSpec(t, dir, nil)
			binDir := fakeOpencode(t, tc.body)
			rec := &recordingPost{}
			cfg := testConfig(t, specPath, binDir, rec.post)

			if err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
			if rec.calls() != 0 {
				t.Errorf("fallback posts = %d, want 0: %+v", rec.calls(), rec.results)
			}
		})
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec, specPath := writeSpec(t, dir, nil)
	binDir := fakeOpencode(t, `echo $$ > pid.txt
printf 'partial stdout'
sleep 30
exit 0`)
	rec := &recordingPost{}
	cfg := testConfig(t, specPath, binDir, rec.post)
	// Generous budget: parallel test load can delay shell startup by
	// 100ms+; the fake writes its output within the first moments either
	// way, and the kill semantics are what is under test.
	cfg.Timeout = 2 * time.Second

	start := time.Now()
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run() took %v, want it to return at the 2s timeout", elapsed)
	}

	if rec.calls() != 1 {
		t.Fatalf("fallback posts = %d, want 1", rec.calls())
	}
	res := rec.last()
	if res.Status != wire.StatusFailed || res.Error == nil || res.Error.Kind != wire.KindTimeout {
		t.Errorf("result = %+v, want failed with timeout kind", res)
	}
	if res.Summary != "partial stdout" {
		t.Errorf("summary = %q, want partial stdout captured before the kill", res.Summary)
	}
	if err := res.Validate(nil); err != nil {
		t.Errorf("timeout result invalid: %v", err)
	}

	// The whole process group must be dead: poll until signaling the group
	// with 0 fails with ESRCH.
	pidData, err := os.ReadFile(filepath.Join(cfg.WorkDir, jobsDirName, spec.ID, "pid.txt"))
	if err != nil {
		t.Fatalf("fake opencode never wrote its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("pid %q: %v", pidData, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(-pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still alive after timeout kill", pid)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestRunPostRetryAtLeastOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, specPath := writeSpec(t, dir, nil)
	binDir := fakeOpencode(t, `printf 'done fast'`)

	cases := []struct {
		name      string
		fails     int
		wantErr   bool
		wantCalls int
	}{
		{"flaky inbox then success", 2, false, 3},
		{"inbox down", 99, true, maxPostAttempts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			calls := 0
			cfg := testConfig(t, specPath, binDir, func(context.Context, string, string, wire.JobResult) error {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls > tc.fails {
					return nil
				}
				return errors.New("connection refused")
			})
			err := Run(context.Background(), cfg)
			if (err != nil) != tc.wantErr {
				t.Errorf("Run() = %v, wantErr %v", err, tc.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if calls != tc.wantCalls {
				t.Errorf("post attempts = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// TestRunEndToEndFallbackPost drives a whole run with the default HTTP
// post: fake opencode crashes, and the fallback lands on a fake inbox
// endpoint with Bearer auth and a valid job.result envelope.
func TestRunEndToEndFallbackPost(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec, specPath := writeSpec(t, dir, nil)
	binDir := fakeOpencode(t, `printf 'halfway there'; exit 4`)

	var auth string
	var env wire.MessageEnvelope
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &env); err != nil {
			t.Errorf("request body is not an envelope: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := testConfig(t, specPath, binDir, nil) // nil Post: the default HTTP post
	cfg.ResultURL = srv.URL
	cfg.Token = "tok-endtoend"

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if auth != "Bearer tok-endtoend" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer tok-endtoend")
	}
	if env.Type != resultEnvelopeType || env.JobID != spec.ID || env.From != senderFrom(spec.ID) {
		t.Errorf("envelope = %s/%s from %s, want job.result/%s from %s",
			env.Type, env.JobID, env.From, resultEnvelopeType, spec.ID)
	}
	var res wire.JobResult
	if err := json.Unmarshal(env.Payload, &res); err != nil {
		t.Fatalf("payload is not a JobResult: %v", err)
	}
	if res.Status != wire.StatusFailed || res.Summary != "halfway there" {
		t.Errorf("result = %+v, want failed with the captured stdout", res)
	}
	if res.Error == nil || res.Error.Kind != wire.KindCrash || res.Error.Message != "exit status 4" {
		t.Errorf("error = %+v, want crash exit status 4", res.Error)
	}
}

func TestRunBadSpecDoesNotExecChild(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		spec string
	}{
		{"missing file", ""},
		{"invalid json", `{"task": nope`},
		{"spec without id", `{"task":"t"}`},
		{"spec with bad id prefix", `{"id":"msg_01","task":"t"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			specPath := filepath.Join(dir, "job.json")
			if tc.spec != "" {
				if err := os.WriteFile(specPath, []byte(tc.spec), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The fake fails the test if the harness reaches it.
			binDir := fakeOpencode(t, `printf 'should not run' >&2; exit 42`)
			rec := &recordingPost{}
			cfg := testConfig(t, specPath, binDir, rec.post)

			if err := Run(context.Background(), cfg); err == nil {
				t.Error("Run() = nil, want error")
			}
			if rec.calls() != 0 {
				t.Errorf("fallback posts = %d, want 0", rec.calls())
			}
		})
	}
}

func TestRunStartFailurePostsCrashResult(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec, specPath := writeSpec(t, dir, nil)
	rec := &recordingPost{}
	cfg := testConfig(t, specPath, t.TempDir(), rec.post)
	cfg.OpenCODEBin = filepath.Join(t.TempDir(), "opencode-not-on-path")
	if _, err := exec.LookPath(cfg.OpenCODEBin); err == nil {
		t.Fatal("test binary unexpectedly resolves")
	}

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() = %v, want nil (crash result posted)", err)
	}
	if rec.calls() != 1 {
		t.Fatalf("fallback posts = %d, want 1", rec.calls())
	}
	res := rec.last()
	if res.JobID != spec.ID || res.Status != wire.StatusFailed {
		t.Errorf("result = %+v, want failed for %s", res, spec.ID)
	}
	if res.Error == nil || res.Error.Kind != wire.KindCrash {
		t.Fatalf("error = %+v, want crash kind", res.Error)
	}
	if !strings.Contains(res.Error.Message, "opencode-not-on-path") {
		t.Errorf("crash message = %q, want the start failure", res.Error.Message)
	}
	if err := res.Validate(nil); err != nil {
		t.Errorf("crash result invalid: %v", err)
	}
}

// fakeClaude writes a fake `claude` shell script into a fresh temp dir and
// returns the dir, for PATH-style injection. The fake never invokes a real
// model.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := writeExecutable(filepath.Join(dir, "claude"), script); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testClaudeConfig builds a Run config routed to the claude runner: both
// fake binaries are set by absolute path (exec.Command resolves with the
// parent's PATH, so a routing bug can never silently exec the real tools).
func testClaudeConfig(t *testing.T, specPath, binDir string, post PostFunc) Config {
	t.Helper()
	cfg := testConfig(t, specPath, binDir, post)
	cfg.ClaudeBin = filepath.Join(binDir, "claude")
	return cfg
}

// TestRunnerForRoutingDecision pins the routing decision (spec §8.4):
// bare claude-* ids (and the bare `claude` alias, and the anthropic/
// provider-prefixed shape) route to the Claude Code CLI; GLM models,
// empty models, and OpenRouter-hosted claude models (a different
// credential path - the OpenRouter key, not the Anthropic CLI's own
// auth) stay on opencode.
func TestRunnerForRoutingDecision(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  string
	}{
		{"claude-sonnet-4-5", RunnerClaude},
		{"claude-opus-4-6", RunnerClaude},
		{"claude-haiku-4-5", RunnerClaude},
		{"claude", RunnerClaude},
		{"anthropic/claude-sonnet-4-5", RunnerClaude},
		{"", RunnerOpencode},
		{"openrouter/z-ai/glm-5.3", RunnerOpencode},
		{"openrouter/z-ai/glm-5.3-flash", RunnerOpencode},
		{"openrouter/z-ai/glm-5.3-flash#max", RunnerOpencode},
		{"openrouter/anthropic/claude-sonnet-4-5", RunnerOpencode},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			if got := RunnerFor(tc.model); got != tc.want {
				t.Errorf("RunnerFor(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

// TestRunClaudeRoutingAndArgs: a claude-* model routes the run to the
// Claude Code CLI - the fake opencode must never run - with the SAME
// per-job cwd, prompt, token-env, flag-file, and fallback-result contract
// as the opencode runner (spec §8.3, §8.4, §8.5). Args verified against
// the installed CLI (2.1.325).
func TestRunClaudeRoutingAndArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		model     string
		wantModel string // the --model value (anthropic/ stripped)
	}{
		{"bare claude id", "claude-sonnet-4-5", "claude-sonnet-4-5"},
		{"anthropic-prefixed id strips the provider", "anthropic/claude-opus-4-6", "claude-opus-4-6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			spec, specPath := writeSpec(t, dir, nil)
			claudeDir := fakeClaude(t, `n=1
for a in "$@"; do
	printf '%s' "$a" > "arg$n.txt"
	n=$((n+1))
done
pwd > pwd.txt
printf '%s' "$WORKER_TOKEN" > token.txt
cat claude-mcp.json > mcp.txt
printf 'claude ran'`)
			rec := &recordingPost{}
			// OpenCODEBin points at a nonexistent path: if the routing
			// decision ever went to opencode on a claude-* model, the run
			// fails loudly instead of silently exercising the wrong CLI.
			cfg := testConfig(t, specPath, t.TempDir(), rec.post)
			cfg.ClaudeBin = filepath.Join(claudeDir, "claude")
			cfg.Model = tc.model

			if err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
			jobDir := filepath.Join(cfg.WorkDir, jobsDirName, spec.ID)
			wantPwd, err := filepath.EvalSymlinks(jobDir)
			if err != nil {
				t.Fatal(err)
			}
			// The routed prompt uses the claude tool spelling (§8.3).
			wantPromptText := buildPrompt(spec, RunnerClaude)
			for name, want := range map[string]string{
				"arg1": "-p",
				"arg2": "--output-format",
				"arg3": "stream-json",
				"arg4": "--verbose",
				"arg5": "--model",
				"arg6": tc.wantModel,
				"arg7": "--mcp-config",
				// The exec arg is the unresolved per-job join (cmd.Dir
				// handles the cwd; macOS resolves /var in getcwd only).
				"arg8":  filepath.Join(jobDir, claudeConfigFileName),
				"arg9":  "--strict-mcp-config",
				"arg10": "--permission-mode",
				"arg11": "bypassPermissions",
				"arg12": "--no-session-persistence",
				"arg13": wantPromptText,
				"pwd":   wantPwd,
				"token": "tok-abc",
			} {
				data, err := os.ReadFile(filepath.Join(jobDir, name+".txt"))
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if got := strings.TrimRight(string(data), "\n"); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}

			// The generated MCP config rides --mcp-config: verified shape,
			// 0600, token only via ${WORKER_TOKEN} expansion, and NO
			// opencode.json (the opencode runner's file is dead weight
			// on a claude run).
			info, err := os.Stat(filepath.Join(jobDir, claudeConfigFileName))
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("generated claude config perms = %o, want 0600", perm)
			}
			mcpData, err := os.ReadFile(filepath.Join(jobDir, claudeConfigFileName))
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				MCPServers map[string]struct {
					Type    string            `json:"type"`
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(mcpData, &got); err != nil {
				t.Fatalf("generated claude config is not the mcpServers shape: %v\n%s", err, mcpData)
			}
			srv, ok := got.MCPServers["inbox"]
			if !ok {
				t.Fatalf("generated claude config has no inbox server:\n%s", mcpData)
			}
			if srv.Type != "http" || srv.URL != cfg.MCPURL {
				t.Errorf("inbox server = %+v, want http %s", srv, cfg.MCPURL)
			}
			if srv.Headers["Authorization"] != "Bearer ${WORKER_TOKEN}" {
				t.Errorf("Authorization header = %q, want the ${WORKER_TOKEN} reference", srv.Headers["Authorization"])
			}
			if containsSecret(t, mcpData, cfg.Token) {
				t.Errorf("generated claude config leaks the worker token at rest:\n%s", mcpData)
			}
			if _, err := os.Stat(filepath.Join(jobDir, configFileName)); !os.IsNotExist(err) {
				t.Errorf("opencode.json exists on a claude run (err = %v), want absent", err)
			}

			// Same fallback-result contract (§8.5).
			if rec.calls() != 1 {
				t.Fatalf("fallback posts = %d, want 1", rec.calls())
			}
			res := rec.last()
			if res.Status != wire.StatusDone || res.Error != nil || res.Summary != "claude ran" || res.JobID != spec.ID {
				t.Errorf("result = %+v, want done %s summary %q", res, spec.ID, "claude ran")
			}
		})
	}
}

// TestRunOpencodeRoutingUnchanged: the GLM tiers keep running opencode -
// the generated config stays the opencode.json project config, and no
// claude config appears (spec §8.4). ClaudeBin points at a nonexistent
// path: if the routing decision ever went to claude on a GLM model, the
// run fails loudly instead of silently exercising the wrong CLI.
func TestRunOpencodeRoutingUnchanged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec, specPath := writeSpec(t, dir, nil)
	binDir := fakeOpencode(t, `printf 'opencode ran'`)
	rec := &recordingPost{}
	cfg := testConfig(t, specPath, binDir, rec.post)
	cfg.ClaudeBin = filepath.Join(t.TempDir(), "claude-not-installed")
	cfg.Model = "openrouter/z-ai/glm-5.3-flash"

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	jobDir := filepath.Join(cfg.WorkDir, jobsDirName, spec.ID)
	if _, err := os.Stat(filepath.Join(jobDir, configFileName)); err != nil {
		t.Errorf("opencode.json missing on an opencode run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jobDir, claudeConfigFileName)); !os.IsNotExist(err) {
		t.Errorf("claude-mcp.json exists on an opencode run (err = %v), want absent", err)
	}
	if rec.calls() != 1 {
		t.Fatalf("fallback posts = %d, want 1", rec.calls())
	}
	if res := rec.last(); res.Status != wire.StatusDone || res.Summary != "opencode ran" {
		t.Errorf("result = %+v, want done summary %q", res, "opencode ran")
	}
}

// TestRunClaudeAuthFailureIsLoud: a claude run that fails inside the run -
// missing auth among them - must surface as a failed job, never a silent
// success. Claude Code prints in-run failures as the result on STDOUT
// with empty stderr (observed on 2.1.325: `Not logged in · Please run
// /login`, exit 1), so the crash message falls back to stdout there
// (spec §8.4).
func TestRunClaudeAuthFailureIsLoud(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantMsg string
		wantSum string
	}{
		{
			"auth failure printed on stdout (observed live shape)",
			`printf 'Not logged in · Please run /login'; exit 1`,
			"Not logged in · Please run /login",
			"Not logged in · Please run /login",
		},
		{
			"auth failure on stderr still preferred",
			`printf 'Invalid API key · Please run /login'; printf 'bad key' >&2; exit 1`,
			"bad key",
			"Invalid API key · Please run /login",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			_, specPath := writeSpec(t, dir, nil)
			binDir := fakeClaude(t, tc.body)
			rec := &recordingPost{}
			cfg := testClaudeConfig(t, specPath, binDir, rec.post)
			cfg.Model = "claude-sonnet-4-5"

			if err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run() = %v, want nil (failed result posted)", err)
			}
			if rec.calls() != 1 {
				t.Fatalf("fallback posts = %d, want 1", rec.calls())
			}
			res := rec.last()
			if res.Status != wire.StatusFailed || res.Error == nil || res.Error.Kind != wire.KindCrash {
				t.Fatalf("result = %+v, want failed crash", res)
			}
			if res.Error.Message != tc.wantMsg {
				t.Errorf("crash message = %q, want %q", res.Error.Message, tc.wantMsg)
			}
			if res.Summary != tc.wantSum {
				t.Errorf("summary = %q, want %q", res.Summary, tc.wantSum)
			}
			if err := res.Validate(nil); err != nil {
				t.Errorf("crash result invalid: %v", err)
			}
		})
	}
}

// TestRunClaudeMissingCLILoudFailure: a claude-routed job whose CLI is
// missing fails loudly - a failed crash result is posted (routed to the
// factory by the spawner's §8.5 spawn-failure surfacing), never a silent
// fake success (spec §8.4).
func TestRunClaudeMissingCLILoudFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec, specPath := writeSpec(t, dir, nil)
	binDir := fakeOpencode(t, `printf 'should not run' >&2; exit 42`)
	rec := &recordingPost{}
	cfg := testClaudeConfig(t, specPath, binDir, rec.post)
	cfg.Model = "claude-sonnet-4-5"
	cfg.ClaudeBin = filepath.Join(t.TempDir(), "claude-not-installed")
	if _, err := exec.LookPath(cfg.ClaudeBin); err == nil {
		t.Fatal("test binary unexpectedly resolves")
	}

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() = %v, want nil (crash result posted)", err)
	}
	if rec.calls() != 1 {
		t.Fatalf("fallback posts = %d, want 1", rec.calls())
	}
	res := rec.last()
	if res.JobID != spec.ID || res.Status != wire.StatusFailed {
		t.Errorf("result = %+v, want failed for %s", res, spec.ID)
	}
	if res.Error == nil || res.Error.Kind != wire.KindCrash {
		t.Fatalf("error = %+v, want crash kind", res.Error)
	}
	if !strings.Contains(res.Error.Message, "claude-not-installed") {
		t.Errorf("crash message = %q, want the missing-CLI start failure", res.Error.Message)
	}
	if err := res.Validate(nil); err != nil {
		t.Errorf("crash result invalid: %v", err)
	}
}

// TestRunClaudeRecordsUsage: the harness parses the CLI's JSON result,
// ALWAYS posts the usage event (even when the agent reported via the flag
// file), and uses the result text for the fallback summary.
func TestRunClaudeRecordsUsage(t *testing.T) {
	t.Parallel()
	for _, agentReported := range []bool{false, true} {
		dir := t.TempDir()
		spec, specPath := writeSpec(t, dir, nil)
		body := `printf '%s' '{"type":"result","is_error":false,"result":"hello summary","session_id":"s9","total_cost_usd":0.25,"usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":11,"cache_read_input_tokens":13}}'`
		if agentReported {
			body = "touch .result_posted\n" + body
		}
		claudeDir := fakeClaude(t, body)
		rec := &recordingPost{}
		cfg := testConfig(t, specPath, t.TempDir(), rec.post)
		cfg.ClaudeBin = filepath.Join(claudeDir, "claude")
		cfg.Model = "claude-sonnet-4-5"
		var got []wire.UsageEvent
		cfg.PostUsage = func(_ context.Context, _, _ string, ev wire.UsageEvent) error {
			got = append(got, ev)
			return nil
		}
		if err := Run(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].JobID != spec.ID || got[0].Kind != wire.UsageKindWorker ||
			got[0].Input != 7 || got[0].Output != 3 || got[0].CacheCreate != 11 || got[0].CacheRead != 13 ||
			got[0].CostUSD != 0.25 || got[0].SessionID != "s9" {
			t.Fatalf("agentReported=%v usage events = %+v", agentReported, got)
		}
		if !agentReported {
			if len(rec.results) != 1 || rec.results[0].Summary != "hello summary" {
				t.Fatalf("fallback results = %+v", rec.results)
			}
		}
	}
}

func TestRunArgsStandaloneAndTerminator(t *testing.T) {
	got := runArgs(Config{Model: "m/x#max"}, "-weird prompt", true)
	want := []string{"run", "--standalone", "-m", "m/x#max", "--", "-weird prompt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	got = runArgs(Config{}, "", false)
	want = []string{"run", "--", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHelpSupportsStandalone(t *testing.T) {
	old := "opencode run [message..]\n\nOptions:\n  -m, --model\n"
	cases := []struct {
		name string
		help string
		err  error
		want bool
	}{
		{"v2", "USAGE\n  opencode run [flags]\n  --standalone  x\n", nil, true},
		{"old yargs", old, nil, false},
		{"old yargs nonzero", old, errors.New("exit 1"), false},
		{"unrecognized", "something else", nil, true},
		{"probe failed", "", errors.New("boom"), true},
	}
	for _, c := range cases {
		if got := helpSupportsStandalone(c.help, c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestCrashMessageKeepsTrailingUsageError(t *testing.T) {
	usage := "opencode run [message..]\n\nPositionals:\n  message  message to send  [array] [default: []]\n\nOptions:\n" +
		strings.Repeat("  -h, --help  Show help\n", 80) + "\nUnknown argument: standalone\n"
	msg := crashMessage(usage, "", errors.New("exit status 1"), false)
	if !strings.Contains(msg, "Unknown argument: standalone") {
		t.Fatalf("rejection line lost: %q", msg)
	}
	if !strings.HasPrefix(msg, "opencode run") {
		t.Fatalf("head lost: %q", msg)
	}
	if n := len([]rune(msg)); n > crashMessageLimit {
		t.Fatalf("len %d > limit", n)
	}
	if short := crashMessage("boom", "", errors.New("x"), false); short != "boom" {
		t.Fatalf("short message altered: %q", short)
	}
}
