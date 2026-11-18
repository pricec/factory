package jobs

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// retryFailAttempt drives a job through one failed attempt: dispatch ->
// start -> failed result. It does NOT arm the retry (MarkFailedForRetry is
// invoked separately so arming decisions stay observable).
func retryFailAttempt(t *testing.T, s *Store, id, kind string) {
	t.Helper()
	mustDispatch(t, s, id, "foreman:alpha")
	mustApplyEvent(t, s, id, "started")
	mustApplyResult(t, s, wire.JobResult{
		JobID:   id,
		Status:  wire.StatusFailed,
		Summary: "attempt failed",
		Error:   &wire.JobError{Kind: wire.ErrorKind(kind), Message: "boom"},
	})
}

// retryToDead cycles dispatch -> start -> fail -> arm -> pump until the job
// dead-letters, advancing the fake clock past each armed retry.
func retryToDead(t *testing.T, s *Store, now *time.Time, id, kind string) {
	t.Helper()
	ctx := context.Background()
	p := NewRetryPicker(s)
	for i := 0; i < 10; i++ {
		job, ok, err := s.Get(ctx, id)
		if err != nil || !ok {
			t.Fatalf("get %s: ok=%v err=%v", id, ok, err)
		}
		st := job.State
		if st == StateDead {
			return
		}
		if st == StatePending || st == StateFailed {
			mustDispatch(t, s, id, "foreman:alpha")
			st = StateDispatched
		}
		if st == StateDispatched {
			mustApplyEvent(t, s, id, "started")
			st = StateRunning
		}
		if st != StateRunning {
			t.Fatalf("retryToDead: unexpected state %s", st)
		}
		mustApplyResult(t, s, wire.JobResult{
			JobID: id, Status: wire.StatusFailed, Summary: "attempt failed",
			Error: &wire.JobError{Kind: wire.ErrorKind(kind), Message: "boom"},
		})
		if err := s.MarkFailedForRetry(ctx, id, kind); err != nil {
			t.Fatal(err)
		}
		job, _, _ = s.Get(ctx, id)
		if job.State == StateDead {
			return
		}
		due, err := time.Parse(time.RFC3339, job.NextAttempt)
		if err != nil {
			t.Fatalf("next_attempt_at %q: %v", job.NextAttempt, err)
		}
		*now = due.Add(time.Second)
		if n, err := p.Pump(ctx); err != nil {
			t.Fatal(err)
		} else if n != 1 {
			t.Fatalf("pump re-dispatched %d jobs, want 1", n)
		}
	}
	t.Fatal("job never dead-ended")
}

func TestMaxRetries(t *testing.T) {
	cases := []struct {
		kind string
		want int
	}{
		{"validation", 0},
		{"auth", 0},
		{"ceiling", 0},
		{"superseded", 0},
		{"timeout", 1},
		{"crash", 2},
		{"llm", 2},
		{"other", 2},
		{"", 0},
		{"mystery", 0},
	}
	for _, tc := range cases {
		if got := MaxRetries(tc.kind); got != tc.want {
			t.Errorf("MaxRetries(%q) = %d, want %d", tc.kind, got, tc.want)
		}
	}
	// The wire constants must agree with the table.
	for k, want := range map[wire.ErrorKind]int{
		wire.KindValidation: 0, wire.KindTimeout: 1, wire.KindCrash: 2,
		wire.KindLLM: 2, wire.KindOther: 2, wire.KindSuperseded: 0, wire.KindAuth: 0,
	} {
		if got := MaxRetries(string(k)); got != want {
			t.Errorf("MaxRetries(%q) = %d, want %d", k, got, want)
		}
	}
}

func TestBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, 2 * time.Minute},
		{3, 8 * time.Minute},
		{4, 8 * time.Minute},
		{10, 8 * time.Minute},
		{0, 30 * time.Second},
	}
	for _, tc := range cases {
		if got := Backoff(tc.attempt); got != tc.want {
			t.Errorf("Backoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestRetryBackoffArming(t *testing.T) {
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	base := *now
	spec := newSpec(t, nil)
	if err := s.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}

	// Attempt 1 fails (attempts=0): armed at +30s.
	retryFailAttempt(t, s, spec.ID, "crash")
	if err := s.MarkFailedForRetry(ctx, spec.ID, "crash"); err != nil {
		t.Fatal(err)
	}
	job, _, _ := s.Get(ctx, spec.ID)
	if job.State != StateFailed {
		t.Fatalf("state = %s, want failed", job.State)
	}
	if want := base.Add(30 * time.Second).Format(time.RFC3339); job.NextAttempt != want {
		t.Errorf("next_attempt_at = %q, want %q", job.NextAttempt, want)
	}

	// Pump before the due time: not picked.
	p := NewRetryPicker(s)
	if n, err := p.Pump(ctx); err != nil || n != 0 {
		t.Fatalf("pump before due: n=%d err=%v, want 0 nil", n, err)
	}
	if st := state(t, s, spec.ID); st != StateFailed {
		t.Fatalf("armed job left failed state: %s", st)
	}

	// Exactly at the due time: picked (<= boundary), attempts 0->1.
	*now = base.Add(30 * time.Second)
	if n, err := p.Pump(ctx); err != nil || n != 1 {
		t.Fatalf("pump at due: n=%d err=%v, want 1 nil", n, err)
	}
	job, _, _ = s.Get(ctx, spec.ID)
	if job.State != StateDispatched {
		t.Errorf("state = %s, want dispatched", job.State)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", job.Attempts)
	}
	if job.NextAttempt != "" {
		t.Errorf("next_attempt_at = %q, want cleared", job.NextAttempt)
	}
	if job.ForemanID != "foreman:alpha" {
		t.Errorf("foreman_id = %q, want the existing foreman", job.ForemanID)
	}

	// Attempt 2 fails (attempts=1): armed at +2m.
	mustApplyEvent(t, s, spec.ID, "started")
	mustApplyResult(t, s, wire.JobResult{
		JobID: spec.ID, Status: wire.StatusFailed, Summary: "again",
		Error: &wire.JobError{Kind: wire.KindCrash, Message: "boom"},
	})
	if err := s.MarkFailedForRetry(ctx, spec.ID, "crash"); err != nil {
		t.Fatal(err)
	}
	job, _, _ = s.Get(ctx, spec.ID)
	want := base.Add(30 * time.Second).Add(2 * time.Minute).Format(time.RFC3339)
	if job.NextAttempt != want {
		t.Errorf("next_attempt_at = %q, want %q (2m backoff)", job.NextAttempt, want)
	}
}

func TestRetryCrashExhaustion(t *testing.T) {
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	spec := newSpec(t, nil)
	if err := s.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}

	hooks := 0
	hookState := ""
	s.SetOnTerminal(func(id, st string) { hooks++; hookState = st })

	retryToDead(t, s, now, spec.ID, "crash")

	job, _, _ := s.Get(ctx, spec.ID)
	if job.State != StateDead {
		t.Errorf("state = %s, want dead", job.State)
	}
	if job.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (crash allows 2 retries)", job.Attempts)
	}
	if job.NextAttempt != "" {
		t.Errorf("next_attempt_at = %q, want cleared", job.NextAttempt)
	}
	if hooks != 1 || hookState != StateDead {
		t.Errorf("terminal hook fired %d times, last %q; want 1 dead", hooks, hookState)
	}
	var results []json.RawMessage
	if err := json.Unmarshal(job.Results, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Errorf("results = %d, want 3 attempts recorded", len(results))
	}
	// Retries re-dispatch the SAME job id: exactly one job in the store.
	jobs, err := s.List(ctx, "", "", 1000)
	if err != nil || len(jobs) != 1 || jobs[0].ID != spec.ID {
		t.Errorf("store has %d jobs, want just %s", len(jobs), spec.ID)
	}
}

