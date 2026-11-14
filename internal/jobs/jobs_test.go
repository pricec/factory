package jobs

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open jobs store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// clock returns an injectable store clock starting at a fixed base.
func clock() (now *time.Time) {
	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	return &base
}

func newSpec(t *testing.T, mutate func(*wire.JobSpec)) wire.JobSpec {
	t.Helper()
	spec := wire.NewJobSpec("do the thing", "test")
	if mutate != nil {
		mutate(&spec)
	}
	return spec
}

// driveTo moves a freshly created job into the given state via its legal
// path; used to set up transition-table cases.
func driveTo(t *testing.T, s *Store, spec wire.JobSpec, target string) {
	t.Helper()
	if err := s.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	switch target {
	case StatePending:
		return
	case StateDispatched:
		mustDispatch(t, s, spec.ID, "foreman:alpha")
		return
	case StateRunning:
		mustDispatch(t, s, spec.ID, "foreman:alpha")
		mustApplyEvent(t, s, spec.ID, "started")
		return
	case StateDone:
		mustDispatch(t, s, spec.ID, "foreman:alpha")
		mustApplyEvent(t, s, spec.ID, "started")
		mustApplyResult(t, s, wire.JobResult{JobID: spec.ID, Status: wire.StatusDone, Summary: "ok"})
		return
	case StateFailed:
		mustDispatch(t, s, spec.ID, "foreman:alpha")
		mustApplyEvent(t, s, spec.ID, "started")
		mustApplyResult(t, s, wire.JobResult{
			JobID: spec.ID, Status: wire.StatusFailed,
			Summary: "no", Error: &wire.JobError{Kind: wire.KindOther, Message: "boom"},
		})
		return
	}
	t.Fatalf("driveTo: cannot reach state %q", target)
}

func mustApplyResult(t *testing.T, s *Store, res wire.JobResult) {
	t.Helper()
	if err := s.ApplyResult(context.Background(), res); err != nil {
		t.Fatal(err)
	}
}

func mustDispatch(t *testing.T, s *Store, id, foreman string) {
	t.Helper()
	if err := s.Dispatch(context.Background(), id, foreman); err != nil {
		t.Fatal(err)
	}
}

func mustApplyEvent(t *testing.T, s *Store, id, event string) {
	t.Helper()
	if err := s.ApplyEvent(context.Background(), id, event); err != nil {
		t.Fatal(err)
	}
}

func state(t *testing.T, s *Store, id string) string {
	t.Helper()
	job, ok, err := s.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("get %s: ok=%v err=%v", id, ok, err)
	}
	return job.State
}

// TestTransitionTable exhaustively asserts every (from,to) pair against the
// pinned legal map (spec §6.1).
func TestTransitionTable(t *testing.T) {
	reachable := map[string]bool{
		StatePending: true, StateDispatched: true, StateRunning: true,
		StateDone: true, StateFailed: true,
	}

	for _, from := range AllStates {
		if !reachable[from] {
			continue // timed_out and dead are only reached via sweeper/#10
		}
		for _, to := range AllStates {
			if from == to {
				continue // self-pairs are tautologies
			}
			want := legal[from][to]

			s := openStore(t)
			spec := newSpec(t, nil)
			driveTo(t, s, spec, from)

			// Attempt the transition through the owning method.
			ctx := context.Background()
			var err error
			switch to {
			case StateDispatched:
				err = s.Dispatch(ctx, spec.ID, "foreman:alpha")
			case StateRunning:
				err = s.ApplyEvent(ctx, spec.ID, "started")
			case StateDone:
				err = s.ApplyResult(ctx, wire.JobResult{JobID: spec.ID, Status: wire.StatusDone, Summary: "ok"})
			case StateFailed:
				err = s.ApplyResult(ctx, wire.JobResult{
					JobID: spec.ID, Status: wire.StatusFailed,
					Summary: "no", Error: &wire.JobError{Kind: wire.KindOther, Message: "x"},
				})
			case StateTimedOut:
				// Only via sweeper; not driven here (see sweeper tests).
				continue
			case StateDead:
				err = s.MarkDead(ctx, spec.ID)
			case StatePending:
				continue // no method targets pending
			}
			if err != nil {
				t.Fatalf("%s -> %s: %v", from, to, err)
			}
			got := state(t, s, spec.ID)
			if want && got != to {
				t.Errorf("legal %s -> %s did not apply; state = %s", from, to, got)
			}
			if !want && got == to {
				t.Errorf("illegal %s -> %s was applied", from, to)
			}
		}
	}
}

