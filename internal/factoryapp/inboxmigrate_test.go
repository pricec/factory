package factoryapp_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/pricec/factory/internal/factoryapp"
	"github.com/pricec/factory/internal/tasks"
	_ "modernc.org/sqlite"
)

// TestBootMigratesLegacyInbox: the factory boots on an inbox.db in the
// pre-redesign schema, migrates it in place and runs the legacy backfill
// exactly once across restarts (spec §9.5 storage, inbox redesign S1).
func TestBootMigratesLegacyInbox(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE messages (id TEXT NOT NULL UNIQUE, inbox_id TEXT NOT NULL, type TEXT NOT NULL,
			job_id TEXT NOT NULL DEFAULT '', sender TEXT NOT NULL, payload TEXT NOT NULL, ts TEXT NOT NULL,
			seq INTEGER PRIMARY KEY AUTOINCREMENT, acked INTEGER NOT NULL DEFAULT 0, acked_at TEXT)`,
		`INSERT INTO messages (id, inbox_id, type, sender, payload, ts, acked)
			VALUES ('msg_old1', 'planner:main', 'chat.message', 'human:operator', '{"text":"hi"}', '2026-11-01T00:00:00Z', 1)`,
		`INSERT INTO messages (id, inbox_id, type, sender, payload, ts, acked)
			VALUES ('msg_old2', 'planner:main', 'brain.reply', 'factory:brain', '{"text":"yo"}', '2026-11-01T00:00:01Z', 1)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	cfg := factoryapp.Config{
		DataDir: dir, Addr: "127.0.0.1:0", AdvertiseURL: testAdvertiseURL,
		Now: func() time.Time { return time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) },
	}
	for i := 0; i < 2; i++ {
		a, err := factoryapp.New(cfg)
		if err != nil {
			t.Fatalf("boot %d: %v", i, err)
		}
		if err := a.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	var id, title, status, reason string
	if err := db.QueryRow(`SELECT COUNT(*), MAX(id), MAX(title), MAX(status), MAX(close_reason) FROM tasks WHERE standing = 0`).
		Scan(&n, &id, &title, &status, &reason); err != nil {
		t.Fatal(err)
	}
	if n != 1 || title != tasks.LegacyTaskTitle || status != "closed" || reason != tasks.LegacyCloseReason {
		t.Fatalf("tasks: n=%d %s %q %s %q", n, id, title, status, reason)
	}
	var stamped, total int
	db.QueryRow(`SELECT COUNT(*), SUM(thread_id = ?) FROM messages`, id).Scan(&total, &stamped)
	if total != 2 || stamped != 2 {
		t.Fatalf("messages total=%d stamped=%d", total, stamped)
	}
}
