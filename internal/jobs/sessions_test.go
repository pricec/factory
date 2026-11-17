package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// openTestStore opens a fresh store in a temp dir.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// seedSession records one ledger row through the store.
func seedSession(t *testing.T, s *Store, u wire.SessionUsage) {
	t.Helper()
	if err := s.RecordSession(context.Background(), u); err != nil {
		t.Fatalf("RecordSession %s: %v", u.SessionID, err)
	}
}

func TestRecordSessionValidates(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	cases := []struct {
		name string
		u    wire.SessionUsage
	}{
		{"missing session id", wire.SessionUsage{Role: wire.SessionRoleWorker}},
		{"unknown role", wire.SessionUsage{SessionID: "ses_a", Role: "boss"}},
		{"negative cost", wire.SessionUsage{SessionID: "ses_a", Role: wire.SessionRoleWorker, CostUSD: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.RecordSession(context.Background(), tc.u); err == nil {
				t.Fatal("RecordSession accepted an invalid payload")
			}
		})
	}
}

func TestRecordSessionUpserts(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	first := wire.SessionUsage{
		SessionID: "ses_a", Role: wire.SessionRoleWorker, ForemanID: "server",
		JobID: "job_x", Model: "openrouter/z-ai/glm-5.3-flash",
		InputTokens: 100, OutputTokens: 10, CostUSD: 0.01,
	}
	seedSession(t, s, first)
	// Duplicate delivery (inbox at-least-once) with corrected numbers:
	// same session id replaces, never double-counts.
	second := first
	second.InputTokens, second.CostUSD = 150, 0.02
	seedSession(t, s, second)

	rows, err := s.ListSessions(context.Background(), "", "", 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (upsert by session id)", len(rows))
	}
	if rows[0].InputTokens != 150 || rows[0].CostUSD != 0.02 {
		t.Errorf("row = %+v, want the second report's numbers", rows[0])
	}
}

