package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Alert states.
const (
	AlertOpen    = "open"
	AlertAcked   = "acked"
	AlertReplied = "replied"
	AlertExpired = "expired"
)

func validAlertState(s string) bool {
	switch s {
	case AlertOpen, AlertAcked, AlertReplied, AlertExpired:
		return true
	}
	return false
}

// Alert is one operator alert. JSON follows contract I4 exactly
// (expires_at and task_id are null when unset; acked_at is not exposed).
type Alert struct {
	ID        string
	Priority  int
	Title     string
	Body      string
	Source    string
	CreatedAt time.Time
	// ExpiresAt nil = never expires.
	ExpiresAt *time.Time
	State     string
	// TaskID "" = no linked task (NULL).
	TaskID  string
	AckedAt *time.Time
}

type alertJSON struct {
	ID        string     `json:"id"`
	Priority  int        `json:"priority"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Source    string     `json:"source"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	State     string     `json:"state"`
	TaskID    *string    `json:"task_id"`
}

// MarshalJSON renders the I4 Alert shape.
func (a Alert) MarshalJSON() ([]byte, error) {
	j := alertJSON{
		ID: a.ID, Priority: a.Priority, Title: a.Title, Body: a.Body, Source: a.Source,
		CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt, State: a.State,
	}
	if a.TaskID != "" {
		tid := a.TaskID
		j.TaskID = &tid
	}
	return json.Marshal(j)
}

// UnmarshalJSON parses the I4 Alert shape (round-trip for clients/tests).
func (a *Alert) UnmarshalJSON(b []byte) error {
	var j alertJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*a = Alert{
		ID: j.ID, Priority: j.Priority, Title: j.Title, Body: j.Body, Source: j.Source,
		CreatedAt: j.CreatedAt, ExpiresAt: j.ExpiresAt, State: j.State,
	}
	if j.TaskID != nil {
		a.TaskID = *j.TaskID
	}
	return nil
}

// NewAlert is the input to CreateAlert. The store applies no TTL policy:
// ExpiresAt is stored exactly as given (nil = never).
type NewAlert struct {
	Priority  int
	Title     string
	Body      string
	Source    string
	ExpiresAt *time.Time
	TaskID    string
}

// AlertFilter selects alerts for ListAlerts.
type AlertFilter struct {
	// States restricts to these states; empty = all states.
	States []string
	// Limit caps the result; <= 0 = unbounded.
	Limit int
}

// CreateAlert stores a new open alert (created_at = now). Title is
// required.
func (s *Store) CreateAlert(ctx context.Context, n NewAlert) (Alert, error) {
	if strings.TrimSpace(n.Title) == "" {
		return Alert{}, errInvalid("title: required")
	}
	id := NewAlertID()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO alerts (id, priority, title, body, source, created_at, expires_at, state, task_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'open', ?)`,
		id, n.Priority, n.Title, n.Body, n.Source, formatTS(s.now()), nullTS(n.ExpiresAt), nullStr(n.TaskID)); err != nil {
		return Alert{}, err
	}
	return s.GetAlert(ctx, id)
}

const alertCols = `id, priority, title, body, source, created_at, expires_at, state, IFNULL(task_id, ''), acked_at`

func scanAlert(r rowScanner) (Alert, error) {
	var (
		a                Alert
		created          string
		expires, ackedAt sql.NullString
	)
	if err := r.Scan(&a.ID, &a.Priority, &a.Title, &a.Body, &a.Source, &created, &expires, &a.State, &a.TaskID, &ackedAt); err != nil {
		return a, err
	}
	var err error
	if a.CreatedAt, err = parseTS(created); err != nil {
		return a, err
	}
	if a.ExpiresAt, err = parseNullTS(expires); err != nil {
		return a, err
	}
	if a.AckedAt, err = parseNullTS(ackedAt); err != nil {
		return a, err
	}
	return a, nil
}

// GetAlert returns the alert or ErrNotFound.
func (s *Store) GetAlert(ctx context.Context, id string) (Alert, error) {
	a, err := scanAlert(s.db.QueryRowContext(ctx, `SELECT `+alertCols+` FROM alerts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Alert{}, ErrNotFound
	}
	return a, err
}

