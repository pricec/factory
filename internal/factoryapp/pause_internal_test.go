// S5 usage-limit pause: rejection -> PAUSED, held requeue without retry,
// restart persistence, re-probe extend/resume, override, chat exemption.
package factoryapp

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/throttle"
	"github.com/pricec/factory/internal/usagewin"
	"github.com/pricec/factory/internal/wire"
)

var pauseNow = time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)

func newPauseApp(t *testing.T, dir string, clk *gateClock) *App {
	a := newGateApp(t, dir, clk)
	a.pauseJitter = func() time.Duration { return 0 }
	a.wakePoll = 5 * time.Millisecond
	return a
}

func rejectedJSON(reset time.Time, typ string) string {
	r := ""
	if !reset.IsZero() {
		r = `,"resetsAt":` + strconv.FormatInt(reset.Unix(), 10)
	}
	return `{"status":"rejected","rateLimitType":"` + typ + `"` + r + `}`
}

func alertTitles(t *testing.T, a *App) []string {
	t.Helper()
	al, err := a.tasks.ListAlerts(context.Background(), tasks.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range al {
		out = append(out, x.Title)
	}
	return out
}

// runningJob creates a job and drives it to running.
func runningJob(t *testing.T, a *App, id string) {
	t.Helper()
	ctx := context.Background()
	spec := wire.JobSpec{ID: id, Task: "work", Priority: 5}
	spec.Normalize()
	if err := a.jobs.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := a.jobs.Dispatch(ctx, id, "fm_1"); err != nil {
		t.Fatal(err)
	}
	if err := a.jobs.ApplyEvent(ctx, id, "started"); err != nil {
		t.Fatal(err)
	}
}

func sendResult(t *testing.T, a *App, res wire.JobResult) {
	t.Helper()
	b, _ := json.Marshal(res)
	a.handleEnvelope(context.Background(), wire.MessageEnvelope{Type: "job.result", JobID: res.JobID, Payload: b}, map[string]bool{})
}

func TestPauseRejectionEventPausesWithUntil(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	a := newPauseApp(t, t.TempDir(), clk)
	defer a.Close()
	reset := time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC)
	if err := a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindPlanner, RateLimit: rejectedJSON(reset, "five_hour")}); err != nil {
		t.Fatal(err)
	}
	// A second rejection (another run) does not alert again.
	_ = a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindBrain, RateLimit: rejectedJSON(reset, "five_hour")})

	_, u := doJSON(t, a, "GET", "/v1/usage", nil)
	g := u["gate"].(map[string]any)
	if g["paused"] != true || g["level"] != "PAUSED" || g["paused_window"] != "5h" ||
		g["paused_until"] != "2026-11-16T23:30:00Z" || g["pause_reason"] != "usage limit reached: 5h window, resumes 23:30Z" {
		t.Fatalf("gate block: %v", g)
	}
	if titles := alertTitles(t, a); len(titles) != 1 || !strings.HasPrefix(titles[0], "Factory paused: usage limit reached: 5h window") {
		t.Fatalf("alerts: %v", titles)
	}

	// New work is held with that reason - P0 included.
	_, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "x", "priority": 1})
	if out["state"] != jobs.StateHeld || out["held_reason"] != "usage limit reached: 5h window, resumes 23:30Z" {
		t.Fatalf("create during pause: %v", out)
	}
	if a.gate.Dispatching(ctx) || !a.pending.Hold(ctx) || !a.picker.Hold(ctx) {
		t.Fatalf("pickers must hold during the pause")
	}
}

func TestPauseUntilFallbacks(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	// Endpoint resets_at for the exhausted window when the event has none.
	a := newPauseApp(t, t.TempDir(), clk)
	setUsage(a, clk, 97)
	_ = a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindPlanner, RateLimit: rejectedJSON(time.Time{}, "five_hour")})
	if st, ok := a.gate.Pause(ctx); !ok || !st.Until.Equal(time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC)) || strings.Contains(st.Reason, "estimated") {
		t.Fatalf("endpoint fallback: %+v %v", st, ok)
	}
	a.Close()
	// Last resort: now + 15m, flagged as estimated.
	b := newPauseApp(t, t.TempDir(), clk)
	defer b.Close()
	_ = b.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindBrain, Limit: "You've hit your limit"})
	if st, ok := b.gate.Pause(ctx); !ok || !st.Until.Equal(pauseNow.Add(15*time.Minute)) || !strings.Contains(st.Reason, "estimated") {
		t.Fatalf("last resort: %+v %v", st, ok)
	}
}

