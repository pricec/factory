// Package tasks is the storage layer for the inbox redesign (spec §9.5):
// operator tasks (conversation threads), task↔job links, alerts and
// free-form settings. It shares the factory inbox database (inbox.db) with
// internal/inbox; open it with New(ctx, inboxService.DB()) AFTER inbox.Open,
// which owns the messages.thread_id column this package backfills.
//
// The store is deliberately policy-free: no TTL defaults, no wakes, no
// reaper, no state-machine rules beyond keeping closed_at/acked_at
// consistent with status/state. Those belong to the callers (factoryapp).
package tasks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// ID prefixes.
const (
	TaskIDPrefix  = "task_"
	AlertIDPrefix = "alert_"
)

// NewTaskID returns a fresh "task_<ulid>" id.
func NewTaskID() string { return TaskIDPrefix + ulid.Make().String() }

// NewAlertID returns a fresh "alert_<ulid>" id.
func NewAlertID() string { return AlertIDPrefix + ulid.Make().String() }

// Errors.
var (
	// ErrNotFound: no task/alert with that id.
	ErrNotFound = errors.New("tasks: not found")
	// ErrStateConflict: TransitionAlert found the alert in a state not in
	// its allowed-from set. The returned Alert carries the current state.
	ErrStateConflict = errors.New("tasks: state conflict")
	// ErrInvalid: a field value is outside its domain (bad status/state,
	// empty title).
	ErrInvalid = errors.New("tasks: invalid argument")
)

// tsLayout is the fixed-width UTC timestamp format stored in every time
// column of this package. Fixed width keeps text ordering == time ordering;
// a "YYYY-MM-DD" prefix matches a day.
const tsLayout = "2006-01-02T15:04:05.000Z"

func formatTS(t time.Time) string { return t.UTC().Format(tsLayout) }

// parseTS accepts this package's layout and RFC3339(Nano) (message ts).
func parseTS(s string) (time.Time, error) {
	if t, err := time.Parse(tsLayout, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func parseNullTS(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTS(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTS(*t)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Store is the tasks/alerts/settings store. Safe for concurrent use (it
// relies on database/sql; the inbox handle is a single connection).
type Store struct {
	db *sql.DB
	// Now is the clock (UTC is applied on write). Tests override it.
	Now func() time.Time
}

// New migrates the shared inbox database (additive, idempotent) and runs
// the one-time legacy backfill, then returns the store. db must be the
// handle of an inbox already opened by inbox.Open (messages.thread_id must
// exist); New fails otherwise. The Store does not own db: never Close it
// through the store.
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{db: db, Now: time.Now}
	if err := s.migrate(ctx); err != nil {
		return nil, fmt.Errorf("tasks: migrate: %w", err)
	}
	if _, err := s.backfillLegacy(ctx); err != nil {
		return nil, fmt.Errorf("tasks: legacy backfill: %w", err)
	}
	return s, nil
}

func (s *Store) now() time.Time { return s.Now().UTC() }

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id           TEXT PRIMARY KEY,
	title        TEXT NOT NULL,
	status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
	priority     INTEGER NOT NULL DEFAULT 2,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL,
	closed_at    TEXT,
	close_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tasks_rank ON tasks(status, priority, updated_at);
CREATE TABLE IF NOT EXISTS task_jobs (
	task_id TEXT NOT NULL,
	job_id  TEXT NOT NULL,
	PRIMARY KEY (task_id, job_id)
);
CREATE INDEX IF NOT EXISTS idx_task_jobs_job ON task_jobs(job_id);
CREATE TABLE IF NOT EXISTS alerts (
	id         TEXT PRIMARY KEY,
	priority   INTEGER NOT NULL,
	title      TEXT NOT NULL,
	body       TEXT NOT NULL DEFAULT '',
	source     TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	expires_at TEXT,
	state      TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'acked', 'replied', 'expired')),
	task_id    TEXT,
	acked_at   TEXT
);
CREATE INDEX IF NOT EXISTS idx_alerts_state ON alerts(state, priority, created_at);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks_migrations (
	name       TEXT PRIMARY KEY,
	applied_at TEXT NOT NULL,
	detail     TEXT NOT NULL DEFAULT ''
);
`

func (s *Store) migrate(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'thread_id'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("messages.thread_id missing: open the database with inbox.Open first")
	}
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	// needs_operator (spec §9.5.2): added after the first release, so
	// existing databases get it by ALTER (additive, idempotent).
	if err := s.addColumnIfMissing(ctx, "tasks", "needs_operator", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// standing: flags the one well-known standing planner chat (additive).
	if err := s.addColumnIfMissing(ctx, "tasks", "standing", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// follow_up_of / parent_task_id: optional links to other tasks (set at
	// creation, validated against existing tasks; additive, nullable).
	if err := s.addColumnIfMissing(ctx, "tasks", "follow_up_of", "TEXT"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, "tasks", "parent_task_id", "TEXT"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_tasks_parent ON tasks(parent_task_id);
		 CREATE INDEX IF NOT EXISTS idx_tasks_follow_up ON tasks(follow_up_of)`); err != nil {
		return err
	}
	return s.EnsureStanding(ctx)
}

// addColumnIfMissing runs ALTER TABLE ... ADD COLUMN unless the column is
// already there.
func (s *Store) addColumnIfMissing(ctx context.Context, table, column, decl string) error {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+decl)
	return err
}

// likeEscape escapes LIKE wildcards for use with ESCAPE '\'.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func errInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}
