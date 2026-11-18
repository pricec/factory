// Claude CLI auth health and the operator re-login flow (spec §13.3).
package factoryapp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/claudeauth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

const testToken = "sk-ant-oat01-TESTTOKENabcdefghijklmnopqrstuvwxyz0123456789"

var claudeNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// fakeProbe answers Probe with a fixed verdict and records the tokens.
type fakeProbe struct {
	mu     sync.Mutex
	state  string
	tokens []string
}

func (f *fakeProbe) run(_ context.Context, _ string, _, env []string, _ string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, claudeauth.TokenEnv+"="); ok {
			f.tokens = append(f.tokens, v)
		}
	}
	switch f.state {
	case claudeauth.StateOK:
		return `{"type":"result","is_error":false,"result":"OK"}`, "", nil
	case claudeauth.StateFailed:
		return `{"type":"result","is_error":true,"api_error_status":401,"result":"Failed to authenticate. API Error: 401 OAuth access token is invalid."}`, "", nil
	}
	return "", "network unreachable", errors.New("exit status 1")
}

// fakeSetup is a setup-token session that mints testToken for "good-code".
type fakeSetup struct {
	mu        sync.Mutex
	submitted []string
	closed    bool
}

func (s *fakeSetup) Submit(_ context.Context, code string, _ time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submitted = append(s.submitted, code)
	if code == "good-code-0123456789#state" {
		return testToken, nil
	}
	return "", errors.New("OAuth error: That code wasn't accepted.")
}
func (s *fakeSetup) Expired(time.Time) bool { return false }
func (s *fakeSetup) Close()                 { s.mu.Lock(); s.closed = true; s.mu.Unlock() }

type claudeFixture struct {
	a      *App
	clk    *gateClock
	probe  *fakeProbe
	setups []*fakeSetup
}