func TestApplyResultTerminalIdempotent(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	spec := newSpec(t, nil)
	driveTo(t, s, spec, StateRunning)

	terminals := 0
	s.SetOnTerminal(func(jobID, finalState string) { terminals++ })

	res := wire.JobResult{
		JobID:   spec.ID,
		Status:  wire.StatusDone,
		Summary: "all done",
		Usage:   &wire.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.02},
		Result:  json.RawMessage(`{"answer":42}`),
	}
	if err := s.ApplyResult(ctx, res); err != nil {
		t.Fatal(err)
	}
	// Duplicate result: no-op, no second terminal fire.
	if err := s.ApplyResult(ctx, res); err != nil {
		t.Fatal(err)
	}
	if terminals != 1 {
		t.Errorf("terminal hook fired %d times, want 1", terminals)
	}
	job, _, _ := s.Get(ctx, spec.ID)
	if job.State != StateDone {
		t.Errorf("state = %s, want done", job.State)
	}
	var results []wire.JobResult
	if err := json.Unmarshal(job.Results, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Errorf("results = %d, want 1 (duplicate not appended)", len(results))
	}
	var usage wire.Usage
	if err := json.Unmarshal(job.Usage, &usage); err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 10 {
		t.Errorf("usage not recorded: %+v", usage)
	}
}

func TestApplyResultUsageLastWins(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	spec := newSpec(t, nil)
	driveTo(t, s, spec, StateRunning)

	s.SetOnTerminal(func(jobID, st string) {})
	mustApplyEvent(t, s, spec.ID, "started") // no-op; already running

	first := wire.JobResult{
		JobID: spec.ID, Status: wire.StatusFailed, Summary: "try 1",
		Error: &wire.JobError{Kind: wire.KindCrash, Message: "x"},
		Usage: &wire.Usage{CostUSD: 1.0},
	}
	if err := s.ApplyResult(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Retry (failed -> dispatched -> running), second result with new usage.
	mustDispatch(t, s, spec.ID, "foreman:alpha")
	mustApplyEvent(t, s, spec.ID, "started")
	second := wire.JobResult{
		JobID: spec.ID, Status: wire.StatusDone, Summary: "try 2",
		Usage: &wire.Usage{CostUSD: 2.0},
	}
	if err := s.ApplyResult(ctx, second); err != nil {
		t.Fatal(err)
	}
	job, _, _ := s.Get(ctx, spec.ID)
	var usage wire.Usage
	if err := json.Unmarshal(job.Usage, &usage); err != nil {
		t.Fatal(err)
	}
	if usage.CostUSD != 2.0 {
		t.Errorf("usage = %+v, want last-wins 2.0", usage)
	}
	var results []wire.JobResult
	if err := json.Unmarshal(job.Results, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2 attempts recorded", len(results))
	}
	if job.Attempts != 0 {
		t.Errorf("attempts = %d; #10 owns attempt counting", job.Attempts)
	}
}

func TestSweeperTimeoutBoundary(t *testing.T) {
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }

	ctx := context.Background()
	spec := newSpec(t, func(sp *wire.JobSpec) { sp.TimeoutS = 300 })
	driveTo(t, s, spec, StateRunning)
	terminals := 0
	s.SetOnTerminal(func(jobID, st string) {
		terminals++
		if st != StateTimedOut {
			t.Errorf("terminal state = %q, want timed_out", st)
		}
	})
	sw := NewSweeper(s)

	// 59s past timeout: still running.
	*now = now.Add(300*time.Second + 59*time.Second)
	if err := sw.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, s, spec.ID); st != StateRunning {
		t.Fatalf("state = %s, want running at timeout+59s", st)
	}
	// 61s past timeout: timed_out, hook fired exactly once.
	*now = now.Add(2 * time.Second)
	if err := sw.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, s, spec.ID); st != StateTimedOut {
		t.Fatalf("state = %s, want timed_out at timeout+61s", st)
	}
	// Re-pump: terminal already applied, no double fire.
	if err := sw.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if terminals != 1 {
		t.Errorf("terminal fired %d times, want 1", terminals)
	}
}

