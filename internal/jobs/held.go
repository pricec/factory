package jobs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pricec/factory/internal/wire"
)

// Held queue (token throttle S3, spec §6.10): the dispatch gate parks new
// work in the persisted `held` state instead of dispatching it, and the
// factory's releaser moves held jobs to pending (held -> pending, a legal
// §6.1 edge) in priority-then-FIFO order once the usage state allows. From
// pending, the ordinary pending picker dispatches. A held job is
// non-terminal: cancel_job / POST /v1/jobs/{id}/cancel move it to
// cancelled/superseded like any other non-terminal job.

// Admission is the dispatch gate hook (S3). taskID is the task the job is
// filed under ("" for none). hold=true parks the job in StateHeld with
// reason. A nil Admission admits everything (the pre-S3 behaviour).
type Admission func(ctx context.Context, spec wire.JobSpec, taskID string) (hold bool, reason string)

// SetAdmission installs the dispatch gate hook consulted by Admit.
func (s *Store) SetAdmission(fn Admission) { s.admission = fn }

// Admit asks the dispatch gate whether a NEW job may proceed to the normal
// match/dispatch path. Every create path (MCP create_job, POST /v1/jobs,
// the scheduler) calls it before Create; a hold means CreateHeld instead.
func (s *Store) Admit(ctx context.Context, spec wire.JobSpec, taskID string) (hold bool, reason string) {
	if s.admission == nil {
		return false, ""
	}
	return s.admission(ctx, spec, taskID)
}

// CreateHeld stores spec as a held job with reason (S3). The job id is
// usable at once: cancel, list and get all work on held jobs.
func (s *Store) CreateHeld(ctx context.Context, spec wire.JobSpec, reason string) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("create held job: %w", err)
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, spec, state, held_reason, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		spec.ID, string(specJSON), StateHeld, reason, now, now)
	return err
}

// Release moves a held job to pending (held -> pending) and clears its
// held_reason; the pending picker dispatches it from there. applied is
// false when the job was no longer held (cancelled meanwhile, or another
// releaser won the race).
func (s *Store) Release(ctx context.Context, id string) (applied bool, err error) {
	_, applied, err = s.transition(ctx, id, StatePending)
	if err != nil || !applied {
		return applied, err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE jobs SET held_reason = NULL, updated_at = ? WHERE id = ? AND state = ?`,
		s.now(), id, StatePending)
	return true, err
}

// SetHeldReason refreshes a held job's reason (the releaser re-evaluates
// every pass, so list_jobs shows why the job is waiting NOW). No-op for
// jobs that left held. updated_at is left alone: it is the hold-since stamp.
func (s *Store) SetHeldReason(ctx context.Context, id, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET held_reason = ? WHERE id = ? AND state = ? AND IFNULL(held_reason, '') != ?`,
		reason, id, StateHeld, reason)
	return err
}

// ListHeld returns held jobs in release order: job priority ascending
// (1 = most urgent), then FIFO by created_at, then id for a stable tie
// break. limit <= 0 means 1000.
func (s *Store) ListHeld(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, spec, state, foreman_id, attempts,
		IFNULL(next_attempt_at, ''), IFNULL(last_error, ''), usage, results, over_budget, created_at, updated_at,
		IFNULL(held_reason, '')
		FROM jobs WHERE state = ?
		ORDER BY CAST(IFNULL(json_extract(spec, '$.priority'), 5) AS INTEGER) ASC, created_at ASC, id ASC
		LIMIT ?`, StateHeld, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, _, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// CountHeld returns how many jobs are held.
func (s *Store) CountHeld(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE state = ?`, StateHeld).Scan(&n)
	return n, err
}

// ActiveJobIDs returns the ids of jobs currently occupying a worker
// (dispatched or running), for the gate's concurrency cap.
func (s *Store) ActiveJobIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM jobs WHERE state IN (?, ?)`, StateDispatched, StateRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