func newClaudeFixture(t *testing.T) *claudeFixture {
	t.Helper()
	f := &claudeFixture{clk: &gateClock{t: claudeNow}, probe: &fakeProbe{state: claudeauth.StateOK}}
	f.a = newGateApp(t, t.TempDir(), f.clk)
	t.Cleanup(func() { f.a.Close() })
	f.a.claude.async = func(fn func()) { fn() }
	f.a.claude.prober = claudeauth.Prober{Run: f.probe.run}
	f.a.claude.startSetup = func(context.Context) (setupHandle, string, error) {
		s := &fakeSetup{}
		f.setups = append(f.setups, s)
		return s, "https://claude.com/cai/oauth/authorize?code=true&state=abc", nil
	}
	ctx := context.Background()
	for _, id := range []string{"foreman:assistant", "foreman:router"} {
		if err := f.a.jobs.UpsertForeman(ctx, id, json.RawMessage(`{"os":"linux"}`)); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// report pushes a claude_auth report as foreman id would.
func (f *claudeFixture) report(t *testing.T, id, state string, checked time.Time) claudeauth.Directive {
	t.Helper()
	raw, _ := json.Marshal(claudeauth.Report{State: state, CheckedAt: checked.UTC().Format(time.RFC3339), ClaudeTiers: []string{"hardest"}})
	out := f.a.claudeCapsExt(context.Background(), id, raw)
	d, _ := out["claude_auth"].(claudeauth.Directive)
	return d
}

// pinnedRunning creates a job pinned to foreman and drives it to running.
func (f *claudeFixture) pinnedRunning(t *testing.T, id, foreman, tier string) {
	t.Helper()
	ctx := context.Background()
	spec := wire.JobSpec{ID: id, Task: "work", Priority: 5, Foreman: foreman, Tier: tier}
	spec.Normalize()
	if err := f.a.jobs.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := f.a.jobs.Dispatch(ctx, id, foreman); err != nil {
		t.Fatal(err)
	}
	if err := f.a.jobs.ApplyEvent(ctx, id, "started"); err != nil {
		t.Fatal(err)
	}
}

func notLoggedIn(id string) wire.JobResult {
	return wire.JobResult{
		JobID: id, Status: wire.StatusFailed, Summary: "(no output)",
		Error: &wire.JobError{Kind: wire.KindAuth, Message: "Not logged in · Please run /login"}, // the harness kind since #49
	}
}

func (f *claudeFixture) thread(t *testing.T, taskID string) string {
	t.Helper()
	code, out := doJSON(t, f.a, "GET", "/v1/tasks/"+taskID+"?internal=1", nil)
	if code != 200 {
		t.Fatalf("get task %s: %d", taskID, code)
	}
	b, _ := json.Marshal(out["messages"])
	return string(b)
}

func (f *claudeFixture) authTask(t *testing.T) tasks.Task {
	t.Helper()
	f.a.claude.mu.Lock()
	id := f.a.claudeBookLocked(context.Background()).TaskID
	f.a.claude.mu.Unlock()
	if id == "" {
		t.Fatal("no claude auth task open")
	}
	tk, err := f.a.tasks.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// TestClaudeAuthEndToEnd: detection -> hold -> operator task -> token
// pasted -> stored -> foremen report ok -> held jobs released -> task
// closed, with the token never in a thread.
func TestClaudeAuthEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newClaudeFixture(t)
	a := f.a
	f.report(t, "foreman:assistant", claudeauth.StateOK, claudeNow.Add(-time.Hour))

	f.pinnedRunning(t, "job_a", "foreman:assistant", "hardest")
	f.clk.Add(time.Minute)
	sendResult(t, a, notLoggedIn("job_a"))

	j := mustJob(t, a, "job_a")
	if j.State != jobs.StateHeld || !strings.HasPrefix(j.HeldReason, claudeHoldPrefix) || j.Attempts != 0 {
		t.Fatalf("job after auth failure: state=%s reason=%q attempts=%d", j.State, j.HeldReason, j.Attempts)
	}
	tk := f.authTask(t)
	if !tk.NeedsOperator || tk.Priority != claudeAuthTaskPriority || !strings.Contains(tk.Title, "foreman:assistant") {
		t.Fatalf("auth task: %+v", tk)
	}
	th := f.thread(t, tk.ID)
	if !strings.Contains(th, "https://claude.com/cai/oauth/authorize") || !strings.Contains(th, "foreman:assistant") {
		t.Fatalf("task thread lacks the login link: %s", th)
	}

	// A second job pinned there is admitted held, not dispatched.
	code, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "x", "foreman": "foreman:assistant", "tier": "hardest"})
	if code != 201 || out["state"] != jobs.StateHeld || !strings.HasPrefix(out["held_reason"].(string), claudeHoldPrefix) {
		t.Fatalf("create on failed foreman: %d %v", code, out)
	}
	// A non-claude tier on the same foreman is unaffected.
	if hold, _ := a.claudeHold(ctx, wire.JobSpec{Foreman: "foreman:assistant", Tier: "flash"}, false); hold {
		t.Fatal("flash tier (not claude there) must not be held")
	}
	// The other foreman also fails: same task, no second link.
	f.pinnedRunning(t, "job_r", "foreman:router", "hardest")
	sendResult(t, a, notLoggedIn("job_r"))
	if got := f.authTask(t).ID; got != tk.ID {
		t.Fatalf("second foreman opened a new task %s (want %s)", got, tk.ID)
	}
	if len(f.setups) != 1 {
		t.Fatalf("setup sessions started: %d, want 1", len(f.setups))
	}

	// A stale ok (probed before the failure) cannot clear it, and the
	// directive keeps asking for a probe.
	d := f.report(t, "foreman:assistant", claudeauth.StateOK, claudeNow.Add(-time.Hour))
	if !d.Probe || !a.claudeBlocks(ctx, wire.JobSpec{Tier: "hardest"}, "foreman:assistant") {
		t.Fatalf("stale ok cleared the failure (directive %+v)", d)
	}
	if d.TokenRev != "" {
		t.Fatalf("token rev with no token stored: %q", d.TokenRev)
	}

	// The operator pastes the token into the thread.
	code, out = doJSON(t, a, "POST", "/v1/chat", map[string]any{"task_id": tk.ID, "text": "here you go " + testToken})
	if code != 201 || out["type"] != tasks.TypeBrainReply {
		t.Fatalf("token reply: %d %v", code, out)
	}
	if v, meta, err := a.secrets.Get(claudeauth.SecretName); err != nil || v != testToken || meta.EnvVar != claudeauth.TokenEnv {
		t.Fatalf("secret not stored: %v %+v", err, meta)
	}
	if len(f.probe.tokens) != 1 || f.probe.tokens[0] != testToken {
		t.Fatalf("token not verified before storing: %v", f.probe.tokens)
	}
	if th := f.thread(t, tk.ID); strings.Contains(th, testToken) || strings.Contains(th, "TESTTOKEN") {
		t.Fatalf("token leaked into the thread: %s", th)
	}

	// Foremen now see a token revision; the token endpoint serves it.
	d = f.report(t, "foreman:assistant", claudeauth.StateFailed, f.clk.Now())
	if d.TokenRev == "" || !d.Probe {
		t.Fatalf("directive after store: %+v", d)
	}
	v, _ := a.claudeToken()
	if v != testToken {
		t.Fatalf("claudeToken: %q", v)
	}
	if env := a.claudeEnv(); env[claudeauth.TokenEnv] != testToken {
		t.Fatalf("planner env lacks the token")
	}

	// Both foremen probe OK with the token: jobs release, task closes.
	f.clk.Add(time.Minute)
	f.report(t, "foreman:assistant", claudeauth.StateOK, f.clk.Now())
	if j := mustJob(t, a, "job_a"); j.State == jobs.StateHeld {
		t.Fatalf("job_a still held after its foreman recovered: %q", j.HeldReason)
	}
	if f.authTask(t).Status != tasks.StatusOpen {
		t.Fatal("task closed while foreman:router is still failed")
	}
	f.report(t, "foreman:router", claudeauth.StateOK, f.clk.Now())
	if j := mustJob(t, a, "job_r"); j.State == jobs.StateHeld {
		t.Fatalf("job_r still held: %q", j.HeldReason)
	}
	got, err := a.tasks.GetTask(ctx, tk.ID)
	if err != nil || got.Status != tasks.StatusClosed || got.CloseReason != "claude-auth-restored" {
		t.Fatalf("task after recovery: %+v %v", got, err)
	}
	th = f.thread(t, tk.ID)
	for _, want := range []string{"foreman:assistant: Claude auth check passed", "foreman:router: Claude auth check passed", "No foreman is failing"} {
		if !strings.Contains(th, want) {
			t.Fatalf("thread lacks %q: %s", want, th)
		}
	}
	if strings.Contains(th, testToken) {
		t.Fatal("token leaked into the thread")
	}
	if !f.setups[0].closed {
		t.Fatal("setup session not closed when the episode ended")
	}
}

