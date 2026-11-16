package tasks_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
	_ "modernc.org/sqlite"
)

// legacyDDL is the inbox schema exactly as shipped before the redesign
// (origin/master 38a6368, inbox.go migrate). Fixture DBs are built from it
// directly, without any of the new code.
const legacyDDL = `
CREATE TABLE IF NOT EXISTS messages (
	id       TEXT NOT NULL UNIQUE,
	inbox_id TEXT NOT NULL,
	type     TEXT NOT NULL,
	job_id   TEXT NOT NULL DEFAULT '',
	sender   TEXT NOT NULL,
	payload  TEXT NOT NULL,
	ts       TEXT NOT NULL,
	seq      INTEGER PRIMARY KEY AUTOINCREMENT,
	acked    INTEGER NOT NULL DEFAULT 0,
	acked_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_messages_inbox_unacked
	ON messages(inbox_id, seq) WHERE acked = 0;
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_result_dedupe
	ON messages(inbox_id, job_id) WHERE type = 'job.result';
`

type fixtureRow struct {
	id, inbox, typ, job, sender, payload, ts string
	acked                                    bool
}

var fixtureRows = []fixtureRow{
	{"msg_A1", "planner:main", "chat.message", "", "human:operator", `{"text":"hello"}`, "2026-11-01T10:00:00Z", true},
	{"msg_A2", "planner:main", "brain.reply", "", "factory:brain", `{"text":"hi"}`, "2026-11-01T10:00:05Z", true},
	{"msg_A3", "factory:main", "job.result", "job_X", "foreman:f1", `{"ok":true}`, "2026-11-01T10:01:00Z", true},
	{"msg_A4", "planner:main", "foreman.report", "job_X", "foreman:f1", `{"text":"done"}`, "2026-11-01T10:02:00Z", true},
	{"msg_A5", "planner:main", "notify.job", "job_X", "factory", `{"state":"done"}`, "2026-11-01T10:02:01Z", true},
	{"msg_A6", "foreman:f1", "job.assign", "job_Y", "factory", `{"spec":{}}`, "2026-11-02T09:00:00Z", false},
	{"msg_A7", "planner:main", "notify.pending", "job_Y", "factory", `{"reason":"x"}`, "2026-11-02T09:30:00Z", true},
	{"msg_A8", "planner:main", "brain.failed", "job_Y", "foreman:f1", `{"error":"boom"}`, "2026-11-03T08:00:00Z", false},
	{"msg_A9", "planner:main", "chat.message", "", "human:operator", `null`, "2026-11-03T08:05:00Z", false},
}

