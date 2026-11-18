package tasks

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Task statuses.
const (
	StatusOpen   = "open"
	StatusClosed = "closed"
)

// DefaultPriority is the priority of a task created without one (0 = most
// urgent).
const DefaultPriority = 2

// Task is one operator task (a conversation thread; its messages carry
// messages.thread_id = Task.ID). JSON tags follow contract I3 except
// "preview", which callers derive from the thread.
type Task struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Status      string     `json:"status"`
	Priority    int        `json:"priority"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	CloseReason string     `json:"close_reason,omitempty"`
	// NeedsOperator is the planner's explicit "waiting on the operator"
	// flag (spec §9.5.2): set by the planner (update_task), cleared when
	// the operator posts on the task or the task is closed.
	NeedsOperator bool `json:"needs_operator"`
	// Standing marks the one well-known standing planner chat (StandingID):
	// a task row that is never closed, never listed with ordinary tasks and
	// never "awaiting"/"needs you". See EnsureStanding.
	Standing bool `json:"standing,omitempty"`
	// FollowUpOf is the task this one continues after a change of goal
	// (empty when none). ParentTaskID is the task this one is a child of
	// (empty when none). Both are set at creation and reference existing
	// tasks.
	FollowUpOf   string `json:"follow_up_of,omitempty"`
	ParentTaskID string `json:"parent_task_id,omitempty"`
	// ChildTaskIDs and FollowUpTaskIDs are the reverse links: tasks whose
	// parent_task_id / follow_up_of is this task, oldest first. Computed on
	// read; never nil on tasks returned by the store.
	ChildTaskIDs    []string `json:"child_task_ids"`
	FollowUpTaskIDs []string `json:"follow_up_task_ids"`
	// JobIDs are the linked jobs (task_jobs), in link order. Never nil on
	// tasks returned by the store.
	JobIDs []string `json:"job_ids"`
	// LastActivityAt is max(UpdatedAt, ts of the newest message in the
	// thread). Computed on read, not stored.
	LastActivityAt time.Time `json:"last_activity_at"`
}

// TaskFilter selects tasks for ListTasks.
type TaskFilter struct {
	// Status: "open", "closed", or "" / "all" for both.
	Status string
	// Query, when non-empty, matches (case-insensitively for ASCII) a title
	// substring, an id prefix (with or without "task_"), or a prefix of
	// created_at / last activity (so "2026-11-16" matches a day).
	Query string
	// Limit caps the result; <= 0 means unbounded.
	Limit int
	// IncludeStanding also returns the standing planner chat; by default
	// it is excluded from every listing.
	IncludeStanding bool
	// ParentTaskID / FollowUpOf, when non-empty, keep only tasks linked to
	// that task id.
	ParentTaskID string
	FollowUpOf   string
}

// TaskUpdate is a partial update; nil fields are left alone.
type TaskUpdate struct {
	Title    *string
	Priority *int
	// Status "open" reopens (clears closed_at and close_reason); "closed"
	// closes (sets closed_at=now unless already closed).
	Status *string
	// CloseReason is stored as given (typically alongside Status=closed).
	CloseReason *string
	// NeedsOperator sets or clears the needs_operator flag. When nil, a
	// close (Status=closed on an open task) clears it.
	NeedsOperator *bool
}

// CreateTask inserts a new open task. Title must be non-empty.
func (s *Store) CreateTask(ctx context.Context, title string, priority int) (Task, error) {
	return s.CreateTaskLinked(ctx, title, priority, TaskLinks{})
}

// TaskLinks are the optional links of a new task.
type TaskLinks struct {
	FollowUpOf   string
	ParentTaskID string
}

// CreateTaskLinked is CreateTask with links. Each non-empty link must name
// an existing, non-standing task, else ErrInvalid.
func (s *Store) CreateTaskLinked(ctx context.Context, title string, priority int, l TaskLinks) (Task, error) {
	if strings.TrimSpace(title) == "" {
		return Task{}, errInvalid("title: required")
	}
	for _, c := range []struct{ name, id string }{{"follow_up_of", l.FollowUpOf}, {"parent_task_id", l.ParentTaskID}} {
		if c.id == "" {
			continue
		}
		var standing bool
		err := s.db.QueryRowContext(ctx, `SELECT standing FROM tasks WHERE id = ?`, c.id).Scan(&standing)
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, errInvalid("%s: unknown task %q", c.name, c.id)
		}
		if err != nil {
			return Task{}, err
		}
		if standing {
			return Task{}, errInvalid("%s: the standing chat cannot be linked", c.name)
		}
	}
	now := formatTS(s.now())
	id := NewTaskID()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, title, status, priority, created_at, updated_at, follow_up_of, parent_task_id)
		 VALUES (?, ?, 'open', ?, ?, ?, ?, ?)`, id, title, priority, now, now,
		nullStr(l.FollowUpOf), nullStr(l.ParentTaskID)); err != nil {
		return Task{}, err
	}
	return s.GetTask(ctx, id)
}

