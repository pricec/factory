// S3 dispatch gate: held -> released lifecycle, cancel while held, restart
// persistence, P0 bypass, chat-wake bypass, override, fail-open.
package factoryapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/budgets"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/usagewin"
	"github.com/pricec/factory/internal/wire"
)

type gateClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *gateClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *gateClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newGateApp(t *testing.T, dir string, clk *gateClock, opts ...func(*Config)) *App {
	t.Helper()
	cfg := Config{
		DataDir: dir, Addr: "127.0.0.1:0", AdvertiseURL: "http://factory.test",
		Budget: budgets.DefaultConfig(), Now: clk.Now,
	}
	for _, o := range opts {
		o(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// setUsage feeds a fresh live 5h reading (the S2 poller's path).
func setUsage(a *App, clk *gateClock, util float64) {
	a.usage.Set(usagewin.Win5h, usagewin.SourceEndpoint, usagewin.Reading{
		Utilization: util, ResetsAt: time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC), ObservedAt: clk.Now(),
	})
}

func doJSON(t *testing.T, a *App, method, path string, in any) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	if in != nil {
		_ = json.NewEncoder(&body).Encode(in)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, &body))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func mustJob(t *testing.T, a *App, id string) jobs.Job {
	t.Helper()
	j, ok, err := a.jobs.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("get %s: ok=%v err=%v", id, ok, err)
	}
	return j
}

func TestThrottleHeldLifecycle(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	setUsage(a, clk, 96) // >= 95% -> RED

	code, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "build it", "priority": 5})
	if code != http.StatusCreated || out["state"] != jobs.StateHeld {
		t.Fatalf("create: %d %v", code, out)
	}
	reason, _ := out["held_reason"].(string)
	if !strings.HasPrefix(reason, "usage RED: 5h 96%, resets 23:30Z") {
		t.Fatalf("held_reason %q", reason)
	}
	id := out["job_id"].(string)
	if j := mustJob(t, a, id); j.State != jobs.StateHeld || j.HeldReason != reason || j.Spec.Tier != "" && j.Spec.Tier != "flash" {
		t.Fatalf("stored: %+v", j)
	}
	// A more urgent job created later releases first (priority, then FIFO).
	_, out2 := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "urgent", "priority": 2})
	_, out3 := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "later", "priority": 5})
	held, err := a.jobs.ListHeld(ctx, 0)
	if err != nil || len(held) != 3 || held[0].ID != out2["job_id"] || held[1].ID != id || held[2].ID != out3["job_id"] {
		t.Fatalf("held order: %+v %v", held, err)
	}

	// GET /v1/usage carries the gate block.
	_, u := doJSON(t, a, "GET", "/v1/usage", nil)
	g := u["gate"].(map[string]any)
	if g["mode"] != "auto" || g["level"] != "RED" || g["held"].(float64) != 3 {
		t.Fatalf("gate block: %v", g)
	}

	// Still RED: nothing released.
	if n, err := a.ReleasePass(ctx); err != nil || n != 0 {
		t.Fatalf("release in RED: %d %v", n, err)
	}
	// Usage falls to GREEN: hysteresis keeps RED for 10 minutes.
	setUsage(a, clk, 10)
	if n, _ := a.ReleasePass(ctx); n != 0 {
		t.Fatalf("released during hysteresis: %d", n)
	}
	clk.Add(10 * time.Minute)
	setUsage(a, clk, 10)
	n, err := a.ReleasePass(ctx)
	if err != nil || n != 3 {
		t.Fatalf("release after hysteresis: %d %v", n, err)
	}
	j := mustJob(t, a, id)
	if j.State != jobs.StatePending || j.HeldReason != "" {
		t.Fatalf("released job: state %s reason %q", j.State, j.HeldReason)
	}
	// From pending, the normal picker dispatches (no foreman here: stays pending).
	if c, _ := a.jobs.CountHeld(ctx); c != 0 {
		t.Fatalf("held left: %d", c)
	}
}

