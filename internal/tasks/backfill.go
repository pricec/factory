package tasks

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Legacy backfill constants (spec §9.5 migration).
const (
	// LegacyInboxID is the operator inbox whose pre-tasks history is
	// backfilled (== auth.PlannerMainInbox; asserted in tests).
	LegacyInboxID = "planner:main"
	// LegacyTaskTitle is the title of the single closed task holding all
	// pre-tasks planner:main history.
	LegacyTaskTitle = "Legacy chat (pre-tasks)"
	// LegacyCloseReason is that task's close_reason.
	LegacyCloseReason = "migration"
	// MigrationLegacyBackfill is the tasks_migrations guard row name.
	MigrationLegacyBackfill = "legacy_backfill_v1"
)

// backfillLegacy runs once per database, ever, guarded by the
// tasks_migrations row MigrationLegacyBackfill (written in the same
// transaction, so a crash leaves either everything or nothing and the next
// start retries). It creates ONE closed task "Legacy chat (pre-tasks)"
// (close_reason "migration", created_at = oldest row's ts) and stamps it as
// thread_id on every planner:main row that has none. Nothing is deleted,
// re-typed or re-acked. When planner:main is empty (fresh install) no task
// is created; the guard is still written so later rows are never swept in.
// Returns the legacy task id ("" when none was created by this call).
func (s *Store) backfillLegacy(ctx context.Context) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var done int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks_migrations WHERE name = ?`, MigrationLegacyBackfill).Scan(&done); err != nil {
		return "", err
	}
	if done > 0 {
		return "", nil
	}

	now := formatTS(s.now())
	var (
		n      int
		oldest sql.NullString
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(ts) FROM messages WHERE inbox_id = ? AND thread_id IS NULL`,
		LegacyInboxID).Scan(&n, &oldest); err != nil {
		return "", err
	}
	id, detail := "", "planner:main empty: no legacy task"
	if n > 0 {
		id = NewTaskID()
		created := now
		if oldest.Valid {
			if t, err := time.Parse(time.RFC3339Nano, oldest.String); err == nil {
				created = formatTS(t)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tasks (id, title, status, priority, created_at, updated_at, closed_at, close_reason)
			 VALUES (?, ?, 'closed', ?, ?, ?, ?, ?)`,
			id, LegacyTaskTitle, DefaultPriority, created, now, now, LegacyCloseReason); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET thread_id = ? WHERE inbox_id = ? AND thread_id IS NULL`,
			id, LegacyInboxID); err != nil {
			return "", err
		}
		detail = id
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks_migrations (name, applied_at, detail) VALUES (?, ?, ?)`,
		MigrationLegacyBackfill, now, detail); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// LegacyTaskID returns the id of the legacy backfill task; ok is false
// when the backfill created none (empty planner:main at migration time).
func (s *Store) LegacyTaskID(ctx context.Context) (id string, ok bool, err error) {
	var detail string
	err = s.db.QueryRowContext(ctx,
		`SELECT detail FROM tasks_migrations WHERE name = ?`, MigrationLegacyBackfill).Scan(&detail)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if len(detail) <= len(TaskIDPrefix) || detail[:len(TaskIDPrefix)] != TaskIDPrefix {
		return "", false, nil
	}
	return detail, true, nil
}
