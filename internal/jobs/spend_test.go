package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
		if !reflect.DeepEqual(got, c.want) {
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
		if !reflect.DeepEqual(got, want) {
			t.Errorf("totals = %+v, want %+v", got, want)
		}
	}

	// The additive session-ledger fields: one zero row per pinned role
	// on a day with no recorded sessions (spec §6.5 reporting
	// amendment), summed into session_total_usd.
	zeroRoles := []RoleTotals{
		{Role: wire.SessionRolePlanner},
		{Role: wire.SessionRoleForemanBrain},
		{Role: wire.SessionRoleWorker},
	}

	// Explicit day.
	wantTotals(t, get("/v1/spend?day=2026-11-14"), SpendTotals{Total: 1.25, Jobs: 1, Roles: zeroRoles})
	// Default day: the store clock's date.
	wantTotals(t, get("/v1/spend"), SpendTotals{Total: 1.25, Jobs: 1, Roles: zeroRoles})
	// A day with no jobs at all.
	wantTotals(t, get("/v1/spend?day=2026-10-01"), SpendTotals{Roles: zeroRoles})

	// Malformed day: 400 with the normative error model.
	rec := get("/v1/spend?day=2026-13-99")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_request") {
		t.Errorf("malformed day: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRefuseIfSpendCap(t *testing.T) {
	s, now := spendSetup(t)
	ctx := context.Background()
	base := *now

	// Yesterday's expensive job must not count toward today's breaker.
	*now = base.AddDate(0, 0, -1)
	spendJob(t, s, 50)
	*now = base

	// Today: 5 against a 20 cap is allowed even though yesterday hit 50.
	spendJob(t, s, 5)
	if err := s.RefuseIfSpendCap(ctx, 20, false); err != nil {
		t.Fatalf("below cap refused: %v", err)
	}

	// A second job today pushes spend to 25: refused.
	spendJob(t, s, 20)
	err := s.RefuseIfSpendCap(ctx, 20, false)
	var capErr *CapError
	if !errors.As(err, &capErr) {
		t.Fatalf("want *CapError, got %v", err)
	}
	if capErr.Code() != "spend_cap_exceeded" {
		t.Errorf("Code() = %q, want spend_cap_exceeded", capErr.Code())
	}
	if capErr.Day != "2026-11-14" || capErr.Spend != 25 || capErr.Cap != 20 {
		t.Errorf("cap error = %+v", capErr)
	}

	// Exactly at the cap is allowed; the override bypasses.
	if err := s.RefuseIfSpendCap(ctx, 25, false); err != nil {
		t.Errorf("at cap refused: %v", err)
	}
	if err := s.RefuseIfSpendCap(ctx, 20, true); err != nil {
		t.Errorf("override refused: %v", err)
	}
}