// ListAlerts returns alerts ordered priority ASC, then created_at DESC,
// then id DESC. It does not apply expiry; call ExpireAlerts first for a
// lazily-expired view.
func (s *Store) ListAlerts(ctx context.Context, f AlertFilter) ([]Alert, error) {
	q := `SELECT ` + alertCols + ` FROM alerts`
	var args []any
	if len(f.States) > 0 {
		ph := make([]string, len(f.States))
		for i, st := range f.States {
			if !validAlertState(st) {
				return nil, errInvalid("state: unknown %q", st)
			}
			ph[i] = "?"
			args = append(args, st)
		}
		q += ` WHERE state IN (` + strings.Join(ph, ",") + `)`
	}
	limit := f.Limit
	if limit <= 0 {
		limit = -1
	}
	q += ` ORDER BY priority ASC, created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TransitionAlert moves alert id to state `to`, but only if its current
// state is in `from` (empty from = any state: an unconditional set). It is
// an atomic compare-and-set; moving to "acked" stamps acked_at = now.
// Returns ErrNotFound, or ErrStateConflict together with the alert as it
// currently is (unchanged) when the current state is not allowed.
func (s *Store) TransitionAlert(ctx context.Context, id string, from []string, to string) (Alert, error) {
	if !validAlertState(to) {
		return Alert{}, errInvalid("state: unknown %q", to)
	}
	set := `state = ?`
	args := []any{to}
	if to == AlertAcked {
		set += `, acked_at = ?`
		args = append(args, formatTS(s.now()))
	}
	q := `UPDATE alerts SET ` + set + ` WHERE id = ?`
	args = append(args, id)
	if len(from) > 0 {
		ph := make([]string, len(from))
		for i, st := range from {
			if !validAlertState(st) {
				return Alert{}, errInvalid("state: unknown %q", st)
			}
			ph[i] = "?"
			args = append(args, st)
		}
		q += ` AND state IN (` + strings.Join(ph, ",") + `)`
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return Alert{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Alert{}, err
	}
	a, err := s.GetAlert(ctx, id)
	if err != nil {
		return Alert{}, err // ErrNotFound when the id is unknown
	}
	if n == 0 {
		return a, ErrStateConflict
	}
	return a, nil
}

// SetAlertTask links (or, with "", unlinks) the alert to a task. It does
// not touch state. ErrNotFound when the alert does not exist.
func (s *Store) SetAlertTask(ctx context.Context, id, taskID string) (Alert, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE alerts SET task_id = ? WHERE id = ?`, nullStr(taskID), id)
	if err != nil {
		return Alert{}, err
	}
	if err := mustAffect(res); err != nil {
		return Alert{}, err
	}
	return s.GetAlert(ctx, id)
}

// ExpireAlerts moves every OPEN alert whose expires_at <= now to "expired"
// and returns them (post-update). Never-expiring alerts (NULL expires_at)
// are untouched. This is the storage primitive behind lazy-on-read expiry
// and the expiry ticker; "now" comes from the caller's clock.
func (s *Store) ExpireAlerts(ctx context.Context, now time.Time) ([]Alert, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cut := formatTS(now)
	rows, err := tx.QueryContext(ctx,
		`SELECT `+alertCols+` FROM alerts
		 WHERE state = 'open' AND expires_at IS NOT NULL AND expires_at <= ?
		 ORDER BY priority ASC, created_at DESC, id DESC`, cut)
	if err != nil {
		return nil, err
	}
	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		a.State = AlertExpired
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(out) == 0 {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE alerts SET state = 'expired'
		 WHERE state = 'open' AND expires_at IS NOT NULL AND expires_at <= ?`, cut); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
