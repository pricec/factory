package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// TestCancelFromActiveStates (table-driven): every non-terminal state moves
// to cancelled or superseded as a real transition, firing the terminal hook
// exactly once and recording the reason (spec §6.1, 2026-11-15 amendment).
func TestCancelFromActiveStates(t *testing.T) {
	for _, from := range []string{StatePending, StateDispatched, StateRunning, StateFailed} {
		for _, to := range []string{StateCancelled, StateSuperseded} {
			t.Run(from+"->"+to, func(t *testing.T) {
				s := openStore(t)
				spec := newSpec(t, nil)
				driveTo(t, s, spec, from)

				fired := 0
				var firedState string
				s.SetOnTerminal(func(id, st string) { fired++; firedState = st })

				job, err := s.Cancel(context.Background(), spec.ID, to, "test: no longer needed")
				if err != nil {
					t.Fatalf("Cancel: %v", err)
				}
				if job.State != to {
					t.Errorf("state = %s, want %s", job.State, to)
				}
				if got := state(t, s, spec.ID); got != to {
					t.Errorf("persisted state = %s, want %s", got, to)
				}
				if !Terminal(to) {
					t.Errorf("Terminal(%s) = false, want true", to)
				}
				if fired != 1 || firedState != to {
					t.Errorf("terminal hook fired %d times (state %q), want 1 (%s)", fired, firedState, to)
				}
				if job.LastError != to+": test: no longer needed" {
					t.Errorf("last_error = %q, want reason recorded", job.LastError)
				}
				// A superseded/cancelled job never re-enters the pipeline.
				if err := s.Dispatch(context.Background(), spec.ID, "foreman:alpha"); err != nil {
					t.Fatal(err)
				}
				if got := state(t, s, spec.ID); got != to {
					t.Errorf("post-cancel dispatch moved state to %s, want %s (terminal)", got, to)
				}
			})
		}
	}
}

// TestCancelTerminalRejected: cancel/supersede of a terminal job is
// rejected with ErrTerminal and leaves the state untouched.
func TestCancelTerminalRejected(t *testing.T) {
	for _, from := range []string{StateDone, StateTimedOut, StateDead} {
		t.Run(from, func(t *testing.T) {
			s := openStore(t)
			spec := newSpec(t, nil)

			hooks := 0
			s.SetOnTerminal(func(id, st string) { hooks++ })

			ctx := context.Background()
			switch from {
			case StateDone:
				driveTo(t, s, spec, StateDone)
			case StateTimedOut:
				driveTo(t, s, spec, StateRunning)
				// Sweep past the timeout deadline (spec §6.2).
				base := s.Now()
				s.Now = func() time.Time {
					return base.Add(time.Duration(spec.TimeoutS)*time.Second + timeoutGrace + time.Second)
				}
				sw := NewSweeper(s)
				if err := sw.Pump(ctx); err != nil {
					t.Fatal(err)
				}
			case StateDead:
				// Dead is only reachable via exhausted retries (#10):
				// validation kind = 0 retries dead-letters immediately.
				driveTo(t, s, spec, StateFailed)
				if err := s.MarkFailedForRetry(ctx, spec.ID, string(wire.KindValidation)); err != nil {
					t.Fatal(err)
				}
			}
			if got := state(t, s, spec.ID); got != from {
				t.Fatalf("setup: state = %s, want %s", got, from)
			}
			hooks = 0 // only the setup transition's fire matters below

			for _, to := range []string{StateCancelled, StateSuperseded} {
				if _, err := s.Cancel(ctx, spec.ID, to, "late cancel"); !errors.Is(err, ErrTerminal) {
					t.Errorf("Cancel %s of %s job: err = %v, want ErrTerminal", to, from, err)
				}
			}
			if got := state(t, s, spec.ID); got != from {
				t.Errorf("state = %s, want %s (rejection left it untouched)", got, from)
			}
			if hooks != 0 {
				t.Errorf("terminal hook fired %d times on rejection, want 0", hooks)
			}
		})
	}
}