func buildFixture(t *testing.T, rows []fixtureRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inbox.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(legacyDDL); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		var ackedAt any
		if r.acked {
			ackedAt = r.ts
		}
		if _, err := db.Exec(`INSERT INTO messages (id, inbox_id, type, job_id, sender, payload, ts, acked, acked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.id, r.inbox, r.typ, r.job, r.sender, r.payload, r.ts, r.acked, ackedAt); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// snapshot captures every pre-existing column of every row.
func snapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT id, inbox_id, type, job_id, sender, payload, ts, seq, acked, IFNULL(acked_at, '<nil>') FROM messages ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c [10]string
		if err := rows.Scan(&c[0], &c[1], &c[2], &c[3], &c[4], &c[5], &c[6], &c[7], &c[8], &c[9]); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(c))
	}
	return out
}

func rawSnapshot(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return snapshot(t, db)
}

func openBoth(t *testing.T, path string) (*inbox.Service, *tasks.Store) {
	t.Helper()
	svc, err := inbox.Open(path)
	if err != nil {
		t.Fatalf("inbox.Open: %v", err)
	}
	st, err := tasks.New(context.Background(), svc.DB())
	if err != nil {
		svc.Close()
		t.Fatalf("tasks.New: %v", err)
	}
	return svc, st
}

func threadOf(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT id, IFNULL(thread_id, '') FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var id, th string
		if err := rows.Scan(&id, &th); err != nil {
			t.Fatal(err)
		}
		m[id] = th
	}
	return m
}

func TestLegacyInboxIDMatchesAuth(t *testing.T) {
	if tasks.LegacyInboxID != auth.PlannerMainInbox {
		t.Fatalf("LegacyInboxID %q != auth.PlannerMainInbox %q", tasks.LegacyInboxID, auth.PlannerMainInbox)
	}
}

func TestBackfillFixtureMigratesInPlace(t *testing.T) {
	ctx := context.Background()
	path := buildFixture(t, fixtureRows)
	before := rawSnapshot(t, path)

	svc, st := openBoth(t, path)
	db := svc.DB()

	// Nothing deleted or rewritten.
	if after := snapshot(t, db); !reflect.DeepEqual(before, after) {
		t.Fatalf("pre-existing columns changed:\nbefore %v\nafter  %v", before, after)
	}

	// Exactly one task: the closed legacy task.
	all, err := st.ListTasks(ctx, tasks.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d tasks, want 1: %+v", len(all), all)
	}
	lt := all[0]
	if lt.Title != tasks.LegacyTaskTitle || lt.Status != tasks.StatusClosed ||
		lt.CloseReason != tasks.LegacyCloseReason || lt.ClosedAt == nil || lt.Priority != tasks.DefaultPriority {
		t.Fatalf("legacy task = %+v", lt)
	}
	if got := lt.CreatedAt.Format("2006-01-02T15:04:05Z"); got != "2026-11-01T10:00:00Z" {
		t.Errorf("legacy created_at = %s, want oldest planner:main ts", got)
	}
	if id, ok, err := st.LegacyTaskID(ctx); err != nil || !ok || id != lt.ID {
		t.Errorf("LegacyTaskID = %q %v %v, want %q", id, ok, err, lt.ID)
	}

	// Every planner:main row (acked or not, every type) is stamped; other
	// inboxes untouched.
	th := threadOf(t, db)
	for _, r := range fixtureRows {
		want := ""
		if r.inbox == "planner:main" {
			want = lt.ID
		}
		if th[r.id] != want {
			t.Errorf("%s (%s) thread_id = %q, want %q", r.id, r.inbox, th[r.id], want)
		}
	}

	// The thread reads back through the inbox API, oldest first, with types
	// and ack state intact.
	msgs, err := svc.ThreadMessages(ctx, lt.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs []string
	for _, m := range msgs {
		gotIDs = append(gotIDs, m.ID)
		if m.ThreadID != lt.ID {
			t.Errorf("%s ThreadID = %q", m.ID, m.ThreadID)
		}
	}
	wantIDs := []string{"msg_A1", "msg_A2", "msg_A4", "msg_A5", "msg_A7", "msg_A8", "msg_A9"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("thread = %v, want %v", gotIDs, wantIDs)
	}
	// Unacked delivery unchanged.
	unacked, err := svc.Get(ctx, "planner:main", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(unacked) != 2 || unacked[0].ID != "msg_A8" || unacked[1].ID != "msg_A9" {
		t.Errorf("unacked = %+v", unacked)
	}
	// job.result dedupe index still enforced post-migration.
	_, err = svc.Post(ctx, "factory:main", wire.MessageEnvelope{Type: "job.result", JobID: "job_X", From: "foreman:f1"})
	if _, ok := err.(*inbox.DuplicateError); !ok {
		t.Errorf("dup job.result after migration: err = %v, want DuplicateError", err)
	}
	// LastActivityAt follows the newest thread message unless updated_at
	// (migration time) is later — here it is.
	if lt.LastActivityAt.Before(lt.UpdatedAt) {
		t.Errorf("LastActivityAt %v before UpdatedAt %v", lt.LastActivityAt, lt.UpdatedAt)
	}
	svc.Close()
}

func TestBackfillReopenIdempotent(t *testing.T) {
	ctx := context.Background()
	path := buildFixture(t, fixtureRows)

	svc, st := openBoth(t, path)
	legacy, _, _ := st.LegacyTaskID(ctx)
	// A post-migration row in planner:main with no thread must NOT be swept
	// into the legacy task on a later start.
	posted, err := svc.Post(ctx, "planner:main", wire.MessageEnvelope{Type: "chat.message", From: "human:operator", Payload: []byte(`{"text":"new"}`)})
	if err != nil {
		t.Fatal(err)
	}
	snap1 := snapshot(t, svc.DB())
	th1 := threadOf(t, svc.DB())
	svc.Close()

	for i := 0; i < 3; i++ {
		svc, st = openBoth(t, path)
		db := svc.DB()
		if got := snapshot(t, db); !reflect.DeepEqual(got, snap1) {
			t.Fatalf("reopen %d changed rows", i)
		}
		if got := threadOf(t, db); !reflect.DeepEqual(got, th1) {
			t.Fatalf("reopen %d changed thread ids: %v vs %v", i, got, th1)
		}
		if th1[posted.ID] != "" {
			t.Fatalf("new row stamped: %q", th1[posted.ID])
		}
		ts, err := st.ListTasks(ctx, tasks.TaskFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(ts) != 1 || ts[0].ID != legacy {
			t.Fatalf("reopen %d: tasks = %+v, want only %s", i, ts, legacy)
		}
		var nMig int
		if err := db.QueryRow(`SELECT COUNT(*) FROM tasks_migrations`).Scan(&nMig); err != nil || nMig != 1 {
			t.Fatalf("tasks_migrations rows = %d (%v)", nMig, err)
		}
		// Column added exactly once; index present.
		var nCol, nIdx int
		db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='thread_id'`).Scan(&nCol)
		db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_messages_thread'`).Scan(&nIdx)
		if nCol != 1 || nIdx != 1 {
			t.Fatalf("thread_id cols=%d idx=%d", nCol, nIdx)
		}
		// Explicitly re-running New on the same handle is also a no-op.
		if _, err := tasks.New(ctx, db); err != nil {
			t.Fatal(err)
		}
		svc.Close()
	}
}