const taskSelect = `
SELECT id, title, status, priority, created_at, updated_at, closed_at, close_reason, needs_operator, standing, follow_up_of, parent_task_id, last_act FROM (
	SELECT t.*, MAX(t.updated_at, IFNULL(
		(SELECT m.ts FROM messages m WHERE m.thread_id = t.id ORDER BY m.seq DESC LIMIT 1), '')) AS last_act
	FROM tasks t
)`

func scanTask(r rowScanner) (Task, error) {
	var (
		t                         Task
		created, updated, lastAct string
		closed                    sql.NullString
		followUp, parent          sql.NullString
	)
	if err := r.Scan(&t.ID, &t.Title, &t.Status, &t.Priority, &created, &updated, &closed, &t.CloseReason, &t.NeedsOperator, &t.Standing, &followUp, &parent, &lastAct); err != nil {
		return t, err
	}
	t.FollowUpOf, t.ParentTaskID = followUp.String, parent.String
	var err error
	if t.CreatedAt, err = parseTS(created); err != nil {
		return t, err
	}
	if t.UpdatedAt, err = parseTS(updated); err != nil {
		return t, err
	}
	if t.ClosedAt, err = parseNullTS(closed); err != nil {
		return t, err
	}
	if t.LastActivityAt, err = parseTS(lastAct); err != nil {
		return t, err
	}
	if t.LastActivityAt.Before(t.UpdatedAt) {
		t.LastActivityAt = t.UpdatedAt
	}
	t.JobIDs = []string{}
	t.ChildTaskIDs = []string{}
	t.FollowUpTaskIDs = []string{}
	return t, nil
}

type rowScanner interface{ Scan(dest ...any) error }

// GetTask returns the task with its job ids, or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, taskSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	ts := []Task{t}
	if err := s.fillRelations(ctx, ts); err != nil {
		return Task{}, err
	}
	return ts[0], nil
}

// ListTasks returns tasks ranked: open before closed, then priority ASC
// (0 = most urgent), then last activity DESC, then id DESC.
func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	var (
		where []string
		args  []any
	)
	if !f.IncludeStanding {
		where = append(where, `standing = 0`)
	}
	switch f.Status {
	case "", "all":
	case StatusOpen, StatusClosed:
		where = append(where, `status = ?`)
		args = append(args, f.Status)
	default:
		return nil, errInvalid("status: want open, closed or all, got %q", f.Status)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		e := likeEscape(q)
		where = append(where, `(title LIKE ? ESCAPE '\' OR id LIKE ? ESCAPE '\' OR id LIKE ? ESCAPE '\'
			OR created_at LIKE ? ESCAPE '\' OR last_act LIKE ? ESCAPE '\')`)
		args = append(args, "%"+e+"%", e+"%", likeEscape(TaskIDPrefix)+e+"%", e+"%", e+"%")
	}
	if f.ParentTaskID != "" {
		where = append(where, `parent_task_id = ?`)
		args = append(args, f.ParentTaskID)
	}
	if f.FollowUpOf != "" {
		where = append(where, `follow_up_of = ?`)
		args = append(args, f.FollowUpOf)
	}
	q := taskSelect
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = -1
	}
	q += ` ORDER BY (status = 'open') DESC, priority ASC, last_act DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err := s.fillRelations(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// fillRelations fills the computed reverse task links, then the job ids.
func (s *Store) fillRelations(ctx context.Context, ts []Task) error {
	if len(ts) == 0 {
		return nil
	}
	if err := s.fillJobIDs(ctx, ts); err != nil {
		return err
	}
	idx := make(map[string]int, len(ts))
	ph := make([]string, len(ts))
	args := make([]any, 0, 2*len(ts))
	for i, t := range ts {
		idx[t.ID] = i
		ph[i] = "?"
		args = append(args, t.ID)
	}
	in := strings.Join(ph, ",")
	args = append(args, args...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, parent_task_id, follow_up_of FROM tasks
		 WHERE parent_task_id IN (`+in+`) OR follow_up_of IN (`+in+`)
		 ORDER BY created_at, id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var parent, followUp sql.NullString
		if err := rows.Scan(&id, &parent, &followUp); err != nil {
			return err
		}
		if i, ok := idx[parent.String]; ok && parent.Valid {
			ts[i].ChildTaskIDs = append(ts[i].ChildTaskIDs, id)
		}
		if i, ok := idx[followUp.String]; ok && followUp.Valid {
			ts[i].FollowUpTaskIDs = append(ts[i].FollowUpTaskIDs, id)
		}
	}
	return rows.Err()
}

func (s *Store) fillJobIDs(ctx context.Context, ts []Task) error {
	if len(ts) == 0 {
		return nil
	}
	idx := make(map[string]int, len(ts))
	ph := make([]string, len(ts))
	args := make([]any, len(ts))
	for i, t := range ts {
		idx[t.ID] = i
		ph[i] = "?"
		args[i] = t.ID
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id, job_id FROM task_jobs WHERE task_id IN (`+strings.Join(ph, ",")+`) ORDER BY rowid`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tid, jid string
		if err := rows.Scan(&tid, &jid); err != nil {
			return err
		}
		i := idx[tid]
		ts[i].JobIDs = append(ts[i].JobIDs, jid)
	}
	return rows.Err()
}