// TestClaudeAuthCodeFlow: the operator pastes the browser code; the setup
// session redeems it; a bad code yields a fresh link; a rejected token is
// not stored.
func TestClaudeAuthCodeFlow(t *testing.T) {
	f := newClaudeFixture(t)
	a := f.a
	f.pinnedRunning(t, "job_a", "foreman:assistant", "hardest")
	sendResult(t, a, notLoggedIn("job_a"))
	tk := f.authTask(t)

	// A bad code: rejected, a new session (link) starts.
	code, _ := doJSON(t, a, "POST", "/v1/chat", map[string]any{"task_id": tk.ID, "text": "bad-code-0123456789#state"})
	if code != 201 || len(f.setups) != 2 || len(f.setups[0].submitted) != 1 {
		t.Fatalf("bad code: %d setups=%d", code, len(f.setups))
	}
	if _, _, err := a.secrets.Get(claudeauth.SecretName); err == nil {
		t.Fatal("secret stored after a bad code")
	}
	// Ordinary chat on the task still reaches the planner (not consumed).
	if _, _, ok := a.interceptClaudeChat(context.Background(), "what is this about?", tk.ID); ok {
		t.Fatal("plain chat was consumed")
	}
	// The good code mints and stores the token.
	f.probe.state = claudeauth.StateOK
	if code, _ := doJSON(t, a, "POST", "/v1/chat", map[string]any{"task_id": tk.ID, "text": " good-code-0123456789#state "}); code != 201 {
		t.Fatalf("good code: %d", code)
	}
	if v, _, err := a.secrets.Get(claudeauth.SecretName); err != nil || v != testToken {
		t.Fatalf("token not stored from code: %v", err)
	}
	th := f.thread(t, tk.ID)
	if strings.Contains(th, "good-code-0123456789") || strings.Contains(th, testToken) {
		t.Fatalf("code or token leaked into the thread: %s", th)
	}

	// A token Anthropic rejects is never stored.
	g := newClaudeFixture(t)
	g.probe.state = claudeauth.StateFailed
	code, out := doJSON(t, g.a, "POST", "/v1/chat", map[string]any{"text": testToken})
	if code != 201 {
		t.Fatalf("token chat: %d %v", code, out)
	}
	if _, _, err := g.a.secrets.Get(claudeauth.SecretName); err == nil {
		t.Fatal("rejected token was stored")
	}
	if th := g.thread(t, out["task_id"].(string)); !strings.Contains(th, "rejected") || strings.Contains(th, testToken) {
		t.Fatalf("rejection thread: %s", th)
	}
}