func TestRetryValidationImmediateDead(t *testing.T) {
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	spec := newSpec(t, nil)
	if err := s.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}

	hooks := 0
	hookState := ""
	s.SetOnTerminal(func(id, st string) { hooks++; hookState = st })

	retryFailAttempt(t, s, spec.ID, "validation")
	if err := s.MarkFailedForRetry(ctx, spec.ID, "validation"); err != nil {
		t.Fatal(err)
	}
	job, _, _ := s.Get(ctx, spec.ID)
	if job.State != StateDead {
		t.Errorf("state = %s, want dead (validation never retries)", job.State)
	}
	if job.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", job.Attempts)
	}
	if job.NextAttempt != "" {
		t.Errorf("next_attempt_at = %q, want unset", job.NextAttempt)
	}
	if hooks != 1 || hookState != StateDead {
		t.Errorf("terminal hook fired %d times, last %q; want 1 dead", hooks, hookState)
	}
}

func TestRetryTimeoutDeadAfterOneRetry(t *testing.T) {
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	spec := newSpec(t, nil)
	if err := s.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	var hookStates []string
	s.SetOnTerminal(func(id, st string) { hookStates = append(hookStates, st) })

	retryToDead(t, s, now, spec.ID, "timeout")
	job, _, _ := s.Get(context.Background(), spec.ID)
	if job.State != StateDead {
		t.Errorf("state = %s, want dead", job.State)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (timeout allows 1 retry)", job.Attempts)
	}
	// Only the dead transition is terminal; failed marks must not fire.
	if len(hookStates) != 1 || hookStates[0] != StateDead {
		t.Errorf("hook states = %v, want exactly [dead]", hookStates)
	}
}

func TestRetryNoopOnTerminal(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	// Terminal-idempotency: retry scheduling applied to an already-terminal
	// job is a logged no-op.
	spec := newSpec(t, nil)
	driveTo(t, s, spec, StateDone)

	hooks := 0
	s.SetOnTerminal(func(id, st string) { hooks++ })
	if err := s.MarkFailedForRetry(ctx, spec.ID, "crash"); err != nil {
		t.Fatal(err)
	}
	job, _, _ := s.Get(ctx, spec.ID)
	if job.State != StateDone {
		t.Errorf("state = %s, want done (no-op on terminal)", job.State)
	}
	if job.NextAttempt != "" {
		t.Errorf("next_attempt_at = %q, want unset", job.NextAttempt)
	}
	if hooks != 0 {
		t.Errorf("terminal hook fired %d times on no-op, want 0", hooks)
	}
}

func TestRequeue(t *testing.T) {
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()

	// A dead job with nonzero attempts: crash-kind exhaustion.
	deadSpec := newSpec(t, nil)
	if err := s.Create(ctx, deadSpec); err != nil {
		t.Fatal(err)
	}
	retryToDead(t, s, now, deadSpec.ID, "crash")
	pre, _, _ := s.Get(ctx, deadSpec.ID)
	if pre.State != StateDead || pre.Attempts != 2 {
		t.Fatalf("setup: state=%s attempts=%d, want dead/2", pre.State, pre.Attempts)
	}

	if err := s.Requeue(ctx, deadSpec.ID); err != nil {
		t.Fatal(err)
	}
	job, _, _ := s.Get(ctx, deadSpec.ID)
	if job.State != StatePending {
		t.Errorf("state = %s, want pending", job.State)
	}
	if job.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", job.Attempts)
	}
	if job.NextAttempt != "" {
		t.Errorf("next_attempt_at = %q, want cleared", job.NextAttempt)
	}

	// Requeued job is runnable again.
	mustDispatch(t, s, deadSpec.ID, "foreman:beta")
	if st := state(t, s, deadSpec.ID); st != StateDispatched {
		t.Errorf("requeued job did not dispatch: %s", st)
	}

	// Requeue on running and done jobs is a logged no-op.
	runningSpec := newSpec(t, nil)
	driveTo(t, s, runningSpec, StateRunning)
	if err := s.Requeue(ctx, runningSpec.ID); err != nil {
		t.Fatal(err)
	}
	job, _, _ = s.Get(ctx, runningSpec.ID)
	if job.State != StateRunning || job.Attempts != 0 {
		t.Errorf("running job disturbed by requeue: %+v", job)
	}

	doneSpec := newSpec(t, nil)
	driveTo(t, s, doneSpec, StateDone)
	if err := s.Requeue(ctx, doneSpec.ID); err != nil {
		t.Fatal(err)
	}
	if st := state(t, s, doneSpec.ID); st != StateDone {
		t.Errorf("done job disturbed by requeue: %s", st)
	}
}

