package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// spendSetup opens a store on the shared fake clock (base 2026-11-14 UTC).
func spendSetup(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s := openStore(t)
	now := clock()
	s.Now = func() time.Time { return *now }
	return s, now
}

// spendJob creates a job under the store clock and drives it through the
// legal path - create, dispatch, started, done result - recording costUSD
// of usage (spec §6.1, §6.5).
func spendJob(t *testing.T, s *Store, costUSD float64) wire.JobSpec {
	t.Helper()
	spec := newSpec(t, nil)
	driveTo(t, s, spec, StateRunning)
	mustApplyResult(t, s, wire.JobResult{
		JobID:   spec.ID,
		Status:  wire.StatusDone,
		Summary: "done",
		Usage:   &wire.Usage{CostUSD: costUSD},
	})
	return spec
}

func TestSpendDailySpend(t *testing.T) {
	s, now := spendSetup(t)
	ctx := context.Background()
	base := *now

	// Yesterday: one completed job costing 2.50.
	*now = base.AddDate(0, 0, -1)
	spendJob(t, s, 2.50)

	// Today: one completed job costing 1.25 plus one pending job (created,
	// no usage recorded - counts, contributes zero).
	*now = base
	spendJob(t, s, 1.25)
	pending := newSpec(t, nil)
	if err := s.Create(ctx, pending); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		day  string
		want SpendTotals
	}{
		{"2026-11-14", SpendTotals{Total: 1.25, Jobs: 2}},
		{"2026-11-13", SpendTotals{Total: 2.50, Jobs: 1}},
		{"2026-11-01", SpendTotals{}},
	}
	for _, c := range cases {
		got, err := s.DailySpend(ctx, c.day)
		if err != nil {
			t.Fatalf("day %s: %v", c.day, err)
		}
		if got != c.want {
			t.Errorf("day %s: got %+v, want %+v", c.day, got, c.want)
		}
	}
}

func TestOverBudgetMark(t *testing.T) {
	s, _ := spendSetup(t)
	ctx := context.Background()
	spec := newSpec(t, nil)
	driveTo(t, s, spec, StateDone)

	if err := s.MarkOverBudget(ctx, spec.ID); err != nil {
		t.Fatal(err)
	}
	job, ok, err := s.Get(ctx, spec.ID)
	if err != nil || !ok {
		t.Fatalf("get %s: ok=%v err=%v", spec.ID, ok, err)
	}
	if !job.OverBudget {
		t.Error("over_budget not set")
	}
	if job.State != StateDone {
		t.Errorf("state changed to %s; MarkOverBudget must not transition", job.State)
	}

	// Idempotent: flagging again is fine.
	if err := s.MarkOverBudget(ctx, spec.ID); err != nil {
		t.Fatal(err)
	}

	// Guarded: a missing job is an error.
	if err := s.MarkOverBudget(ctx, "job_missing"); err == nil {
		t.Error("missing job: want error")
	}
}

func TestSpendHandler(t *testing.T) {
	s, _ := spendSetup(t)
	spendJob(t, s, 1.25)

	h := s.SpendHandler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	wantTotals := func(t *testing.T, rec *httptest.ResponseRecorder, want SpendTotals) {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var got SpendTotals
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("totals = %+v, want %+v", got, want)
		}
	}

	// Explicit day.
	wantTotals(t, get("/v1/spend?day=2026-11-14"), SpendTotals{Total: 1.25, Jobs: 1})
	// Default day: the store clock's date.
	wantTotals(t, get("/v1/spend"), SpendTotals{Total: 1.25, Jobs: 1})
	// A day with no jobs at all.
	wantTotals(t, get("/v1/spend?day=2026-10-01"), SpendTotals{})

	// Malformed day: 400 with the normative error model.
	rec := get("/v1/spend?day=2026-13-99")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_request") {
		t.Errorf("malformed day: %d %s", rec.Code, rec.Body.String())
	}
}