func TestLivenessAndOfflineMarking(t *testing.T) {
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()

	if err := s.UpsertForeman(ctx, "foreman:alpha", json.RawMessage(`{"gpu":"true"}`)); err != nil {
		t.Fatal(err)
	}
	live := NewLiveness(s)
	live.OnActivity("foreman:alpha", *now)

	// Fresh activity: online.
	sw := NewSweeper(s)
	if err := sw.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	foremen, err := s.Foremen(ctx)
	if err != nil || len(foremen) != 1 {
		t.Fatalf("foremen: %v (%v)", foremen, err)
	}
	if !foremen[0].Online {
		t.Error("foreman should be online after activity")
	}
	if foremen[0].Capabilities != `{"gpu":"true"}` {
		t.Errorf("capabilities = %q", foremen[0].Capabilities)
	}

	// 44s silent: still online.
	*now = now.Add(44 * time.Second)
	_ = sw.Pump(ctx)
	foremen, _ = s.Foremen(ctx)
	if !foremen[0].Online {
		t.Error("foreman marked offline at 44s; threshold is 45s")
	}
	// 46s silent: offline.
	*now = now.Add(2 * time.Second)
	_ = sw.Pump(ctx)
	foremen, _ = s.Foremen(ctx)
	if foremen[0].Online {
		t.Error("foreman still online at 46s; threshold is 45s")
	}
	// OnDisconnect marks offline immediately; OnActivity brings it back.
	live.OnDisconnect("foreman:alpha")
	foremen, _ = s.Foremen(ctx)
	if foremen[0].Online {
		t.Error("OnDisconnect did not mark offline")
	}
	live.OnConnect("foreman:alpha")
	foremen, _ = s.Foremen(ctx)
	if !foremen[0].Online {
		t.Error("OnConnect did not mark online")
	}
}

func TestRestartPreservesState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	spec := newSpec(t, nil)
	driveTo(t, s, spec, StateRunning)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if st := state(t, s2, spec.ID); st != StateRunning {
		t.Fatalf("state after restart = %s, want running", st)
	}
}

func TestAPIFiltersAndErrors(t *testing.T) {
	s := openStore(t)
	specA := newSpec(t, nil)
	driveTo(t, s, specA, StateRunning) // foreman:alpha, running
	specB := newSpec(t, nil)
	driveTo(t, s, specB, StateDone) // foreman:alpha, done

	h := s.APIHandler()

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/v1/jobs?state=running")
	if rec.Code != 200 {
		t.Fatalf("state filter: %d", rec.Code)
	}
	var jobs []Job
	if err := json.NewDecoder(rec.Body).Decode(&jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != specA.ID {
		t.Errorf("state=running returned %d jobs", len(jobs))
	}

	rec = get("/v1/jobs?foreman=foreman:alpha")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), specB.ID) {
		t.Errorf("foreman filter missing done job: %d %s", rec.Code, rec.Body.String())
	}

	rec = get("/v1/jobs?state=bogus")
	if rec.Code != 400 {
		t.Errorf("bad state filter: %d, want 400", rec.Code)
	}

	rec = get("/v1/jobs/" + specA.ID)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"running"`) {
		t.Errorf("get job: %d %s", rec.Code, rec.Body.String())
	}

	rec = get("/v1/jobs/job_missing")
	if rec.Code != 404 {
		t.Errorf("missing job: %d, want 404", rec.Code)
	}

	rec = get("/v1/foremen")
	if rec.Code != 200 {
		t.Errorf("foremen: %d", rec.Code)
	}
}