func TestPickerPump(t *testing.T) {
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	base := *now

	// armed: failed job with next_attempt_at in the future.
	armed := newSpec(t, nil)
	if err := s.Create(ctx, armed); err != nil {
		t.Fatal(err)
	}
	retryFailAttempt(t, s, armed.ID, "crash")
	if err := s.MarkFailedForRetry(ctx, armed.ID, "crash"); err != nil {
		t.Fatal(err)
	}

	// unarmed: failed job with next_attempt_at NULL (driveTo creates it).
	unarmed := newSpec(t, nil)
	driveTo(t, s, unarmed, StateFailed)

	// running: must never be touched (driveTo creates it).
	runningSpec := newSpec(t, nil)
	driveTo(t, s, runningSpec, StateRunning)

	p := NewRetryPicker(s)

	// Not due yet: nothing picked.
	if n, err := p.Pump(ctx); err != nil || n != 0 {
		t.Fatalf("pump before due: n=%d err=%v, want 0 nil", n, err)
	}
	if st := state(t, s, armed.ID); st != StateFailed {
		t.Fatalf("armed job left failed state: %s", st)
	}

	// Due: picked, same id, attempts incremented, next_attempt_at cleared.
	*now = base.Add(31 * time.Second)
	if n, err := p.Pump(ctx); err != nil || n != 1 {
		t.Fatalf("pump after due: n=%d err=%v, want 1 nil", n, err)
	}
	job, _, _ := s.Get(ctx, armed.ID)
	if job.State != StateDispatched {
		t.Errorf("state = %s, want dispatched", job.State)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", job.Attempts)
	}
	if job.NextAttempt != "" {
		t.Errorf("next_attempt_at = %q, want cleared", job.NextAttempt)
	}
	if job.ForemanID != "foreman:alpha" {
		t.Errorf("foreman_id = %q, want existing foreman", job.ForemanID)
	}

	// Unarmed and running jobs untouched.
	ujob, _, _ := s.Get(ctx, unarmed.ID)
	if ujob.State != StateFailed || ujob.Attempts != 0 || ujob.NextAttempt != "" {
		t.Errorf("unarmed failed job disturbed: %+v", ujob)
	}
	if st := state(t, s, runningSpec.ID); st != StateRunning {
		t.Errorf("running job disturbed: %s", st)
	}

	// Same job id throughout: no new jobs created by retries.
	jobs, err := s.List(ctx, "", "", 1000)
	if err != nil || len(jobs) != 3 {
		t.Errorf("store has %d jobs, want 3 (no new ids)", len(jobs))
	}
}

