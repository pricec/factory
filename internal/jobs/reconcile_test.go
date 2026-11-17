package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

func failedResult(id, kind string) wire.JobResult {
	r := wire.JobResult{JobID: id, Status: wire.StatusFailed, Summary: "x"}
	if kind != "" {
		r.Error = &wire.JobError{Kind: wire.ErrorKind(kind), Message: "boom"}
	}
	return r
}

// A failed final result ends in a terminal state or an armed retry, even
// when the started event never arrived (result in dispatched) or the
// retry arming never ran (stranded failed).
func TestFailedFinalResultReachesTerminalOrRetry(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		started bool
		kind    string
		arm     bool
		want    string
		retry   bool
	}{
		{"validation exhausted -> dead", true, "validation", true, StateDead, false},
		{"other -> retry armed", true, "other", true, StateFailed, true},
		{"stranded, kind other -> retry armed by sweeper", true, "other", false, StateFailed, true},
		{"stranded, no error kind -> dead by sweeper", true, "", false, StateDead, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			now := clock()
			s.Now = func() time.Time { return *now }
			spec := newSpec(t, nil)
			if err := s.Create(ctx, spec); err != nil {
				t.Fatal(err)
			}
			mustDispatch(t, s, spec.ID, "foreman:alpha")
			if tc.started {
				mustApplyEvent(t, s, spec.ID, "started")
			}
			mustApplyResult(t, s, failedResult(spec.ID, tc.kind))
			if st := state(t, s, spec.ID); st == StateRunning || st == StateDispatched {
				t.Fatalf("state after failed result = %s, want non-active", st)
			}
			if tc.arm {
				if err := s.MarkFailedForRetry(ctx, spec.ID, tc.kind); err != nil {
					t.Fatal(err)
				}
			} else {
				*now = now.Add(3 * time.Minute)
				if err := NewSweeper(s).Pump(ctx); err != nil {
					t.Fatal(err)
				}
			}
			job, _, _ := s.Get(ctx, spec.ID)
			if job.State != tc.want {
				t.Fatalf("state = %s, want %s", job.State, tc.want)
			}
			if got := job.NextAttempt != ""; got != tc.retry {
				t.Fatalf("retry armed = %v, want %v", got, tc.retry)
			}
		})
	}
}

// A foreman that re-registers without claiming a job the server thinks it
// is running gets that job reconciled: retried, or dead when a final
// result for the attempt already exists. Claimed and fresh jobs stay.
func TestReconcileForemanUnclaimedRunning(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }

	mk := func() string {
		spec := newSpec(t, nil)
		driveTo(t, s, spec, StateRunning)
		return spec.ID
	}
	lost, claimed, withFinal, other := mk(), mk(), mk(), mk()
	// withFinal: a final result recorded for the current attempt (attempts
	// = 0, results = 1) while the job is still marked running.
	if _, err := s.db.Exec(`UPDATE jobs SET results = '[{"status":"failed"}]' WHERE id = ?`, withFinal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET foreman_id = 'foreman:other' WHERE id = ?`, other); err != nil {
		t.Fatal(err)
	}

	// Inside the grace window nothing is believed lost.
	if got, err := s.ReconcileForeman(ctx, "foreman:alpha", nil); err != nil || len(got) != 0 {
		t.Fatalf("reconcile inside grace = %v, %v; want none", got, err)
	}

	*now = now.Add(3 * time.Minute)
	got, err := s.ReconcileForeman(ctx, "foreman:alpha", []string{claimed})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("reaped = %v, want lost and withFinal", got)
	}
	lostJob, _, _ := s.Get(ctx, lost)
	if lostJob.State != StateFailed || lostJob.NextAttempt == "" {
		t.Fatalf("lost: state=%s next=%q, want failed with retry armed", lostJob.State, lostJob.NextAttempt)
	}
	if st := state(t, s, withFinal); st != StateDead {
		t.Fatalf("withFinal state = %s, want dead", st)
	}
	if st := state(t, s, claimed); st != StateRunning {
		t.Fatalf("claimed state = %s, want running", st)
	}
	if st := state(t, s, other); st != StateRunning {
		t.Fatalf("other foreman's job state = %s, want running", st)
	}
}

// A running job on a foreman offline past the grace is reaped; before the
// grace it is left alone.
func TestSweeperReapsRunningOnOfflineForeman(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	spec := newSpec(t, func(sp *wire.JobSpec) { sp.TimeoutS = 14400 })
	driveTo(t, s, spec, StateRunning)
	if err := s.UpsertForeman(ctx, "foreman:alpha", nil); err != nil {
		t.Fatal(err)
	}
	sw := NewSweeper(s)

	*now = now.Add(5 * time.Minute) // offline, inside grace
	if err := sw.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, s, spec.ID); st != StateRunning {
		t.Fatalf("state = %s, want running inside offline grace", st)
	}
	*now = now.Add(6 * time.Minute)
	if err := sw.Pump(ctx); err != nil {
		t.Fatal(err)
	}
	if st := state(t, s, spec.ID); st != StateFailed {
		t.Fatalf("state = %s, want failed (retry armed) past offline grace", st)
	}
}

// timeout_s is the server-side backstop for a running job whose worker
// went quiet while its foreman stays online and claims it.
func TestSweeperTimeoutReapsQuietRunningJob(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	spec := newSpec(t, func(sp *wire.JobSpec) { sp.TimeoutS = 600 })
	driveTo(t, s, spec, StateRunning)
	sw := NewSweeper(s)
	for _, d := range []time.Duration{2 * time.Minute, 10 * time.Minute} {
		*now = now.Add(d)
		if err := s.UpsertForeman(ctx, "foreman:alpha", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReconcileForeman(ctx, "foreman:alpha", []string{spec.ID}); err != nil {
			t.Fatal(err)
		}
		if err := sw.Pump(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if st := state(t, s, spec.ID); st != StateTimedOut {
		t.Fatalf("state = %s, want timed_out", st)
	}
}