// UpdateTask applies u to task id (bumping updated_at) and returns the
// updated task, or ErrNotFound / ErrInvalid.
func (s *Store) UpdateTask(ctx context.Context, id string, u TaskUpdate) (Task, error) {
	if u.Title != nil && strings.TrimSpace(*u.Title) == "" {
		return Task{}, errInvalid("title: must not be empty")
	}
	if u.Status != nil && *u.Status != StatusOpen && *u.Status != StatusClosed {
		return Task{}, errInvalid("status: want open or closed, got %q", *u.Status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()

	var (
		status   string
		standing bool
	)
	err = tx.QueryRowContext(ctx, `SELECT status, standing FROM tasks WHERE id = ?`, id).Scan(&status, &standing)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	if standing {
		// The standing chat is permanent: it cannot be closed, retitled or
		// flagged as waiting on the operator.
		if u.Status != nil && *u.Status == StatusClosed {
			return Task{}, errInvalid("the standing planner chat cannot be closed")
		}
		if u.Title != nil {
			return Task{}, errInvalid("the standing planner chat cannot be renamed")
		}
		if u.NeedsOperator != nil && *u.NeedsOperator {
			return Task{}, errInvalid("the standing planner chat cannot need the operator")
		}
	}
	now := formatTS(s.now())
	sets := []string{`updated_at = ?`}
	args := []any{now}
	if u.Title != nil {
		sets = append(sets, `title = ?`)
		args = append(args, *u.Title)
	}
	if u.Priority != nil {
		sets = append(sets, `priority = ?`)
		args = append(args, *u.Priority)
	}
	if u.Status != nil {
		switch {
		case *u.Status == StatusClosed && status != StatusClosed:
			sets = append(sets, `status = 'closed'`, `closed_at = ?`)
			args = append(args, now)
			if u.NeedsOperator == nil {
				sets = append(sets, `needs_operator = 0`)
			}
		case *u.Status == StatusOpen && status != StatusOpen:
			sets = append(sets, `status = 'open'`, `closed_at = NULL`, `close_reason = ''`)
		}
	}
	if u.CloseReason != nil {
		sets = append(sets, `close_reason = ?`)
		args = append(args, *u.CloseReason)
	}
	if u.NeedsOperator != nil {
		sets = append(sets, `needs_operator = ?`)
		args = append(args, *u.NeedsOperator)
	}
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return s.GetTask(ctx, id)
}

// ClearNeedsOperator clears the needs_operator flag (the operator
// answered). It does not bump updated_at. ErrNotFound for an unknown task.
func (s *Store) ClearNeedsOperator(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET needs_operator = 0 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// TouchTask bumps updated_at to now (e.g. on activity that adds no message).
func (s *Store) TouchTask(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, formatTS(s.now()), id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// LinkJob records jobID under taskID (task_jobs). Idempotent: re-linking
// the same pair is a no-op. ErrNotFound when the task does not exist.
func (s *Store) LinkJob(ctx context.Context, taskID, jobID string) error {
	if jobID == "" {
		return errInvalid("job_id: required")
	}
	if taskID == StandingID {
		return errInvalid("jobs cannot be linked to the standing planner chat; create a task for the work")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO task_jobs (task_id, job_id)
		 SELECT id, ? FROM tasks WHERE id = ?`, jobID, taskID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	// Nothing inserted: either already linked or no such task.
	var one int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, taskID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// JobIDs returns the jobs linked to taskID in link order (empty, not an
// error, for an unknown task).
func (s *Store) JobIDs(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id FROM task_jobs WHERE task_id = ? ORDER BY rowid`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// TaskForJob returns the task jobID is linked to (the earliest link if
// several). ok is false when the job is not linked. It does NOT follow
// ParentJobID; job-lineage resolution is the caller's (S2) business.
func (s *Store) TaskForJob(ctx context.Context, jobID string) (taskID string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT task_id FROM task_jobs WHERE job_id = ? ORDER BY rowid LIMIT 1`, jobID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return taskID, true, nil
}

func mustAffect(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Standing chat (the operator's permanent planner conversation).
const (
	// StandingID is the well-known id of the standing planner chat task.
	StandingID = "task_standing"
	// StandingTitle is its fixed title.
	StandingTitle = "Planner"
)

// EnsureStanding creates the standing chat task when missing and repairs
// it when an older row somehow lost its flag or got closed. Idempotent;
// called by New, so it exists on every start.
func (s *Store) EnsureStanding(ctx context.Context) error {
	now := formatTS(s.now())
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, title, status, priority, created_at, updated_at, standing)
		 VALUES (?, ?, 'open', 0, ?, ?, 1)
		 ON CONFLICT(id) DO UPDATE SET standing = 1, status = 'open', closed_at = NULL,
		   close_reason = '', needs_operator = 0, title = excluded.title`,
		StandingID, StandingTitle, now, now); err != nil {
		return err
	}
	return nil
}
