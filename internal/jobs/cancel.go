package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Cancel/supersede (2026-11-15 amendment, spec §6.1, §6.9): a job in any
// non-terminal state moves to the cancelled or superseded terminal state
// through the transition machinery - never a direct DB write. A late
// job.result for a cancelled job is ignored (terminal-idempotent, §6.4);
// v1 has no foreman-side kill, so in-flight worker work is not force-stopped.

// Cancel outcomes: no such job, or the job sits in a terminal state and
// rejects the transition (the HTTP handler surfaces these as 404 / 409,
// the MCP cancel_job tool as tool errors).
var (
	ErrNoSuchJob = errors.New("jobs: no such job")
	ErrTerminal  = errors.New("jobs: job is already terminal")
)

// cancelTargets are the terminal states Cancel may move a job to.
var cancelTargets = map[string]bool{StateCancelled: true, StateSuperseded: true}

// Cancel moves a non-terminal job to `to` (cancelled or superseded) and
// records `reason` in last_error. `to` must be a cancel target; reason is
// stored as "<to>: <reason>" for the notify.job payload and dead-letter
// view. Returns the refreshed job. Errors: ErrNoSuchJob, ErrTerminal
// (rejection - terminal jobs are never re-transitioned), or a storage
// error. The transition is a compare-and-swap on the observed state, so a
// concurrent Dispatch/ApplyResult/sweep wins the race and Cancel classifies
// the result from the fresh read instead of overwriting it.
func (s *Store) Cancel(ctx context.Context, id, to, reason string) (Job, error) {
	if !cancelTargets[to] {
		return Job{}, fmt.Errorf("jobs: cancel target must be cancelled or superseded, got %q", to)
	}
	msg := to
	if reason != "" {
		msg = to + ": " + reason
	}
	for range 3 { // re-read + classify on CAS races; 2 losses are pathological
		var state string
		err := s.db.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, id).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, fmt.Errorf("%w: %s", ErrNoSuchJob, id)
		}
		if err != nil {
			return Job{}, err
		}
		if Terminal(state) {
			return Job{}, fmt.Errorf("%w: %s is %s", ErrTerminal, id, state)
		}
		res, err := s.db.ExecContext(ctx,
			`UPDATE jobs SET state = ?, last_error = ?, next_attempt_at = NULL, updated_at = ?
			 WHERE id = ? AND state = ?`,
			to, msg, s.now(), id, state)
		if err != nil {
			return Job{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return Job{}, err
		}
		if n == 0 {
			continue // raced with another transition; re-read and classify
		}
		s.fireTerminal(id, to)
		job, _, err := s.Get(ctx, id)
		if err != nil {
			return Job{}, err
		}
		return job, nil
	}
	return Job{}, fmt.Errorf("jobs: cancel of %s lost repeated CAS races", id)
}

// handleCancelJob serves POST /v1/jobs/{id}/cancel (spec §6.9): body
// {reason?, terminal_state?} with terminal_state cancelled (default) or
// superseded; an empty body cancels. 200 + job, 404 unknown job,
// 409 terminal job, 400 malformed body.
func (s *Store) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason        string `json:"reason"`
		TerminalState string `json:"terminal_state"`
	}
	// The body is optional: an empty body is a plain cancel.
	if r.Body != nil {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error(), false)
			return
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error(), false)
				return
			}
		}
	}
	to := in.TerminalState
	if to == "" {
		to = StateCancelled
	}
	if !cancelTargets[to] {
		writeError(w, http.StatusBadRequest, "bad_request",
			"terminal_state must be cancelled or superseded", false)
		return
	}
	job, err := s.Cancel(r.Context(), r.PathValue("id"), to, in.Reason)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoSuchJob):
			writeError(w, http.StatusNotFound, "not_found", err.Error(), false)
		case errors.Is(err, ErrTerminal):
			writeError(w, http.StatusConflict, "already_terminal", err.Error(), false)
		default:
			writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		}
		return
	}
	writeJSON(w, http.StatusOK, job)
}