// TestClaudeAuthIgnoresOtherFailures: non-claude runs and other tools'
// logins never trip the gate.
func TestClaudeAuthIgnoresOtherFailures(t *testing.T) {
	f := newClaudeFixture(t)
	a := f.a
	f.report(t, "foreman:assistant", claudeauth.StateOK, claudeNow)
	// flash is not a claude tier on foreman:assistant.
	f.pinnedRunning(t, "job_f", "foreman:assistant", "flash")
	sendResult(t, a, notLoggedIn("job_f"))
	if j := mustJob(t, a, "job_f"); j.State == jobs.StateHeld {
		t.Fatal("non-claude job was held")
	}
	f.pinnedRunning(t, "job_g", "foreman:assistant", "hardest")
	sendResult(t, a, wire.JobResult{
		JobID: "job_g", Status: wire.StatusFailed, Summary: "x",
		Error: &wire.JobError{Kind: wire.KindOther, Message: "gh: You are not logged into any GitHub hosts. Run gh auth login"},
	})
	if j := mustJob(t, a, "job_g"); j.State == jobs.StateHeld {
		t.Fatal("gh login failure tripped the claude gate")
	}
	a.claude.mu.Lock()
	tid := a.claudeBookLocked(context.Background()).TaskID
	a.claude.mu.Unlock()
	if tid != "" {
		t.Fatal("an auth task opened for non-claude failures")
	}
}

// TestClaudeAuthProactiveReport: a foreman's own probe failure (no job
// involved) opens the task; the pending picker routes around it.
func TestClaudeAuthProactiveReport(t *testing.T) {
	ctx := context.Background()
	f := newClaudeFixture(t)
	a := f.a
	f.report(t, "foreman:router", claudeauth.StateFailed, claudeNow)
	tk := f.authTask(t)
	if !strings.Contains(tk.Title, "foreman:router") {
		t.Fatalf("task: %+v", tk)
	}
	if !a.pending.Exclude(ctx, wire.JobSpec{Tier: "hardest"}, "foreman:router") {
		t.Fatal("pending picker would dispatch a claude job to the failed foreman")
	}
	if a.pending.Exclude(ctx, wire.JobSpec{Tier: "hardest"}, "foreman:assistant") {
		t.Fatal("healthy foreman excluded")
	}
	// GET /v1/claude-auth shows the state, never a value.
	code, out := doJSON(t, a, "GET", "/v1/claude-auth", nil)
	if code != 200 || out["token_stored"] != false {
		t.Fatalf("status: %d %v", code, out)
	}
	// Persisted across a restart.
	a.claude.book = nil
	if !a.claudeBlocks(ctx, wire.JobSpec{Tier: "hardest"}, "foreman:router") {
		t.Fatal("state not persisted")
	}
}

// TestClaudeAuthAlertReply: pasting the code into the alert's reply box
// goes to the re-login flow, not to a new planner task.
func TestClaudeAuthAlertReply(t *testing.T) {
	f := newClaudeFixture(t)
	a := f.a
	f.pinnedRunning(t, "job_a", "foreman:assistant", "hardest")
	sendResult(t, a, notLoggedIn("job_a"))
	tk := f.authTask(t)
	al, err := a.tasks.ListAlerts(context.Background(), tasks.AlertFilter{})
	if err != nil || len(al) != 1 || al[0].Source != claudeAuthSource || al[0].TaskID != tk.ID {
		t.Fatalf("alerts: %+v %v", al, err)
	}
	code, out := doJSON(t, a, "POST", "/v1/alerts/"+al[0].ID+"/reply", map[string]any{"text": "good-code-0123456789#state"})
	if code != 201 || out["task_id"] != tk.ID {
		t.Fatalf("alert reply: %d %v", code, out)
	}
	if v, _, err := a.secrets.Get(claudeauth.SecretName); err != nil || v != testToken {
		t.Fatalf("token not stored via alert reply: %v", err)
	}
	if strings.Contains(f.thread(t, tk.ID), "good-code-0123456789") {
		t.Fatal("code leaked into the thread")
	}
}

// TestClaudeAuthUnreportedForemanRetries: an older foreman that never
// reports cannot confirm recovery; its held jobs are retried after a while.
func TestClaudeAuthUnreportedForemanRetries(t *testing.T) {
	f := newClaudeFixture(t)
	a := f.a
	f.pinnedRunning(t, "job_a", "foreman:assistant", "hardest")
	sendResult(t, a, notLoggedIn("job_a"))
	a.claudeAuthPass(context.Background())
	if mustJob(t, a, "job_a").State != jobs.StateHeld {
		t.Fatal("released too early")
	}
	f.clk.Add(claudeUnreportedRetry)
	a.claudeAuthPass(context.Background())
	if j := mustJob(t, a, "job_a"); j.State == jobs.StateHeld {
		t.Fatalf("not retried: %q", j.HeldReason)
	}
}
