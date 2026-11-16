// Long-pending alert behavior tests (spec §6.7, §9.5; 2026-11-15
// amendment): fires past 30m pending-with-no-match, skips pending jobs
// that merely queue behind a deliberate hold (they match an online
// foreman), and dedupes per job per pending episode.
//
// The env is quiet (no background loops): the alert passes are driven
// explicitly, so the pending picker cannot dispatch the jobs out from
// under the assertions and the fake clock alone controls job age.
package factoryapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pricec/factory/internal/budgets"
	"github.com/pricec/factory/internal/factoryapp"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// newQuietEnv boots the App with the fake clock but WITHOUT Run: no
// background loops, so tests drive PendingAlertPass deterministically.
func newQuietEnv(t *testing.T) *testEnv {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 11, 15, 15, 0, 0, 0, time.UTC)}
	cfg := factoryapp.Config{
		DataDir:      t.TempDir(),
		Addr:         "127.0.0.1:0",
		AdvertiseURL: testAdvertiseURL,
		Budget:       budgets.DefaultConfig(),
		Now:          clock.Now,
	}
	a, err := factoryapp.New(cfg)
	if err != nil {
		t.Fatalf("factoryapp.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &testEnv{t: t, a: a, ts: httptest.NewServer(a.Handler()), clock: clock, ctx: ctx, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		e.ts.Close()
		_ = a.Close()
	})
	return e
}

// createPendingJob creates a job via the UI create path (POST /v1/jobs,
// spec §6.9) and fails unless it came back pending.
func (e *testEnv) createPendingJob(requires []string) string {
	e.t.Helper()
	var created struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	body := map[string]any{"task": "pending alert smoke"}
	if len(requires) > 0 {
		body["requires"] = requires
	}
	if status := e.do("POST", "/v1/jobs", "", body, &created); status != http.StatusCreated {
		e.t.Fatalf("POST /v1/jobs status = %d, want 201", status)
	}
	if created.State != jobs.StatePending {
		e.t.Fatalf("created state = %s, want pending", created.State)
	}
	return created.JobID
}

// pendingAlerts returns the notify.pending envelopes in the operator
// inbox (planner:main, spec §9.5), with payloads decoded.
func (e *testEnv) pendingAlerts() []struct {
	env  wire.MessageEnvelope
	body map[string]any
} {
	e.t.Helper()
	var envs []wire.MessageEnvelope
	if status := e.do("GET", "/v1/inboxes/planner:main/recent?limit=100", "", nil, &envs); status != http.StatusOK {
		e.t.Fatalf("GET planner:main/recent status = %d, want 200", status)
	}
	var out []struct {
		env  wire.MessageEnvelope
		body map[string]any
	}
	for _, env := range envs {
		if env.Type != "notify.pending" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(env.Payload, &body); err != nil {
			e.t.Fatalf("decode notify.pending payload: %v", err)
		}
		out = append(out, struct {
			env  wire.MessageEnvelope
			body map[string]any
		}{env, body})
	}
	return out
}

// markOffline flips a foreman offline through the same liveness observer
// the hub drives on disconnect (spec §6.3), and returns its last_seen.
func (e *testEnv) markOffline(id string) string {
	e.t.Helper()
	jobs.NewLiveness(e.a.Jobs()).OnDisconnect(id)
	rows, err := e.a.Jobs().Foremen(e.ctx)
	if err != nil {
		e.t.Fatalf("foremen: %v", err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row.LastSeen
		}
	}
	e.t.Fatalf("foreman %s not registered", id)
	return ""
}

func bodyString(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	v, ok := body[key].(string)
	if !ok {
		t.Fatalf("payload %s = %v (%T), want string", key, body[key], body[key])
	}
	return v
}

func bodyStrings(t *testing.T, body map[string]any, key string) []string {
	t.Helper()
	raw, ok := body[key].([]any)
	if !ok {
		t.Fatalf("payload %s = %v (%T), want array", key, body[key], body[key])
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("payload %s item = %v (%T), want string", key, item, item)
		}
		out = append(out, s)
	}
	return out
}