func TestThrottleCancelWhileHeld(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	setUsage(a, clk, 99)
	_, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "x"})
	id := out["job_id"].(string)
	code, c := doJSON(t, a, "POST", "/v1/jobs/"+id+"/cancel", map[string]any{"reason": "not needed"})
	if code != http.StatusOK || c["state"] != jobs.StateCancelled {
		t.Fatalf("cancel: %d %v", code, c)
	}
	a.gate.SetOverride(ctx, "off", time.Time{}) //nolint:errcheck // admit everything
	if n, _ := a.ReleasePass(ctx); n != 0 {
		t.Fatalf("released a cancelled job")
	}
	if j := mustJob(t, a, id); j.State != jobs.StateCancelled {
		t.Fatalf("state %s", j.State)
	}
	// Cancelling again: terminal.
	if code, _ := doJSON(t, a, "POST", "/v1/jobs/"+id+"/cancel", nil); code != http.StatusConflict {
		t.Fatalf("re-cancel %d", code)
	}
}

func TestThrottleHeldSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, dir, clk)
	setUsage(a, clk, 97)
	_, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "persist me"})
	id := out["job_id"].(string)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b := newGateApp(t, dir, clk)
	defer b.Close()
	j := mustJob(t, b, id)
	if j.State != jobs.StateHeld || !strings.Contains(j.HeldReason, "RED") {
		t.Fatalf("after restart: %s %q", j.State, j.HeldReason)
	}
	// No reading after restart yet: fail open releases it (never deadlock).
	n, err := b.ReleasePass(ctx)
	if err != nil || n != 1 || mustJob(t, b, id).State != jobs.StatePending {
		t.Fatalf("release after restart: %d %v", n, err)
	}
}

func TestThrottleP0BypassAndOrange(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	p0, err := a.tasks.CreateTask(ctx, "fire", 0)
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := a.tasks.CreateTask(ctx, "normal", 2)
	setUsage(a, clk, 99) // RED
	_, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "p0 work", "task_id": p0.ID, "priority": 9})
	if out["state"] == jobs.StateHeld {
		t.Fatalf("P0 job held in RED: %v", out)
	}
	_, out = doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "normal", "task_id": p2.ID, "priority": 1})
	if out["state"] != jobs.StateHeld {
		t.Fatalf("non-P0 not held in RED: %v", out)
	}
	heldID := out["job_id"].(string)
	// Operator promotes the task to P0: the releaser lets it through.
	if _, err := a.inbox.DB().ExecContext(ctx, `UPDATE tasks SET priority = 0 WHERE id = ?`, p2.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.ReleasePass(ctx); n != 1 || mustJob(t, a, heldID).State != jobs.StatePending {
		t.Fatalf("promoted job not released: %d", n)
	}

	// ORANGE (util 88%, > 85%): priority >= 5 held, priority 4 admitted.
	// At 23:00 the window is 90% elapsed, so pace (0.98) stays below RED.
	clk2 := &gateClock{t: time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)}
	b := newGateApp(t, t.TempDir(), clk2)
	defer b.Close()
	setUsage(b, clk2, 88)
	_, o5 := doJSON(t, b, "POST", "/v1/jobs", map[string]any{"task": "p5", "priority": 5})
	_, o4 := doJSON(t, b, "POST", "/v1/jobs", map[string]any{"task": "p4", "priority": 4})
	if o5["state"] != jobs.StateHeld || o4["state"] == jobs.StateHeld {
		t.Fatalf("orange: p5=%v p4=%v", o5, o4)
	}
	if r := o5["held_reason"].(string); !strings.HasPrefix(r, "usage ORANGE: 5h 88%, resets 23:30Z") {
		t.Fatalf("orange reason %q", r)
	}
}