func TestSessionTotals(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	// Two days: the clock pins both rows inside the same day boundary.
	day := func(n int) string { return "2026-11-1" + string(rune('0'+n)) }
	_ = day
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	seedSession(t, s, wire.SessionUsage{
		SessionID: "ses_p", Role: wire.SessionRolePlanner, Model: "openrouter/z-ai/glm-5.3",
		InputTokens: 100, OutputTokens: 20, CostUSD: 0.3,
	})
	now = time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	seedSession(t, s, wire.SessionUsage{
		SessionID: "ses_p2", Role: wire.SessionRolePlanner, CostUSD: 0.1, CacheReadTokens: 50,
	})
	now = time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)
	seedSession(t, s, wire.SessionUsage{
		SessionID: "ses_w", Role: wire.SessionRoleWorker, ForemanID: "server", JobID: "job_x",
		CostUSD: 0.02, ReasoningTokens: 7,
	})
	// A different day must stay out of the 2026-11-15 aggregates.
	now = time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC)
	seedSession(t, s, wire.SessionUsage{SessionID: "ses_old", Role: wire.SessionRoleWorker, CostUSD: 9})

	got, err := s.SessionTotals(context.Background(), "2026-11-15")
	if err != nil {
		t.Fatalf("SessionTotals: %v", err)
	}
	want := []RoleTotals{
		{Role: wire.SessionRolePlanner, Sessions: 2, InputTokens: 100, OutputTokens: 20, CacheReadTokens: 50, TotalUSD: 0.4},
		{Role: wire.SessionRoleForemanBrain},
		{Role: wire.SessionRoleWorker, Sessions: 1, ReasoningTokens: 7, TotalUSD: 0.02},
	}
	if len(got) != len(want) {
		t.Fatalf("totals = %d roles, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("totals[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A day with no rows reports every role at zero, not an empty list.
	got, err = s.SessionTotals(context.Background(), "2026-11-16")
	if err != nil {
		t.Fatalf("SessionTotals empty day: %v", err)
	}
	for _, r := range got {
		if r.Sessions != 0 || r.TotalUSD != 0 {
			t.Errorf("empty day role %s = %+v, want zeros", r.Role, r)
		}
	}
}

func TestListSessionsFilters(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	seedSession(t, s, wire.SessionUsage{SessionID: "ses_1", Role: wire.SessionRolePlanner, CostUSD: 1})
	now = now.Add(time.Hour)
	seedSession(t, s, wire.SessionUsage{SessionID: "ses_2", Role: wire.SessionRoleWorker, ForemanID: "server", JobID: "job_1"})
	now = now.Add(time.Hour)
	seedSession(t, s, wire.SessionUsage{SessionID: "ses_3", Role: wire.SessionRoleWorker, ForemanID: "videochat", JobID: "job_2"})
	now = now.Add(24 * time.Hour) // next day, excluded by day filter
	seedSession(t, s, wire.SessionUsage{SessionID: "ses_4", Role: wire.SessionRoleWorker})

	rows, err := s.ListSessions(context.Background(), "2026-11-15", "", 0)
	if err != nil {
		t.Fatalf("ListSessions day: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("day rows = %d, want 3", len(rows))
	}
	// Newest first.
	if rows[0].ID != "ses_3" || rows[2].ID != "ses_1" {
		t.Errorf("order = %s..%s, want ses_3 first and ses_1 last", rows[0].ID, rows[2].ID)
	}

	rows, err = s.ListSessions(context.Background(), "2026-11-15", wire.SessionRoleWorker, 0)
	if err != nil {
		t.Fatalf("ListSessions role: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("role rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Role != wire.SessionRoleWorker {
			t.Errorf("role filter leaked %s row", r.Role)
		}
	}

	rows, err = s.ListSessions(context.Background(), "", "", 2)
	if err != nil {
		t.Fatalf("ListSessions limit: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("limit rows = %d, want 2", len(rows))
	}
}

func TestBackfillJobUsage(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	spec := wire.NewJobSpec("do", "factory")
	spec.TimeoutS = 60
	if err := s.Create(context.Background(), spec); err != nil {
		t.Fatalf("create job: %v", err)
	}

	// Empty usage backfills.
	if err := s.BackfillJobUsage(context.Background(), spec.ID,
		wire.Usage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.01}); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// Agent-reported usage wins: the second backfill must not overwrite.
	if err := s.BackfillJobUsage(context.Background(), spec.ID,
		wire.Usage{CostUSD: 9}); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	job, ok, err := s.Get(context.Background(), spec.ID)
	if err != nil || !ok {
		t.Fatalf("get job: %v (ok %v)", err, ok)
	}
	var u wire.Usage
	if err := json.Unmarshal(job.Usage, &u); err != nil {
		t.Fatalf("usage %s: %v", job.Usage, err)
	}
	if u.InputTokens != 10 || u.CostUSD != 0.01 {
		t.Errorf("usage = %+v, want the first backfill (second must not overwrite)", u)
	}

	// A missing job is not an error.
	if err := s.BackfillJobUsage(context.Background(), "job_missing", wire.Usage{}); err != nil {
		t.Errorf("backfill for a missing job: %v, want nil", err)
	}
}

func TestSessionsHandler(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	s.Now = func() time.Time { return time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC) }
	seedSession(t, s, wire.SessionUsage{SessionID: "ses_1", Role: wire.SessionRolePlanner, CostUSD: 0.25})

	h := s.SessionsHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("default day: %d", rec.Code)
	}
	var rows []Session
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if len(rows) != 1 || rows[0].ID != "ses_1" {
		t.Errorf("rows = %+v, want ses_1 (default day is the store clock's today)", rows)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions?day=2026-11-14", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("other day: %d", rec.Code)
	}
	if got := rec.Body.String(); got != "null\n" && got != "null" {
		t.Errorf("empty day body = %q, want null", got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions?day=15-11-2026", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed day = %d, want 400", rec.Code)
	}
}
