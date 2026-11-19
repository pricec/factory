package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/pricec/factory/internal/wire"
)

// ActiveFootprints returns the declared footprints (jobs with non-empty
// touches) that can still change the repo, for the conflict coordinator:
//
//   - every non-terminal job: held, pending, dispatched, running and failed
//     (a failed job awaiting its retry is non-terminal). Pending and held
//     jobs are included so that two conflicting jobs released in one
//     releaser pass cannot both go out;
//   - every done job whose merge request has not landed or ended yet
//     (queued or merging): its branch is not on master, so it still
//     conflicts. "Blocking until landed, not merely done".
//
// A done job whose request landed, needs_attention or was cancelled, and
// any other terminal job, no longer blocks.
func (s *Store) ActiveFootprints(ctx context.Context) ([]Footprint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT j.id, j.spec, j.state, j.created_at, IFNULL(m.id, '')
		FROM jobs j
		LEFT JOIN merge_requests m ON m.source_job_id = j.id AND m.state IN (?, ?) AND j.state = ?
		  AND NOT (m.state = ? AND EXISTS (SELECT 1 FROM jobs mj WHERE mj.id = m.job_id AND mj.state IN (?, ?, ?, ?)))
		WHERE IFNULL(json_array_length(json_extract(j.spec, '$.touches')), 0) > 0
		  AND (j.state IN (?, ?, ?, ?, ?) OR (j.state = ? AND m.id IS NOT NULL))
		GROUP BY j.id`,
		MergeQueued, MergeMerging, StateDone,
		MergeMerging, StateDead, StateTimedOut, StateCancelled, StateSuperseded,
		StateHeld, StatePending, StateDispatched, StateRunning, StateFailed, StateDone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Footprint
	for rows.Next() {
		var f Footprint
		var specJSON string
		if err := rows.Scan(&f.JobID, &specJSON, &f.State, &f.CreatedAt, &f.MergeRequestID); err != nil {
			return nil, err
		}
		var spec wire.JobSpec
		if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
			continue // unreadable spec: no footprint
		}
		f.Touches, f.Priority = spec.Touches, spec.Priority
		f.Repo = SpecRepo(spec)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // the store has one connection: free it before the merge lookups
	live := out[:0]
	for _, f := range out {
		if IsTerminalState(f.State) && f.State != StateDone {
			continue // belt and braces: a terminal job never holds a lock
		}
		if f.MergeRequestID != "" {
			orphan, err := s.mergeOrphaned(ctx, f.MergeRequestID)
			if err != nil {
				return nil, err
			}
			if orphan {
				continue // its merge can never run: the lock is stale
			}
		}
		live = append(live, f)
	}
	return live, nil
}

// ConflictSince returns when the job's conflict wait began ("" = not
// waiting on a conflict).
func (s *Store) ConflictSince(ctx context.Context, id string) (string, error) {
	var since sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT conflict_since FROM jobs WHERE id = ?`, id).Scan(&since)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return since.String, err
}

// MarkConflictSince records the start of a job's conflict wait (kept if
// already set; the persisted stamp survives restarts so the max-wait bound
// is real). at is an RFC3339 UTC time.
func (s *Store) MarkConflictSince(ctx context.Context, id, at string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET conflict_since = ? WHERE id = ? AND conflict_since IS NULL`, at, id)
	return err
}

// ClearConflictSince ends a job's conflict wait.
func (s *Store) ClearConflictSince(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET conflict_since = NULL WHERE id = ? AND conflict_since IS NOT NULL`, id)
	return err
}

// CountHeldByKind returns how many held jobs there are per hold kind
// ("usage", "conflict", "claude_auth"; see HeldKind).
func (s *Store) CountHeldByKind(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT IFNULL(held_reason, '') FROM jobs WHERE state = ?`, StateHeld)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out[HeldKind(r)]++
	}
	return out, rows.Err()
}

// SpecRepo is the normalized target repo of a job spec: spec.Repo, else a
// "repo" string in the JSON payload; "" when unknown.
func SpecRepo(spec wire.JobSpec) string {
	if r := NormalizeRepo(spec.Repo); r != "" {
		return r
	}
	var p struct {
		Repo string `json:"repo"`
	}
	if len(spec.Payload) > 0 && json.Unmarshal(spec.Payload, &p) == nil {
		return NormalizeRepo(p.Repo)
	}
	return ""
}

// IsTerminalState reports whether a job state is final (no further work
// can change the repo from the job itself).
func IsTerminalState(state string) bool {
	switch state {
	case StateDone, StateDead, StateTimedOut, StateCancelled, StateSuperseded:
		return true
	}
	return false
}

// mergeOrphaned reports whether a queued merge request can never proceed:
// it waits (after_branches) on a branch that has no queued, merging or
// landed request in the same repo (e.g. the dependency was filed under
// another repo, cancelled or needs attention). Such a request would block
// its done job's footprint forever, so the lock is treated as stale.
func (s *Store) mergeOrphaned(ctx context.Context, mrID string) (bool, error) {
	var repo, state, after string
	err := s.db.QueryRowContext(ctx, `SELECT repo, state, after_branches FROM merge_requests WHERE id = ?`, mrID).
		Scan(&repo, &state, &after)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if state != MergeQueued {
		return false, nil
	}
	for _, b := range strings.Split(after, "\n") {
		if b == "" {
			continue
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM merge_requests WHERE repo = ? AND branch = ? AND state IN (?, ?, ?)`,
			repo, b, MergeQueued, MergeMerging, MergeLanded).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
	}
	return false, nil
}