// TestPendingAlertFiresPastThreshold: the incident shape - a foreman is
// online but advertises the wrong keys (os/arch, not os:linux/arch:amd64),
// so a requires-carrying job sits pending with no match past 30 minutes.
// One notify.pending names the job, its requires, and the missing tags;
// a second pass does not repeat it (dedupe per pending episode); a
// deadletter requeue starts a fresh episode and re-alerts.
func TestPendingAlertFiresPastThreshold(t *testing.T) {
	e := newQuietEnv(t)
	// A foreman is around (the registry is not empty) but carries none of
	// the required tags - the 2026-11-15 incident's exact shape.
	_ = e.pair("alpha", nil)
	jobID := e.createPendingJob([]string{"os:linux", "arch:amd64"})

	// 29 minutes: still inside the threshold, no alert.
	e.clock.Advance(29 * time.Minute)
	if n, err := e.a.PendingAlertPass(e.ctx); err != nil || n != 0 {
		t.Fatalf("pass at 29m: posted=%d err=%v, want 0 alerts", n, err)
	}
	if alerts := e.pendingAlerts(); len(alerts) != 0 {
		t.Fatalf("got %d alerts at 29m, want 0", len(alerts))
	}

	// Past 30 minutes: one alert with the diagnosis.
	e.clock.Advance(2 * time.Minute)
	if n, err := e.a.PendingAlertPass(e.ctx); err != nil || n != 1 {
		t.Fatalf("pass at 31m: posted=%d err=%v, want 1 alert", n, err)
	}
	alerts := e.pendingAlerts()
	if len(alerts) != 1 {
		t.Fatalf("got %d notify.pending envelopes, want 1", len(alerts))
	}
	env, body := alerts[0].env, alerts[0].body
	if env.From != "factory" {
		t.Errorf("envelope from = %q, want factory", env.From)
	}
	if got := bodyString(t, body, "job_id"); got != jobID {
		t.Errorf("payload job_id = %q, want %q", got, jobID)
	}
	if got := bodyStrings(t, body, "requires"); len(got) != 2 || got[0] != "os:linux" || got[1] != "arch:amd64" {
		t.Errorf("payload requires = %v, want [os:linux arch:amd64]", got)
	}
	if got := bodyString(t, body, "reason"); got != "no foreman advertises: arch:amd64, os:linux" {
		t.Errorf("payload reason = %q, want the missing-tags diagnosis", got)
	}
	if got := bodyStrings(t, body, "missing_tags"); len(got) != 2 || got[0] != "arch:amd64" || got[1] != "os:linux" {
		t.Errorf("payload missing_tags = %v, want [arch:amd64 os:linux]", got)
	}
	if got, ok := body["pending_minutes"].(float64); !ok || got < 31 {
		t.Errorf("payload pending_minutes = %v, want >= 31", body["pending_minutes"])
	}

	// Dedupe: another pass with the job still pending posts nothing.
	if n, err := e.a.PendingAlertPass(e.ctx); err != nil || n != 0 {
		t.Fatalf("dedupe pass: posted=%d err=%v, want 0", n, err)
	}
	if alerts := e.pendingAlerts(); len(alerts) != 1 {
		t.Fatalf("alerts after dedupe pass = %d, want 1 (no spam)", len(alerts))
	}

	// The job leaves pending (runs, fails validation, dead-letters) and is
	// requeued: a fresh pending episode, so it alerts again.
	if err := e.a.Jobs().Dispatch(e.ctx, jobID, "foreman:alpha"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := e.a.Jobs().ApplyEvent(e.ctx, jobID, "started"); err != nil {
		t.Fatalf("started: %v", err)
	}
	res := wire.JobResult{
		JobID: jobID, Status: wire.StatusFailed, Summary: "boom",
		Error: &wire.JobError{Kind: "validation", Message: "boom"},
	}
	if err := e.a.Jobs().ApplyResult(e.ctx, res); err != nil {
		t.Fatalf("apply result: %v", err)
	}
	if err := e.a.Jobs().MarkFailedForRetry(e.ctx, jobID, "validation"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if err := e.a.Jobs().Requeue(e.ctx, jobID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	e.clock.Advance(31 * time.Minute)
	if n, err := e.a.PendingAlertPass(e.ctx); err != nil || n != 1 {
		t.Fatalf("post-requeue pass: posted=%d err=%v, want 1 fresh alert", n, err)
	}
	if alerts := e.pendingAlerts(); len(alerts) != 2 {
		t.Fatalf("alerts after requeue = %d, want 2 (fresh episode re-alerts)", len(alerts))
	}
}

// TestPendingAlertSkipsMatchableJob: a pending job whose requires match an
// online foreman is not stuck - the pending picker dispatches it on its
// next pass (spec §6.7) - so even hours "pending" behind a deliberate hold
// never pages the operator.
func TestPendingAlertSkipsMatchableJob(t *testing.T) {
	e := newQuietEnv(t)
	// Created with no foreman around: pending.
	jobID := e.createPendingJob([]string{"os:linux", "arch:amd64"})
	// A matching foreman comes online with the standard native caps
	// (post-fix2 match semantics: requires "os:linux" is satisfied by a
	// foreman advertising os=linux, not by legacy latch keys). The quiet
	// env runs no pending picker, so the job stays pending - the
	// deliberate-hold shape.
	_ = e.pair("server", map[string]string{"os": "linux", "arch": "amd64"})

	e.clock.Advance(2 * time.Hour)
	for i := 0; i < 3; i++ {
		if n, err := e.a.PendingAlertPass(e.ctx); err != nil || n != 0 {
			t.Fatalf("pass %d: posted=%d err=%v, want 0 alerts for a matchable job", i+1, n, err)
		}
	}
	if alerts := e.pendingAlerts(); len(alerts) != 0 {
		t.Fatalf("got %d notify.pending envelopes for job %s, want 0", len(alerts), jobID)
	}
}

// TestPendingAlertNamesOfflineForeman: the required tags exist in the
// registry but only on an offline foreman - the alert says so, with the
// foreman's last_seen for actionability.
func TestPendingAlertNamesOfflineForeman(t *testing.T) {
	e := newQuietEnv(t)
	// Created with no foreman around: pending.
	jobID := e.createPendingJob([]string{"os:linux", "arch:amd64"})
	// A foreman with exactly the required tags pairs (native caps, the
	// post-fix2 match form), then goes dark (the disconnect path the SSE
	// hub drives, spec §6.3).
	_ = e.pair("server", map[string]string{"os": "linux", "arch": "amd64"})
	lastSeen := e.markOffline("foreman:server")

	e.clock.Advance(31 * time.Minute)
	if n, err := e.a.PendingAlertPass(e.ctx); err != nil || n != 1 {
		t.Fatalf("pass: posted=%d err=%v, want 1 alert", n, err)
	}
	alerts := e.pendingAlerts()
	if len(alerts) != 1 {
		t.Fatalf("got %d notify.pending envelopes, want 1", len(alerts))
	}
	_, body := alerts[0].env, alerts[0].body
	if got := bodyString(t, body, "job_id"); got != jobID {
		t.Errorf("payload job_id = %q, want %q", got, jobID)
	}
	if got := bodyString(t, body, "reason"); got != "only offline foremen have: arch:amd64, os:linux" {
		t.Errorf("payload reason = %q, want the offline-foreman diagnosis", got)
	}
	offline := bodyStrings(t, body, "offline_foremen")
	if len(offline) != 1 || offline[0] != "foreman:server (last seen "+lastSeen+")" {
		t.Errorf("payload offline_foremen = %v, want foreman:server with its last_seen", offline)
	}
}

// TestPendingAlertEmptyRequiresAllOffline: a job with no requires matches
// ANY online foreman; with foremen registered but all offline, the alert
// still fires and says the registry is dark.
func TestPendingAlertEmptyRequiresAllOffline(t *testing.T) {
	e := newQuietEnv(t)
	// Created with no foreman around: pending.
	jobID := e.createPendingJob(nil)
	_ = e.pair("alpha", nil)
	_ = e.markOffline("foreman:alpha")

	e.clock.Advance(31 * time.Minute)
	if n, err := e.a.PendingAlertPass(e.ctx); err != nil || n != 1 {
		t.Fatalf("pass: posted=%d err=%v, want 1 alert", n, err)
	}
	alerts := e.pendingAlerts()
	if len(alerts) != 1 {
		t.Fatalf("got %d notify.pending envelopes, want 1", len(alerts))
	}
	if got := bodyString(t, alerts[0].body, "job_id"); got != jobID {
		t.Errorf("payload job_id = %q, want %q", got, jobID)
	}
	if got := bodyString(t, alerts[0].body, "reason"); got != "all foremen are offline" {
		t.Errorf("payload reason = %q, want the all-offline diagnosis", got)
	}
}