// TestCancelLateResultIgnored: a worker result arriving after a cancel is a
// terminal-idempotent no-op (spec §6.4) - state stays cancelled, no second
// terminal fire, nothing appended.
func TestCancelLateResultIgnored(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	spec := newSpec(t, nil)
	driveTo(t, s, spec, StateRunning)

	fired := 0
	s.SetOnTerminal(func(id, st string) { fired++ })

	if _, err := s.Cancel(ctx, spec.ID, StateCancelled, "duplicate"); err != nil {
		t.Fatal(err)
	}
	mustApplyResult(t, s, wire.JobResult{JobID: spec.ID, Status: wire.StatusDone, Summary: "late"})
	mustApplyEvent(t, s, spec.ID, "started") // also a no-op

	if got := state(t, s, spec.ID); got != StateCancelled {
		t.Errorf("state = %s, want cancelled (late result ignored)", got)
	}
	job, _, err := s.Get(ctx, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	var results []wire.JobResult
	if err := json.Unmarshal(job.Results, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d entries, want 0 (late result not recorded)", len(results))
	}
	if fired != 1 {
		t.Errorf("terminal hook fired %d times, want 1", fired)
	}
}

// TestCancelFailedKillsRetry: cancelling a failed job clears its armed
// retry so the retry picker never re-dispatches it (spec §6.1, §6.4).
func TestCancelFailedKillsRetry(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	spec := newSpec(t, nil)
	driveTo(t, s, spec, StateFailed) // attempts=0, kind other -> 2 retries
	if err := s.MarkFailedForRetry(ctx, spec.ID, string(wire.KindOther)); err != nil {
		t.Fatal(err)
	}
	job, _, err := s.Get(ctx, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.NextAttempt == "" {
		t.Fatalf("retry not armed (next_attempt_at empty)")
	}

	if _, err := s.Cancel(ctx, spec.ID, StateCancelled, "superseded by refile"); err != nil {
		t.Fatal(err)
	}
	job, _, _ = s.Get(ctx, spec.ID)
	if job.NextAttempt != "" {
		t.Errorf("next_attempt_at = %q, want cleared", job.NextAttempt)
	}
	picker := NewRetryPicker(s)
	n, err := picker.Pump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("retry picker re-dispatched %d cancelled job(s), want 0", n)
	}
	if got := state(t, s, spec.ID); got != StateCancelled {
		t.Errorf("state = %s, want cancelled", got)
	}
}

// TestCancelErrors: unknown jobs and non-cancel targets are errors.
func TestCancelErrors(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	if _, err := s.Cancel(ctx, "job_missing", StateCancelled, ""); !errors.Is(err, ErrNoSuchJob) {
		t.Errorf("unknown job: err = %v, want ErrNoSuchJob", err)
	}
	if _, err := s.Cancel(ctx, "job_missing", StateCancelled, ""); !strings.Contains(err.Error(), "job_missing") {
		t.Errorf("unknown job error should name the id, got %q", err.Error())
	}
	spec := newSpec(t, nil)
	driveTo(t, s, spec, StatePending)
	if _, err := s.Cancel(ctx, spec.ID, StateDone, ""); err == nil || !strings.Contains(err.Error(), "cancelled or superseded") {
		t.Errorf("bad target: err = %v, want target-validation error", err)
	}
	if got := state(t, s, spec.ID); got != StatePending {
		t.Errorf("state = %s, want pending (bad target changed nothing)", got)
	}
}

// TestCancelAPI exercises POST /v1/jobs/{id}/cancel (spec §6.9).
func TestCancelAPI(t *testing.T) {
	s := openStore(t)
	h := s.APIHandler()

	cancel := func(id, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/jobs/"+id+"/cancel", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	seed := func() string {
		spec := newSpec(t, nil)
		if err := s.Create(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		return spec.ID
	}

	t.Run("pending default cancel", func(t *testing.T) {
		id := seed()
		rec := cancel(id, "") // empty body: plain cancel
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"cancelled"`) {
			t.Fatalf("empty-body cancel: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("pending superseded with reason", func(t *testing.T) {
		id := seed()
		rec := cancel(id, `{"reason":"duplicate of job_X","terminal_state":"superseded"}`)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"superseded"`) {
			t.Fatalf("supersede: %d %s", rec.Code, rec.Body.String())
		}
		var job Job
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(job.LastError, "duplicate of job_X") {
			t.Errorf("last_error = %q, want the reason", job.LastError)
		}
	})
	t.Run("running cancel allowed", func(t *testing.T) {
		id := seed()
		mustDispatch(t, s, id, "foreman:alpha")
		mustApplyEvent(t, s, id, "started")
		rec := cancel(id, `{"reason":"runaway"}`)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"cancelled"`) {
			t.Fatalf("running cancel: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("terminal rejected 409", func(t *testing.T) {
		id := seed()
		mustDispatch(t, s, id, "foreman:alpha")
		mustApplyEvent(t, s, id, "started")
		mustApplyResult(t, s, wire.JobResult{JobID: id, Status: wire.StatusDone, Summary: "ok"})
		rec := cancel(id, "")
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "already_terminal") {
			t.Fatalf("terminal cancel: %d %s, want 409 already_terminal", rec.Code, rec.Body.String())
		}
	})
	t.Run("unknown 404", func(t *testing.T) {
		if rec := cancel("job_missing", ""); rec.Code != 404 {
			t.Fatalf("unknown job: %d, want 404", rec.Code)
		}
	})
	t.Run("bad bodies 400", func(t *testing.T) {
		id := seed()
		if rec := cancel(id, `{not json`); rec.Code != 400 {
			t.Errorf("invalid JSON: %d, want 400", rec.Code)
		}
		if rec := cancel(id, `{"terminal_state":"done"}`); rec.Code != 400 {
			t.Errorf("bad terminal_state: %d, want 400", rec.Code)
		}
	})
	t.Run("state filter accepts new states", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/jobs?state=cancelled", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("state=cancelled filter: %d %s, want 200", rec.Code, rec.Body.String())
		}
	})
}

// TestCancelHookOnlyForLiveJobs: the onCancelled hook (worker kill) fires for
// dispatched and running jobs, never for pending ones, once per cancel.
func TestCancelHookOnlyForLiveJobs(t *testing.T) {
	for _, tc := range []struct {
		from string
		want int
	}{{StatePending, 0}, {StateDispatched, 1}, {StateRunning, 1}, {StateFailed, 0}} {
		t.Run(tc.from, func(t *testing.T) {
			s := openStore(t)
			spec := newSpec(t, nil)
			driveTo(t, s, spec, tc.from)
			var got []Job
			s.SetOnCancelled(func(j Job) { got = append(got, j) })
			if _, err := s.Cancel(context.Background(), spec.ID, StateSuperseded, "dup"); err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("hook fired %d times, want %d", len(got), tc.want)
			}
			if tc.want == 1 && (got[0].State != StateSuperseded || got[0].ForemanID == "") {
				t.Errorf("hook job = %+v, want superseded with foreman", got[0])
			}
		})
	}
}
