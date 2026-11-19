// Dismissing a needs_attention merge request (2026-11-19 amendment, spec
// §6.11). needs_attention is terminal and CancelMergeRequest refuses it,
// so a request whose work already landed (or was superseded, or is empty)
// stayed on the operator's list forever. DismissMergeRequest closes such a
// request to cancelled with a required reason and writes an audit row;
// the git-state check and alert resolution live in factoryapp.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrMergeNotDismissable is returned when a request is not needs_attention.
var ErrMergeNotDismissable = errors.New("jobs: only needs_attention merge requests can be dismissed")

// Merge audit actors.
const (
	MergeActorPlanner  = "planner"
	MergeActorOperator = "operator"
	MergeActorSweep    = "sweep"
)

const mergeAuditDDL = `
CREATE TABLE IF NOT EXISTS merge_request_audit (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL,
	action     TEXT NOT NULL,
	actor      TEXT NOT NULL,
	reason     TEXT NOT NULL,
	forced     INTEGER NOT NULL DEFAULT 0,
	from_state TEXT NOT NULL,
	to_state   TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_merge_request_audit_req ON merge_request_audit(request_id);
`

// MergeAudit is one merge_request_audit row.
type MergeAudit struct {
	RequestID string `json:"request_id"`
	Action    string `json:"action"`
	Actor     string `json:"actor"`
	Reason    string `json:"reason"`
	Forced    bool   `json:"forced"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	CreatedAt string `json:"created_at"`
}

// NeedsAttentionMerges returns every needs_attention request, oldest first.
func (s *Store) NeedsAttentionMerges(ctx context.Context) ([]MergeRequest, error) {
	return s.queryMerges(ctx, `SELECT `+mergeCols+` FROM merge_requests WHERE state = ? ORDER BY id ASC`, MergeNeedsAttention)
}

// DismissMergeRequest closes a needs_attention request as cancelled with
// result "dismissed (<actor>[, forced]): <reason>" and an audit row, in one
// transaction. reason is required. ErrMergeNotFound for an unknown id;
// ErrMergeNotDismissable when the request is not (or no longer)
// needs_attention.
func (s *Store) DismissMergeRequest(ctx context.Context, id, actor, reason string, forced bool) (MergeRequest, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return MergeRequest{}, &errBadMerge{"reason is required"}
	}
	cur, ok, err := s.GetMergeRequest(ctx, id)
	if err != nil {
		return MergeRequest{}, err
	}
	if !ok {
		return MergeRequest{}, fmt.Errorf("%w: %s", ErrMergeNotFound, id)
	}
	if cur.State != MergeNeedsAttention {
		return MergeRequest{}, fmt.Errorf("%w: %s is %s", ErrMergeNotDismissable, id, cur.State)
	}
	msg := "dismissed (" + actor
	if forced {
		msg += ", forced"
	}
	msg += "): " + reason
	msg = truncateUTF8(msg, maxMergeResultBytes)
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MergeRequest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE merge_requests SET state = ?, result = ?, updated_at = ? WHERE id = ? AND state = ?`,
		MergeCancelled, msg, now, id, MergeNeedsAttention)
	if err != nil {
		return MergeRequest{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return MergeRequest{}, err
	} else if n == 0 {
		return MergeRequest{}, fmt.Errorf("%w: %s changed state concurrently", ErrMergeNotDismissable, id)
	}
	f := 0
	if forced {
		f = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO merge_request_audit (request_id, action, actor, reason, forced, from_state, to_state, created_at)
		 VALUES (?, 'dismiss', ?, ?, ?, ?, ?, ?)`,
		id, actor, reason, f, MergeNeedsAttention, MergeCancelled, now); err != nil {
		return MergeRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return MergeRequest{}, err
	}
	m, _, err := s.GetMergeRequest(ctx, id)
	return m, err
}

// MergeAuditTrail returns the audit rows of request id, oldest first.
func (s *Store) MergeAuditTrail(ctx context.Context, id string) ([]MergeAudit, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT request_id, action, actor, reason, forced, from_state, to_state, created_at
		 FROM merge_request_audit WHERE request_id = ? ORDER BY seq ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MergeAudit{}
	for rows.Next() {
		var a MergeAudit
		var f int
		if err := rows.Scan(&a.RequestID, &a.Action, &a.Actor, &a.Reason, &f, &a.FromState, &a.ToState, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Forced = f != 0
		out = append(out, a)
	}
	return out, rows.Err()
}