func TestThrottleOverrideAndKillSwitch(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	a := newGateApp(t, dir, clk)
	setUsage(a, clk, 99)
	until := clk.Now().Add(30 * time.Minute).Format(time.RFC3339)
	code, g := doJSON(t, a, "POST", "/v1/throttle", map[string]any{"mode": "off", "until": until})
	if code != http.StatusOK || g["mode"] != "off" || g["mode_source"] != "override" {
		t.Fatalf("override: %d %v", code, g)
	}
	if code, _ := doJSON(t, a, "POST", "/v1/throttle", map[string]any{"mode": "force"}); code != http.StatusBadRequest {
		t.Fatalf("bad mode %d", code)
	}
	_, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "x"})
	if out["state"] == jobs.StateHeld {
		t.Fatal("held under off override")
	}
	_ = a.Close()
	// The override survives a restart, then expires on its own.
	b := newGateApp(t, dir, clk)
	defer b.Close()
	setUsage(b, clk, 99)
	if ev := b.gate.Evaluate(ctx); ev.Mode != "off" {
		t.Fatalf("override lost on restart: %+v", ev)
	}
	clk.Add(31 * time.Minute)
	setUsage(b, clk, 99)
	if _, out := doJSON(t, b, "POST", "/v1/jobs", map[string]any{"task": "y"}); out["state"] != jobs.StateHeld {
		t.Fatalf("override did not expire: %v", out)
	}

	// FACTORY_THROTTLE=off.
	c := newGateApp(t, t.TempDir(), clk, func(cfg *Config) { cfg.Throttle.Off = true })
	defer c.Close()
	setUsage(c, clk, 99)
	if _, out := doJSON(t, c, "POST", "/v1/jobs", map[string]any{"task": "z"}); out["state"] == jobs.StateHeld {
		t.Fatal("held with kill switch")
	}
	if _, u := doJSON(t, c, "GET", "/v1/usage", nil); u["gate"].(map[string]any)["mode_source"] != "env" {
		t.Fatalf("usage gate: %v", u["gate"])
	}
}

func TestThrottleFailOpenNoReading(t *testing.T) {
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	setUsage(a, clk, 99)
	clk.Add(11 * time.Minute) // reading goes stale (> 10 min)
	if _, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "x"}); out["state"] == jobs.StateHeld {
		t.Fatalf("held on stale reading: %v", out)
	}
	if _, u := doJSON(t, a, "GET", "/v1/usage", nil); u["gate"].(map[string]any)["fail_open"] != true {
		t.Fatalf("fail_open not shown: %v", u["gate"])
	}
}

func TestThrottleSchedulerHolds(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	tmpl := wire.NewJobSpec("nightly", "factory")
	tmpl.ID, tmpl.CreatedAt = "", ""
	if _, err := a.jobs.CreateSchedule(ctx, "* * * * *", tmpl); err != nil {
		t.Fatal(err)
	}
	clk.Add(2 * time.Minute)
	setUsage(a, clk, 99)
	if _, err := a.scheduler.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.jobs.CountHeld(ctx); n != 1 {
		t.Fatalf("scheduled job not held: %d", n)
	}
}

// TestThrottleChatWakeInRed: an operator chat message still wakes the
// planner (and gets its reply) while usage is RED.
func TestThrottleChatWakeInRed(t *testing.T) {
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	setUsage(a, clk, 100)
	if ev := a.gate.Evaluate(context.Background()); ev.Level != usagewin.Red {
		t.Fatalf("level %s", ev.Level)
	}
	woke := make(chan string, 4)
	a.SetPlannerExec(func(ctx context.Context, prompt string) (string, error) {
		woke <- prompt
		return "ok", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx) //nolint:errcheck
	if code, _ := doJSON(t, a, "POST", "/v1/chat", map[string]any{"text": "status in red?"}); code != http.StatusCreated {
		t.Fatalf("chat %d", code)
	}
	select {
	case p := <-woke:
		if !strings.Contains(p, "status in red?") {
			t.Fatalf("wake prompt lacks chat text")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("planner not woken for chat in RED")
	}
}