func TestBackfillEmptyPlannerInbox(t *testing.T) {
	ctx := context.Background()
	// Only non-planner rows: e.g. a factory DB that never chatted.
	path := buildFixture(t, []fixtureRow{fixtureRows[2], fixtureRows[5]})
	svc, st := openBoth(t, path)
	ts, err := st.ListTasks(ctx, tasks.TaskFilter{})
	if err != nil || len(ts) != 0 {
		t.Fatalf("tasks = %+v %v, want none", ts, err)
	}
	if _, ok, _ := st.LegacyTaskID(ctx); ok {
		t.Fatal("LegacyTaskID ok on empty planner:main")
	}
	// Guard written: a later planner:main row is never swept in.
	if _, err := svc.Post(ctx, "planner:main", wire.MessageEnvelope{Type: "chat.message", From: "human:operator"}); err != nil {
		t.Fatal(err)
	}
	svc.Close()
	svc, st = openBoth(t, path)
	defer svc.Close()
	ts, _ = st.ListTasks(ctx, tasks.TaskFilter{})
	if len(ts) != 0 {
		t.Fatalf("reopen created tasks: %+v", ts)
	}
	for id, th := range threadOf(t, svc.DB()) {
		if th != "" {
			t.Errorf("%s stamped %q", id, th)
		}
	}
}

func TestBackfillFreshDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "inbox.db")
	svc, st := openBoth(t, path)
	defer svc.Close()
	ts, err := st.ListTasks(ctx, tasks.TaskFilter{})
	if err != nil || len(ts) != 0 {
		t.Fatalf("fresh DB tasks = %+v %v", ts, err)
	}
}

func TestNewRequiresInboxMigration(t *testing.T) {
	path := buildFixture(t, nil) // legacy schema, no thread_id column
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := tasks.New(context.Background(), db); err == nil {
		t.Fatal("tasks.New on un-migrated inbox: want error")
	}
}