func TestDeadLetter(t *testing.T) {
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	ctx := context.Background()
	h := s.DeadLetterHandler()

	post := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Empty list is [].
	if rec := get("/v1/deadletter"); rec.Code != 200 || rec.Body.String() != "[]\n" {
		t.Fatalf("empty deadletter: %d %q", rec.Code, rec.Body.String())
	}

	// jobA: dead via validation (immediate dead-letter).
	jobA := newSpec(t, nil)
	if err := s.Create(ctx, jobA); err != nil {
		t.Fatal(err)
	}
	retryFailAttempt(t, s, jobA.ID, "validation")
	if err := s.MarkFailedForRetry(ctx, jobA.ID, "validation"); err != nil {
		t.Fatal(err)
	}

	// jobB: running; jobC: done (driveTo creates them).
	jobB := newSpec(t, nil)
	driveTo(t, s, jobB, StateRunning)
	jobC := newSpec(t, nil)
	driveTo(t, s, jobC, StateDone)

	// jobD: dead via crash exhaustion, created a second later (newer).
	*now = now.Add(time.Second)
	jobD := newSpec(t, nil)
	if err := s.Create(ctx, jobD); err != nil {
		t.Fatal(err)
	}
	retryToDead(t, s, now, jobD.ID, "crash")

	// GET lists only dead jobs, newest first.
	rec := get("/v1/deadletter")
	if rec.Code != 200 {
		t.Fatalf("GET deadletter: %d", rec.Code)
	}
	var dead []Job
	if err := json.NewDecoder(rec.Body).Decode(&dead); err != nil {
		t.Fatal(err)
	}
	if len(dead) != 2 {
		t.Fatalf("deadletter returned %d jobs, want 2 (A, D)", len(dead))
	}
	if dead[0].ID != jobD.ID || dead[1].ID != jobA.ID {
		t.Errorf("order = [%s, %s], want newest first [D, A]", dead[0].ID, dead[1].ID)
	}
	for _, j := range dead {
		if j.LastError == "" || len(j.Results) == 0 {
			t.Errorf("dead job %s missing last_error/results: %+v", j.ID, j)
		}
	}
	if body := rec.Body.String(); strings.Contains(body, jobB.ID) || strings.Contains(body, jobC.ID) {
		t.Errorf("non-dead jobs leaked into deadletter: %s", body)
	}

	// POST requeue: dead -> pending.
	rec = post("/v1/deadletter/" + jobA.ID + "/requeue")
	if rec.Code != 200 {
		t.Fatalf("POST requeue: %d %s", rec.Code, rec.Body.String())
	}
	var reqd Job
	if err := json.Unmarshal(rec.Body.Bytes(), &reqd); err != nil {
		t.Fatal(err)
	}
	if reqd.State != StatePending || reqd.Attempts != 0 {
		t.Errorf("requeue response: state=%s attempts=%d, want pending/0", reqd.State, reqd.Attempts)
	}
	job, _, _ := s.Get(ctx, jobA.ID)
	if job.State != StatePending || job.Attempts != 0 || job.NextAttempt != "" {
		t.Errorf("requeued job: %+v", job)
	}

	// GET now lists only the remaining dead job.
	rec = get("/v1/deadletter")
	var dead2 []Job
	if err := json.NewDecoder(rec.Body).Decode(&dead2); err != nil {
		t.Fatal(err)
	}
	if len(dead2) != 1 || dead2[0].ID != jobD.ID {
		t.Errorf("after requeue, deadletter = %v", dead2)
	}

	// POST on unknown job: 404.
	if rec := post("/v1/deadletter/job_missing/requeue"); rec.Code != 404 {
		t.Errorf("requeue unknown: %d, want 404", rec.Code)
	}

	// POST on a running job: requeue is a logged no-op, job stays running.
	if rec := post("/v1/deadletter/" + jobB.ID + "/requeue"); rec.Code != 200 {
		t.Errorf("requeue running: %d", rec.Code)
	}
	if st := state(t, s, jobB.ID); st != StateRunning {
		t.Errorf("running job requeued into %s", st)
	}

	// Bad limit: 400.
	if rec := get("/v1/deadletter?limit=0"); rec.Code != 400 {
		t.Errorf("limit=0: %d, want 400", rec.Code)
	}
}