func TestPauseRejectedJobRequeuedHeldNoRetry(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	a := newPauseApp(t, t.TempDir(), clk)
	defer a.Close()
	runningJob(t, a, "job_01HLIMITED0000000000000001")
	reset := time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC)
	_ = a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindWorker, JobID: "job_01HLIMITED0000000000000001", RateLimit: rejectedJSON(reset, "five_hour")})
	sendResult(t, a, wire.JobResult{
		JobID: "job_01HLIMITED0000000000000001", Status: wire.StatusFailed,
		Error: &wire.JobError{Kind: wire.KindCrash, Message: "Claude AI usage limit reached"},
	})

	j := mustJob(t, a, "job_01HLIMITED0000000000000001")
	if j.State != jobs.StateHeld || j.HeldReason != "usage limit reached: 5h window, resumes 23:30Z" {
		t.Fatalf("job: state %s reason %q", j.State, j.HeldReason)
	}
	if j.Attempts != 0 || j.NextAttempt != "" || j.LastError != "" {
		t.Fatalf("retry state touched: attempts=%d next=%q err=%q", j.Attempts, j.NextAttempt, j.LastError)
	}
	// The retry picker has nothing to do with it, and after the resume the
	// job releases to pending with attempts still 0.
	clk.Add(3 * time.Hour)
	a.probe = func(context.Context) error { setUsage(a, clk, 5); return nil }
	if !a.PausePass(ctx) {
		t.Fatal("expected resume")
	}
	j = mustJob(t, a, "job_01HLIMITED0000000000000001")
	if j.State != jobs.StatePending || j.Attempts != 0 {
		t.Fatalf("after resume: state %s attempts %d", j.State, j.Attempts)
	}
}

func TestPauseTextOnlyFailureRequeues(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	a := newPauseApp(t, t.TempDir(), clk)
	defer a.Close()
	runningJob(t, a, "job_01HTEXTONLY00000000000001")
	// No usage event at all: only the failure text.
	sendResult(t, a, wire.JobResult{
		JobID: "job_01HTEXTONLY00000000000001", Status: wire.StatusFailed,
		Error: &wire.JobError{Kind: wire.KindCrash, Message: "5-hour limit reached - resets 11:30pm (UTC)"},
	})
	if j := mustJob(t, a, "job_01HTEXTONLY00000000000001"); j.State != jobs.StateHeld {
		t.Fatalf("state %s", j.State)
	}
	if st, ok := a.gate.Pause(ctx); !ok || !st.Until.Equal(time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC)) {
		t.Fatalf("pause: %+v %v", st, ok)
	}
	// An ordinary failure is untouched.
	runningJob(t, a, "job_01HORDINARY0000000000001")
	sendResult(t, a, wire.JobResult{
		JobID: "job_01HORDINARY0000000000001", Status: wire.StatusFailed,
		Error: &wire.JobError{Kind: wire.KindCrash, Message: "segfault"},
	})
	if j := mustJob(t, a, "job_01HORDINARY0000000000001"); j.State != jobs.StateFailed {
		t.Fatalf("ordinary failure: state %s", j.State)
	}
}

func TestPauseSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	dir := t.TempDir()
	a := newPauseApp(t, dir, clk)
	reset := time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC)
	_ = a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindPlanner, RateLimit: rejectedJSON(reset, "five_hour")})
	a.Close()

	b := newPauseApp(t, dir, clk) // restart: no usage reading at all
	defer b.Close()
	ev := b.gate.Evaluate(ctx)
	if !ev.Paused || ev.Level != throttle.Paused || ev.FailOpen || ev.PausedUntil == nil || !ev.PausedUntil.Equal(reset) {
		t.Fatalf("after restart: %+v", ev)
	}
	_, out := doJSON(t, b, "POST", "/v1/jobs", map[string]any{"task": "x", "priority": 5})
	if out["state"] != jobs.StateHeld {
		t.Fatalf("job after restart: %v", out)
	}
	if n := len(alertTitles(t, b)); n != 1 {
		t.Fatalf("restart must not re-alert: %d", n)
	}
	// Before until, the loop does nothing (and does not probe).
	b.probe = func(context.Context) error { t.Fatal("probed before until"); return nil }
	if b.PausePass(ctx) {
		t.Fatal("resumed early")
	}
}

func TestPauseResumeReprobeExtendsThenResumes(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	a := newPauseApp(t, t.TempDir(), clk)
	defer a.Close()
	reset := time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC)
	_ = a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindPlanner, RateLimit: rejectedJSON(reset, "five_hour")})
	_, held := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "later", "priority": 5})
	_, urgent := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "urgent", "priority": 2})

	clk.Add(2*time.Hour + 31*time.Minute) // past until
	probes := 0
	// Endpoint still says exhausted, with no usable reset: backoff steps.
	a.probe = func(context.Context) error {
		probes++
		a.usage.Set(usagewin.Win5h, usagewin.SourceEndpoint, usagewin.Reading{
			Utilization: 100, ResetsAt: clk.Now().Add(time.Hour), ObservedAt: clk.Now(),
		})
		return nil
	}
	if a.PausePass(ctx) {
		t.Fatal("resumed while still exhausted")
	}
	st, ok := a.gate.Pause(ctx)
	if !ok || probes != 1 || !st.Until.Equal(clk.Now().Add(time.Hour)) || st.Probes != 1 {
		t.Fatalf("extend: %+v ok=%v probes=%d", st, ok, probes)
	}
	// Not due again until the new until.
	if a.PausePass(ctx) || probes != 1 {
		t.Fatalf("re-probed before the extended until (probes=%d)", probes)
	}
	// Window reset: the endpoint reports low usage; resume releases most
	// urgent first.
	clk.Add(time.Hour + time.Second)
	a.probe = func(context.Context) error { setUsage(a, clk, 8); return nil }
	if !a.PausePass(ctx) {
		t.Fatal("expected resume")
	}
	if _, still := a.gate.Pause(ctx); still {
		t.Fatal("still paused")
	}
	for _, id := range []string{held["job_id"].(string), urgent["job_id"].(string)} {
		if j := mustJob(t, a, id); j.State != jobs.StatePending {
			t.Fatalf("job %s not released: %s", id, j.State)
		}
	}
	titles := alertTitles(t, a)
	if len(titles) != 2 {
		t.Fatalf("want one pause + one resume alert, got %v", titles)
	}
}

func TestPauseResumeProbeFailureResumes(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	a := newPauseApp(t, t.TempDir(), clk)
	defer a.Close()
	_ = a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindPlanner, RateLimit: rejectedJSON(pauseNow.Add(time.Hour), "five_hour")})
	clk.Add(time.Hour + time.Second)
	a.probe = func(context.Context) error { return context.DeadlineExceeded }
	if !a.PausePass(ctx) {
		t.Fatal("known reset passed and probe unavailable: expect resume")
	}
}

func TestPauseOverrideOffDispatches(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	a := newPauseApp(t, t.TempDir(), clk)
	defer a.Close()
	_ = a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindPlanner, RateLimit: rejectedJSON(pauseNow.Add(2*time.Hour), "five_hour")})
	if _, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "x", "priority": 5}); out["state"] != jobs.StateHeld {
		t.Fatalf("expected held: %v", out)
	}
	code, out := doJSON(t, a, "POST", "/v1/throttle", map[string]any{"mode": "off"})
	if code != 200 || out["paused"] != true || out["mode"] != "off" {
		t.Fatalf("override: %d %v", code, out)
	}
	if _, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "y", "priority": 5}); out["state"] == jobs.StateHeld {
		t.Fatalf("override off must dispatch during the pause: %v", out)
	}
	if !a.gate.Dispatching(ctx) || a.pending.Hold(ctx) {
		t.Fatal("pickers must dispatch under the override")
	}
	if n, err := a.ReleasePass(ctx); err != nil || n != 1 {
		t.Fatalf("held job should release under override: %d %v", n, err)
	}
}

