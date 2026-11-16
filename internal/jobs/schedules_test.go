// Schedule + Scheduler tests (spec §6.8, issue #8). Self-contained helpers
// (sched-prefixed): jobs_test.go is concurrently edited by other changes.

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/wire"
)

func schedStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open jobs store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// schedClock returns an injectable fake clock at a fixed base; tests
// advance it with *now = now.Add(...).
func schedClock() (now *time.Time) {
	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	return &base
}

// schedMatcher is a stub registry.Matcher returning a canned foreman.
type schedMatcher struct {
	foreman  registry.Foreman
	ok       bool
	err      error
	requires []string
	calls    int
}

func (m *schedMatcher) Match(_ context.Context, requires []string) (registry.Foreman, bool, error) {
	m.calls++
	m.requires = requires
	return m.foreman, m.ok, m.err
}

func schedTemplate(mutate func(*wire.JobSpec)) wire.JobSpec {
	tmpl := wire.JobSpec{Task: "scheduled task", CreatedBy: "schedule-test"}
	if mutate != nil {
		mutate(&tmpl)
	}
	return tmpl
}

func mustCreateSchedule(t *testing.T, s *Store, cronSpec string, tmpl wire.JobSpec) Schedule {
	t.Helper()
	sch, err := s.CreateSchedule(context.Background(), cronSpec, tmpl)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	return sch
}

