package jobs

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// Job-less (planner/brain) events dedupe on kind+session id - the unique
// index only covers job-tied rows - and session-less events never dedupe.
func TestRecordUsageDedupesJoblessSessions(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	for i := 0; i < 2; i++ {
		for _, ev := range []wire.UsageEvent{
			{Kind: wire.UsageKindPlanner, SessionID: "ses_p", CostUSD: 1},
			{Kind: wire.UsageKindBrain, SessionID: "ses_p", CostUSD: 1}, // other kind: distinct
			{Kind: wire.UsageKindPlanner, CostUSD: 1},                   // no session: always appended
		} {
			if err := s.RecordUsage(ctx, ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	evs, err := s.ListUsageEvents(ctx, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("events = %+v, want 4 (two deduped sessions + two session-less)", evs)
	}
}

func TestListUsageEventsAndByKind(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	rec := func(ev wire.UsageEvent) {
		t.Helper()
		if err := s.RecordUsage(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	rec(wire.UsageEvent{Kind: wire.UsageKindPlanner, SessionID: "p1", Input: 10, Output: 1, CostUSD: 0.5, DurationMs: 1234})
	rec(wire.UsageEvent{Kind: wire.UsageKindBrain, SessionID: "b1", Input: 20, CacheCreate: 5, CacheRead: 100, CostUSD: 0.25, DurationMs: -1})
	now = now.Add(2 * time.Hour) // next UTC day
	rec(wire.UsageEvent{Kind: wire.UsageKindWorker, JobID: "job_x", SessionID: "w1", Input: 1, Output: 2, CostUSD: 1})

	day1, err := s.ListUsageEvents(ctx, "2026-11-16", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(day1) != 2 || day1[0].SessionID != "b1" || day1[1].SessionID != "p1" {
		t.Fatalf("day1 = %+v, want b1 then p1 (newest first)", day1)
	}
	if day1[1].DurationMs != 1234 || day1[0].DurationMs != 0 || day1[1].ID == 0 || day1[1].TS == "" {
		t.Fatalf("duration/id/ts not persisted: %+v", day1)
	}
	if got, _ := s.ListUsageEvents(ctx, "", wire.UsageKindWorker, 0); len(got) != 1 || got[0].JobID != "job_x" {
		t.Fatalf("kind filter = %+v", got)
	}
	if got, _ := s.ListUsageEvents(ctx, "", "", 2); len(got) != 2 || got[0].SessionID != "w1" {
		t.Fatalf("limit = %+v", got)
	}
	if got, _ := s.ListUsageEvents(ctx, "2026-01-01", "", 0); got == nil || len(got) != 0 {
		t.Fatalf("empty day = %#v, want non-nil empty", got)
	}

	sums, err := s.UsageSince(ctx, time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if sums.Tokens != 11+25+3 || sums.CostUSD != 1.75 {
		t.Fatalf("sums = %+v", sums)
	}
	want := map[string]KindSums{
		wire.UsageKindPlanner: {Runs: 1, Tokens: 11, CostUSD: 0.5},
		wire.UsageKindBrain:   {Runs: 1, Tokens: 25, CostUSD: 0.25},
		wire.UsageKindWorker:  {Runs: 1, Tokens: 3, CostUSD: 1},
	}
	for k, w := range want {
		if sums.ByKind[k] != w {
			t.Errorf("by_kind[%s] = %+v, want %+v", k, sums.ByKind[k], w)
		}
	}
	empty, _ := s.UsageSince(ctx, now.Add(time.Hour))
	if len(empty.ByKind) != 3 || empty.ByKind[wire.UsageKindBrain] != (KindSums{}) {
		t.Fatalf("empty window by_kind = %+v, want every kind at zero", empty.ByKind)
	}
}

// A database created before duration_ms existed gains the column on open.
func TestUsageEventsDurationMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE usage_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, kind TEXT NOT NULL,
		job_id TEXT NOT NULL DEFAULT '', task_id TEXT NOT NULL DEFAULT '',
		foreman_id TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
		tier TEXT NOT NULL DEFAULT '', input INTEGER NOT NULL DEFAULT 0,
		output INTEGER NOT NULL DEFAULT 0, cache_create INTEGER NOT NULL DEFAULT 0,
		cache_read INTEGER NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0,
		session_id TEXT NOT NULL DEFAULT '', limit_raw TEXT NOT NULL DEFAULT '');
		INSERT INTO usage_events (ts, kind, cost_usd) VALUES ('2026-11-16T00:00:00Z', 'planner', 2);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open pre-duration db: %v", err)
	}
	defer s.Close()
	evs, err := s.ListUsageEvents(context.Background(), "", "", 0)
	if err != nil || len(evs) != 1 || evs[0].CostUSD != 2 || evs[0].DurationMs != 0 {
		t.Fatalf("legacy row = %+v err=%v", evs, err)
	}
}