// A worker that stands down as a duplicate lands in superseded (terminal),
// is never armed for retry, and the picker never re-dispatches it.
func TestRetrySupersededResultNeverRetried(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	spec := newSpec(t, nil)
	if err := s.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	var hooked string
	s.SetOnTerminal(func(id, st string) { hooked = st })
	retryFailAttempt(t, s, spec.ID, string(wire.KindSuperseded))
	if hooked != StateSuperseded {
		t.Errorf("terminal hook state = %q, want superseded", hooked)
	}
	// The consumer calls this after every failed result; it must be a no-op.
	if err := s.MarkFailedForRetry(ctx, spec.ID, string(wire.KindSuperseded)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	n, err := NewRetryPicker(s).Pump(ctx)
	if err != nil || n != 0 {
		t.Fatalf("Pump = %d, %v; want 0, nil", n, err)
	}
	job, _, _ := s.Get(ctx, spec.ID)
	if job.State != StateSuperseded || job.Attempts != 0 || job.NextAttempt != "" {
		t.Errorf("job = state %s attempts %d next %q; want superseded/0/unset", job.State, job.Attempts, job.NextAttempt)
	}
}

// A transient failure is still armed and re-dispatched after its backoff.
func TestRetryTransientStillRetried(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	spec := newSpec(t, nil)
	if err := s.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	retryFailAttempt(t, s, spec.ID, "crash")
	if err := s.MarkFailedForRetry(ctx, spec.ID, "crash"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if n, err := NewRetryPicker(s).Pump(ctx); err != nil || n != 1 {
		t.Fatalf("Pump = %d, %v; want 1, nil", n, err)
	}
	if job, _, _ := s.Get(ctx, spec.ID); job.State != StateDispatched {
		t.Errorf("state = %s, want dispatched", job.State)
	}
}

// A job cancelled or superseded after its retry was armed is not
// re-dispatched when the timer fires, and a late failed result is ignored.
func TestRetryNotAfterTerminal(t *testing.T) {
	for _, to := range []string{StateCancelled, StateSuperseded} {
		t.Run(to, func(t *testing.T) {
			s := openStore(t)
			now := time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)
			s.Now = func() time.Time { return now }
			ctx := context.Background()
			spec := newSpec(t, nil)
			if err := s.Create(ctx, spec); err != nil {
				t.Fatal(err)
			}
			retryFailAttempt(t, s, spec.ID, "crash")
			if err := s.MarkFailedForRetry(ctx, spec.ID, "crash"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Cancel(ctx, spec.ID, to, "operator"); err != nil {
				t.Fatal(err)
			}
			// Late failed result and retry arming on the terminal job.
			mustApplyResult(t, s, wire.JobResult{
				JobID: spec.ID, Status: wire.StatusFailed, Summary: "late",
				Error: &wire.JobError{Kind: wire.KindOther, Message: "late"},
			})
			if err := s.MarkFailedForRetry(ctx, spec.ID, "other"); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			if n, err := NewRetryPicker(s).Pump(ctx); err != nil || n != 0 {
				t.Fatalf("Pump = %d, %v; want 0, nil", n, err)
			}
			if job, _, _ := s.Get(ctx, spec.ID); job.State != to {
				t.Errorf("state = %s, want %s", job.State, to)
			}
		})
	}
}

func TestRetryKind(t *testing.T) {
	cases := []struct {
		name, kind, reason, msg string
		wantKey                 string
		wantRetries             int
	}{
		{"auth crash (job_01M7NTAN79MMJ144Q5H90N83AK)", "crash", "", "Not logged in \u00b7 Please run /login", "auth", 0},
		{"auth kind", "auth", "", "Not logged in", "auth", 0},
		{"ceiling timeout (job_01M7NHGG04184GYVM2G1RSHDV4)", "timeout", "ceiling", "job exceeded timeout_s=7200 ceiling; process group killed", "ceiling", 0},
		{"idle timeout still retried", "timeout", "idle", "no output for idle_timeout_s=600", "timeout", 1},
		{"budget", "other", "budget", "spend cap reached", "budget", 0},
		{"loop", "other", "loop", "loop guard", "loop", 0},
		{"invalid api key", "other", "", "401 Unauthorized: invalid API key", "auth", 0},
		{"unknown model", "llm", "", "unknown model \"foo/bar\"", "validation", 0},
		{"missing CLI", "crash", "", "exec: \"claude\": executable file not found in $PATH", "validation", 0},
		{"network crash retried", "crash", "", "dial tcp: connection reset by peer", "crash", 2},
		{"rate limit retried", "llm", "", "429 rate limit exceeded", "llm", 2},
		{"foreman disconnect retried", "crash", "", "foreman disconnected", "crash", 2},
		{"validation stays", "validation", "", "bad spec", "validation", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := RetryKind(tc.kind, tc.reason, tc.msg)
			if key != tc.wantKey {
				t.Errorf("RetryKind = %q, want %q", key, tc.wantKey)
			}
			if got := MaxRetries(key); got != tc.wantRetries {
				t.Errorf("MaxRetries(%q) = %d, want %d", key, got, tc.wantRetries)
			}
		})
	}
}