func schedQueryInt(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func schedJobs(t *testing.T, s *Store) []Job {
	t.Helper()
	jobs, err := s.List(context.Background(), "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestScheduleCreateAndList(t *testing.T) {
	s := schedStore(t)
	now := schedClock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()

	sch := mustCreateSchedule(t, s, "*/5 * * * *", schedTemplate(func(sp *wire.JobSpec) {
		sp.Requires = []string{"gpu"}
	}))
	if !strings.HasPrefix(sch.ID, "sched_") {
		t.Errorf("id %q: want sched_ prefix", sch.ID)
	}
	if !sch.Enabled {
		t.Error("new schedule should be enabled")
	}
	if sch.CronSpec != "*/5 * * * *" {
		t.Errorf("cron_spec = %q", sch.CronSpec)
	}
	// ValidateTemplate applied the pinned defaults and left id/created_at
	// empty (assigned when the schedule fires).
	if sch.JobTemplate.TimeoutS != wire.DefaultTimeoutS || sch.JobTemplate.Priority != wire.DefaultPriority {
		t.Errorf("template defaults: timeout_s=%d priority=%d", sch.JobTemplate.TimeoutS, sch.JobTemplate.Priority)
	}
	if sch.JobTemplate.Budget != (wire.Budget{MaxPromptBytes: wire.DefaultMaxPromptBytes, MaxOutputTokens: wire.DefaultMaxOutputTokens, MaxCostUSD: wire.DefaultMaxCostUSD}) {
		t.Errorf("budget defaults: %+v", sch.JobTemplate.Budget)
	}
	if sch.JobTemplate.ID != "" || sch.JobTemplate.CreatedAt != "" {
		t.Errorf("template id/created_at should be empty: %q %q", sch.JobTemplate.ID, sch.JobTemplate.CreatedAt)
	}
	// Anchor seeded at creation time.
	if want := now.UTC().Format(time.RFC3339); sch.LastRunAt != want {
		t.Errorf("last_run_at = %q, want creation anchor %q", sch.LastRunAt, want)
	}

	list, err := s.Schedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("schedules = %d, want 1", len(list))
	}
	got := list[0]
	if got.ID != sch.ID || got.CronSpec != sch.CronSpec || !got.Enabled ||
		got.JobTemplate.Task != "scheduled task" || len(got.JobTemplate.Requires) != 1 ||
		got.JobTemplate.Requires[0] != "gpu" || got.LastRunAt != sch.LastRunAt {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestScheduleCreateInvalid(t *testing.T) {
	s := schedStore(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		cronSpec string
		mutate   func(*wire.JobSpec)
	}{
		{"not a cron", "not a cron", nil},
		{"six fields", "*/1 * * * * *", nil},
		{"minute out of range", "61 * * * *", nil},
		{"empty cron", "", nil},
		{"empty task", "*/1 * * * *", func(sp *wire.JobSpec) { sp.Task = "" }},
		{"priority out of range", "*/1 * * * *", func(sp *wire.JobSpec) { sp.Priority = 10 }},
		{"budget negative", "*/1 * * * *", func(sp *wire.JobSpec) { sp.Budget = wire.Budget{MaxPromptBytes: -1} }},
		{"bad result schema", "*/1 * * * *", func(sp *wire.JobSpec) {
			sp.ResultSchema = json.RawMessage(`{"bogus_key": 1}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateSchedule(ctx, tc.cronSpec, schedTemplate(tc.mutate))
			var bad *errBadSchedule
			if !errors.As(err, &bad) {
				t.Fatalf("err = %v, want errBadSchedule", err)
			}
		})
	}

	list, err := s.Schedules(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("schedules = %d (err %v), want 0", len(list), err)
	}
}

func TestScheduleEnableDisable(t *testing.T) {
	s := schedStore(t)
	ctx := context.Background()
	sch := mustCreateSchedule(t, s, "*/1 * * * *", schedTemplate(nil))

	if err := s.SetScheduleEnabled(ctx, sch.ID, false); err != nil {
		t.Fatal(err)
	}
	list, err := s.Schedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Enabled {
		t.Error("disable did not take")
	}
	if err := s.SetScheduleEnabled(ctx, sch.ID, true); err != nil {
		t.Fatal(err)
	}
	list, _ = s.Schedules(ctx)
	if !list[0].Enabled {
		t.Error("enable did not take")
	}

	if err := s.SetScheduleEnabled(ctx, "sched_missing", true); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("enable missing: err = %v, want ErrScheduleNotFound", err)
	}
	if err := s.DeleteSchedule(ctx, "sched_missing"); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("delete missing: err = %v, want ErrScheduleNotFound", err)
	}

	if err := s.DeleteSchedule(ctx, sch.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Schedules(ctx); len(list) != 0 {
		t.Errorf("schedules after delete = %d, want 0", len(list))
	}
	if got := schedQueryInt(t, s, `SELECT COUNT(*) FROM schedule_runs`); got != 0 {
		t.Errorf("schedule_runs after delete = %d, want 0", got)
	}
}

func TestSchedulerNextFire(t *testing.T) {
	s := schedStore(t)
	now := schedClock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	m := &schedMatcher{foreman: registry.Foreman{ID: "foreman:alpha"}, ok: true}
	sched := NewScheduler(s, m)
	sch := mustCreateSchedule(t, s, "*/1 * * * *", schedTemplate(nil))

	// 12:00:30: the 12:01 fire is still in the future.
	*now = now.Add(30 * time.Second)
	if n, err := sched.Pump(ctx); err != nil || n != 0 {
		t.Fatalf("pump at 12:00:30 = (%d, %v), want (0, nil)", n, err)
	}

	// Exactly 12:01:00: due (fire <= now).
	*now = now.Add(30 * time.Second)
	n, err := sched.Pump(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pump at 12:01:00 = (%d, %v), want (1, nil)", n, err)
	}
	jobs := schedJobs(t, s)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if want := "2026-11-14T12:01:00Z"; jobs[0].Spec.CreatedAt != want {
		t.Errorf("created_at = %q, want fire time %q", jobs[0].Spec.CreatedAt, want)
	}
	if !strings.HasPrefix(jobs[0].Spec.ID, "job_") {
		t.Errorf("job id %q: want job_ prefix", jobs[0].Spec.ID)
	}
	if jobs[0].State != StateDispatched || jobs[0].ForemanID != "foreman:alpha" {
		t.Errorf("job state=%s foreman=%q, want dispatched to foreman:alpha", jobs[0].State, jobs[0].ForemanID)
	}
	if m.calls != 1 {
		t.Errorf("matcher calls = %d, want 1", m.calls)
	}
	list, _ := s.Schedules(ctx)
	if list[0].LastRunAt != "2026-11-14T12:01:00Z" {
		t.Errorf("last_run_at = %q, want the 12:01 fire", list[0].LastRunAt)
	}
	_ = sch

	// 12:01:30: the 12:02 fire is not yet due.
	*now = now.Add(30 * time.Second)
	if n, err := sched.Pump(ctx); err != nil || n != 0 {
		t.Fatalf("pump at 12:01:30 = (%d, %v), want (0, nil)", n, err)
	}
}

func TestSchedulerDoublePumpIdempotent(t *testing.T) {
	s := schedStore(t)
	now := schedClock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	sched := NewScheduler(s, &schedMatcher{}) // no foreman: jobs stay pending
	sch := mustCreateSchedule(t, s, "*/1 * * * *", schedTemplate(nil))

	// 12:01:30: the 12:01 tick is due.
	*now = now.Add(90 * time.Second)
	if n, err := sched.Pump(ctx); err != nil || n != 1 {
		t.Fatalf("first pump = (%d, %v), want (1, nil)", n, err)
	}
	// Second pump at the same clock: no duplicate.
	if n, err := sched.Pump(ctx); err != nil || n != 0 {
		t.Fatalf("second pump = (%d, %v), want (0, nil)", n, err)
	}
	if jobs := schedJobs(t, s); len(jobs) != 1 {
		t.Fatalf("jobs after double pump = %d, want 1", len(jobs))
	}
	if got := schedQueryInt(t, s, `SELECT COUNT(*) FROM schedule_runs WHERE schedule_id = ?`, sch.ID); got != 1 {
		t.Fatalf("schedule_runs = %d, want 1", got)
	}

	// Crash simulation: the anchor was rewound between the run claim and
	// the last_run_at update. The unique (schedule_id, run_at) row must
	// dedupe the tick instead of creating a second job.
	if _, err := s.db.Exec(`UPDATE schedules SET last_run_at = ? WHERE id = ?`,
		"2026-11-14T12:00:00Z", sch.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := sched.Pump(ctx); err != nil || n != 0 {
		t.Fatalf("pump after crash-sim = (%d, %v), want (0, nil)", n, err)
	}
	if jobs := schedJobs(t, s); len(jobs) != 1 {
		t.Fatalf("jobs after crash-sim = %d, want 1", len(jobs))
	}
}

func TestSchedulerDisabledNeverFires(t *testing.T) {
	s := schedStore(t)
	now := schedClock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	sched := NewScheduler(s, &schedMatcher{})
	sch := mustCreateSchedule(t, s, "*/1 * * * *", schedTemplate(nil))
	if err := s.SetScheduleEnabled(ctx, sch.ID, false); err != nil {
		t.Fatal(err)
	}

	*now = now.Add(5 * time.Minute)
	for i := 0; i < 3; i++ {
		if n, err := sched.Pump(ctx); err != nil || n != 0 {
			t.Fatalf("pump %d while disabled = (%d, %v), want (0, nil)", i, n, err)
		}
	}
	if jobs := schedJobs(t, s); len(jobs) != 0 {
		t.Fatalf("disabled schedule fired %d jobs", len(jobs))
	}
	list, _ := s.Schedules(ctx)
	if list[0].LastRunAt != "2026-11-14T12:00:00Z" {
		t.Errorf("anchor moved while disabled: %q", list[0].LastRunAt)
	}

	// Re-enable: the backlog drains one tick per pass, starting at the
	// first boundary after the anchor (12:01).
	if err := s.SetScheduleEnabled(ctx, sch.ID, true); err != nil {
		t.Fatal(err)
	}
	n, err := sched.Pump(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pump after re-enable = (%d, %v), want (1, nil)", n, err)
	}
	jobs := schedJobs(t, s)
	if len(jobs) != 1 || jobs[0].Spec.CreatedAt != "2026-11-14T12:01:00Z" {
		t.Fatalf("first catch-up job created_at = %q (len %d)", jobs[0].Spec.CreatedAt, len(jobs))
	}
	if n, err := sched.Pump(ctx); err != nil || n != 1 {
		t.Fatalf("second catch-up pump = (%d, %v), want (1, nil)", n, err)
	}
}

func TestSchedulerInstantiation(t *testing.T) {
	s := schedStore(t)
	now := schedClock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	sched := NewScheduler(s, &schedMatcher{})

	tmpl := schedTemplate(func(sp *wire.JobSpec) {
		sp.Payload = json.RawMessage(`{"build": true}`)
		sp.Requires = []string{"gpu", "docker"}
		sp.TimeoutS = 120
		sp.Priority = 7
		sp.Budget = wire.Budget{MaxPromptBytes: 4096, MaxOutputTokens: 512, MaxCostUSD: 1.5}
	})
	mustCreateSchedule(t, s, "*/1 * * * *", tmpl)

	*now = now.Add(time.Minute) // 12:01:00
	if n, err := sched.Pump(ctx); err != nil || n != 1 {
		t.Fatalf("pump = (%d, %v), want (1, nil)", n, err)
	}
	jobs := schedJobs(t, s)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	spec := jobs[0].Spec
	if err := spec.Validate(); err != nil {
		t.Fatalf("instantiated spec invalid: %v", err)
	}
	if !strings.HasPrefix(spec.ID, "job_") || len(spec.ID) <= len("job_") {
		t.Errorf("job id %q: want a fresh job_-prefixed ULID", spec.ID)
	}
	if spec.CreatedAt != "2026-11-14T12:01:00Z" {
		t.Errorf("created_at = %q, want the fire time", spec.CreatedAt)
	}
	if spec.Task != tmpl.Task || spec.CreatedBy != "schedule-test" {
		t.Errorf("task/created_by not copied: %q %q", spec.Task, spec.CreatedBy)
	}
	var payload map[string]any
	if err := json.Unmarshal(spec.Payload, &payload); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if payload["build"] != true {
		t.Errorf("payload = %s, want {\"build\": true}", spec.Payload)
	}
	if len(spec.Requires) != 2 || spec.Requires[0] != "gpu" || spec.Requires[1] != "docker" {
		t.Errorf("requires = %v", spec.Requires)
	}
	if spec.TimeoutS != 120 || spec.Priority != 7 {
		t.Errorf("timeout_s=%d priority=%d: template fields not copied", spec.TimeoutS, spec.Priority)
	}
	if spec.Budget != (wire.Budget{MaxPromptBytes: 4096, MaxOutputTokens: 512, MaxCostUSD: 1.5}) {
		t.Errorf("budget = %+v", spec.Budget)
	}
}

func TestSchedulerNoMatchPending(t *testing.T) {
	s := schedStore(t)
	now := schedClock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	m := &schedMatcher{} // ok=false: no foreman
	sched := NewScheduler(s, m)
	mustCreateSchedule(t, s, "*/1 * * * *", schedTemplate(func(sp *wire.JobSpec) {
		sp.Requires = []string{"gpu"}
	}))

	*now = now.Add(time.Minute)
	if n, err := sched.Pump(ctx); err != nil || n != 1 {
		t.Fatalf("pump = (%d, %v), want (1, nil)", n, err)
	}
	jobs := schedJobs(t, s)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].State != StatePending {
		t.Errorf("state = %s, want pending with no matching foreman", jobs[0].State)
	}
	if jobs[0].ForemanID != "" {
		t.Errorf("foreman_id = %q, want empty", jobs[0].ForemanID)
	}
	if len(m.requires) != 1 || m.requires[0] != "gpu" {
		t.Errorf("matcher requires = %v, want [gpu]", m.requires)
	}

	// Visible via /v1/jobs (the pinned observability path).
	api := s.APIHandler()
	req := httptest.NewRequest("GET", "/v1/jobs?state="+StatePending, nil)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), jobs[0].ID) {
		t.Errorf("GET /v1/jobs?state=pending: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSchedulerDispatch(t *testing.T) {
	s := schedStore(t)
	now := schedClock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	m := &schedMatcher{foreman: registry.Foreman{ID: "foreman:beta"}, ok: true}
	sched := NewScheduler(s, m)
	mustCreateSchedule(t, s, "*/1 * * * *", schedTemplate(func(sp *wire.JobSpec) {
		sp.Requires = []string{"gpu"}
	}))

	*now = now.Add(time.Minute)
	if n, err := sched.Pump(ctx); err != nil || n != 1 {
		t.Fatalf("pump = (%d, %v), want (1, nil)", n, err)
	}
	jobs := schedJobs(t, s)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].State != StateDispatched {
		t.Errorf("state = %s, want dispatched", jobs[0].State)
	}
	if jobs[0].ForemanID != "foreman:beta" {
		t.Errorf("foreman_id = %q, want foreman:beta", jobs[0].ForemanID)
	}
	if m.calls != 1 || len(m.requires) != 1 || m.requires[0] != "gpu" {
		t.Errorf("matcher saw requires = %v (calls %d), want [gpu]", m.requires, m.calls)
	}
}

func TestSchedulerRunStops(t *testing.T) {
	s := schedStore(t)
	now := schedClock()
	s.Now = func() time.Time { return *now }
	sched := NewScheduler(s, &schedMatcher{})
	sched.Tick = 5 * time.Millisecond
	*now = now.Add(time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()
	cancel()
	select {
	case <-done: // returned on cancel; the ticker must not hang the loop
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestSchedulesHandler(t *testing.T) {
	s := schedStore(t)
	now := schedClock()
	s.Now = func() time.Time { return *now }
	h := s.SchedulesHandler()

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Create.
	rec := do("POST", "/v1/schedules", `{"cron_spec": "*/1 * * * *", "job_template": {"task": "hi"}}`)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var sch Schedule
	if err := json.NewDecoder(rec.Body).Decode(&sch); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sch.ID, "sched_") || !sch.Enabled || sch.JobTemplate.Task != "hi" {
		t.Errorf("create response: %+v", sch)
	}
	if sch.JobTemplate.TimeoutS != wire.DefaultTimeoutS {
		t.Errorf("template defaults not applied: %+v", sch.JobTemplate)
	}

	// 400s, with the §10 error model on the first.
	badCases := []struct {
		name string
		body string
	}{
		{"invalid cron", `{"cron_spec": "bogus", "job_template": {"task": "hi"}}`},
		{"missing task", `{"cron_spec": "*/1 * * * *", "job_template": {}}`},
		{"malformed json", `not json`},
	}
	for _, tc := range badCases {
		rec := do("POST", "/v1/schedules", tc.body)
		if rec.Code != 400 {
			t.Errorf("%s: code = %d, want 400", tc.name, rec.Code)
		}
	}
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(do("POST", "/v1/schedules", badCases[0].body).Body.Bytes(), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.Error.Code != "bad_request" || errBody.Error.Message == "" {
		t.Errorf("error model: %+v", errBody)
	}

	// List.
	rec = do("GET", "/v1/schedules", "")
	if rec.Code != 200 {
		t.Fatalf("list: %d", rec.Code)
	}
	var list []Schedule
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != sch.ID {
		t.Errorf("list = %d schedules, want the created one", len(list))
	}

	// Enable/disable round-trip via the API.
	rec = do("POST", "/v1/schedules/"+sch.ID+"/disable", "")
	if rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	var upd Schedule
	if err := json.NewDecoder(rec.Body).Decode(&upd); err != nil {
		t.Fatal(err)
	}
	if upd.Enabled {
		t.Error("disable response still enabled")
	}
	// A disabled schedule must not fire.
	*now = now.Add(5 * time.Minute)
	sched := NewScheduler(s, &schedMatcher{})
	if n, err := sched.Pump(context.Background()); err != nil || n != 0 {
		t.Fatalf("pump while disabled = (%d, %v), want (0, nil)", n, err)
	}
	rec = do("POST", "/v1/schedules/"+sch.ID+"/enable", "")
	if rec.Code != 200 {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(&upd); err != nil {
		t.Fatal(err)
	}
	if !upd.Enabled {
		t.Error("enable response still disabled")
	}

	// 404s.
	for path := range map[string]bool{
		"/v1/schedules/sched_missing/enable":  true,
		"/v1/schedules/sched_missing/disable": true,
	} {
		if rec := do("POST", path, ""); rec.Code != 404 {
			t.Errorf("POST %s: %d, want 404", path, rec.Code)
		}
	}
	if rec := do("DELETE", "/v1/schedules/sched_missing", ""); rec.Code != 404 {
		t.Errorf("delete missing: %d, want 404", rec.Code)
	}

	// Delete.
	if rec := do("DELETE", "/v1/schedules/"+sch.ID, ""); rec.Code != 204 {
		t.Fatalf("delete: %d, want 204", rec.Code)
	}
	list = nil
	if err := json.NewDecoder(do("GET", "/v1/schedules", "").Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("list after delete = %d, want 0", len(list))
	}
}

// TestSchedulerPinnedTemplate (§6.7 foreman pin): a template's foreman pin
// rides every fired run. The pinned run dispatches to the pinned foreman
// when online - the Matcher is never consulted, even though the stub
// matcher would pick a different foreman - and pends when the pin is
// offline.
func TestSchedulerPinnedTemplate(t *testing.T) {
	s := schedStore(t)
	now := schedClock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	m := &schedMatcher{foreman: registry.Foreman{ID: "foreman:alpha"}, ok: true}
	sched := NewScheduler(s, m)

	seed := func(online bool) {
		t.Helper()
		if err := s.UpsertForeman(ctx, "foreman:beta", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		live := NewLiveness(s)
		if online {
			live.OnConnect("foreman:beta")
		} else {
			live.OnDisconnect("foreman:beta")
		}
	}

	seed(true)
	mustCreateSchedule(t, s, "*/1 * * * *", schedTemplate(func(sp *wire.JobSpec) {
		sp.Foreman = "foreman:beta"
		sp.Requires = []string{"gpu"}
	}))
	*now = now.Add(time.Minute)
	if n, err := sched.Pump(ctx); err != nil || n != 1 {
		t.Fatalf("pump = (%d, %v), want (1, nil)", n, err)
	}
	fired := schedJobs(t, s)
	if len(fired) != 1 || fired[0].State != StateDispatched || fired[0].ForemanID != "foreman:beta" {
		t.Fatalf("fired = %+v, want dispatched to the pinned foreman:beta", fired)
	}
	if m.calls != 0 {
		t.Errorf("matcher calls = %d, want 0 - the pin bypasses the Matcher (§6.7)", m.calls)
	}
	if fired[0].Spec.Foreman != "foreman:beta" {
		t.Errorf("fired spec foreman = %q, want the pin to ride the spec", fired[0].Spec.Foreman)
	}

	// Offline pin: the next run pends for the pending picker; still no matcher.
	firstID := fired[0].ID
	seed(false)
	*now = now.Add(2 * time.Minute)
	if n, err := sched.Pump(ctx); err != nil || n != 1 {
		t.Fatalf("pump = (%d, %v), want (1, nil)", n, err)
	}
	all := schedJobs(t, s)
	if len(all) != 2 {
		t.Fatalf("jobs = %d, want 2", len(all))
	}
	var second Job
	for _, j := range all {
		if j.ID != firstID {
			second = j
		}
	}
	if second.ID == "" || second.State != StatePending || second.ForemanID != "" {
		t.Fatalf("second run = %+v, want pending with no foreman while the pin is offline", second)
	}
	if m.calls != 0 {
		t.Errorf("matcher calls = %d, want 0 on the offline pin too", m.calls)
	}
}