func TestPauseChatExemptOthersWait(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	a := newPauseApp(t, t.TempDir(), clk)
	defer a.Close()
	_ = a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindPlanner, RateLimit: rejectedJSON(pauseNow.Add(2*time.Hour), "five_hour")})

	// The gate: chat passes, P0 jobs do not.
	if hold, _ := a.gate.Admit(ctx, "", throttle.Request{Kind: throttle.KindChatWake}, nil, 0); hold {
		t.Fatal("chat wake must pass during the pause")
	}
	if hold, _ := a.gate.Admit(ctx, "job_x", throttle.Request{Kind: throttle.KindJob, P0: true, JobPriority: 1}, nil, 0); !hold {
		t.Fatal("P0 job must be held during the pause")
	}

	// The wake limiter: chat acquires at once; non-chat waits for resume.
	lim := pausedWakes{a: a, inner: unlimitedWakes{}}
	chatCtx, cancel := context.WithTimeout(context.WithValue(ctx, wakeKey{}, true), time.Second)
	defer cancel()
	if rel, err := lim.Acquire(chatCtx); err != nil {
		t.Fatalf("chat acquire: %v", err)
	} else {
		rel()
	}
	got := make(chan error, 1)
	go func() {
		rel, err := lim.Acquire(context.WithValue(ctx, wakeKey{}, false))
		if err == nil {
			rel()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("non-chat wake ran during the pause")
	case <-time.After(60 * time.Millisecond):
	}
	a.gate.Resume(ctx)
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("non-chat wake did not run after the resume")
	}
}

func TestPauseDeferWake(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	a := newPauseApp(t, t.TempDir(), clk)
	defer a.Close()
	if a.deferWake(ctx, context.Canceled) {
		t.Fatal("not paused: no deferral")
	}
	_ = a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindPlanner, RateLimit: rejectedJSON(pauseNow.Add(time.Hour), "five_hour")})
	done := make(chan bool, 1)
	go func() { done <- a.deferWake(ctx, context.Canceled) }()
	select {
	case <-done:
		t.Fatal("deferWake returned during the pause")
	case <-time.After(40 * time.Millisecond):
	}
	a.gate.Resume(ctx)
	if !<-done {
		t.Fatal("deferWake should report rerun after the resume")
	}
}

// TestPauseDefersJobTerminalWake: a terminal notify.job threaded to a task
// is a non-chat wake, so it waits out the pause (deferred, not dropped)
// and runs once the pause lifts.
func TestPauseDefersJobTerminalWake(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: pauseNow}
	a := newPauseApp(t, t.TempDir(), clk)
	defer a.Close()
	var mu sync.Mutex
	var prompts []string
	a.SetPlannerExec(func(_ context.Context, prompt string) (string, error) {
		mu.Lock()
		prompts = append(prompts, prompt)
		mu.Unlock()
		return "ok", nil
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(prompts) }
	tk, err := a.tasks.CreateTask(ctx, "paused job wake", tasks.DefaultPriority)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.recordUsage(ctx, wire.UsageEvent{Kind: wire.UsageKindPlanner, RateLimit: rejectedJSON(pauseNow.Add(2*time.Hour), "five_hour")})
	if a.gate.Dispatching(ctx) {
		t.Fatal("gate should be paused")
	}

	a.notifyOperatorThreaded(ctx, tk.ID, "job_paused", notifyJobType, map[string]any{"job_id": "job_paused", "state": "done"})
	a.dispatchWakers(ctx, []wire.MessageEnvelope{{
		Type: notifyJobType, ThreadID: tk.ID, JobID: "job_paused",
		Payload: json.RawMessage(`{"state":"done"}`),
	}})
	time.Sleep(150 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("job wake ran during the pause: %d sessions", n)
	}
	a.gate.Resume(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "reached a terminal state") {
		t.Fatalf("job wake after resume: %d sessions %q", len(prompts), prompts)
	}
}
