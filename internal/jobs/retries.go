package jobs

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"
)

// Retry policy and dead-letter machinery (#10, spec §6.4): the kind ->
// max-retries table, the fixed backoff curve, retry arming on failure,
// the retry picker, the requeue path, and the dead-letter API.

// retryPolicy is the normative error-kind -> allowed-retries table (spec
// §6.4). Unknown kinds get 0 retries (dead-letter immediately).
var retryPolicy = map[string]int{
	"validation": 0,
	"timeout":    1,
	"crash":      2,
	"llm":        2,
	"other":      2,
}

// MaxRetries returns how many retries a failure kind allows (spec §6.4):
// validation 0, timeout 1, crash|llm|other 2, unknown kinds 0.
func MaxRetries(kind string) int {
	if n, ok := retryPolicy[kind]; ok {
		return n
	}
	return 0
}

// backoffCurve is the fixed retry wait curve (spec §6.4): 30s, 2m, 8m.
var backoffCurve = []time.Duration{30 * time.Second, 2 * time.Minute, 8 * time.Minute}

// Backoff returns the wait before retry attempt n (1-based); the curve is
// 30s, 2m, 8m, capped at 8m for attempts beyond the curve.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		return backoffCurve[0]
	}
	if attempt > len(backoffCurve) {
		return backoffCurve[len(backoffCurve)-1]
	}
	return backoffCurve[attempt-1]
}

// MarkFailedForRetry arms or exhausts a just-failed job's retry (spec §6.4).
// Called by the integrator after ApplyResult leaves the job failed. Reads
// the job's attempts; if attempts >= MaxRetries(kind) the job transitions
// failed->dead (transition + terminal hook); otherwise next_attempt_at is
// set to now + Backoff(attempts+1). Non-failed (e.g. already-terminal) jobs
// are a logged no-op.
func (s *Store) MarkFailedForRetry(ctx context.Context, jobID, kind string) error {
	var state string
	var attempts int
	err := s.db.QueryRowContext(ctx,
		`SELECT state, attempts FROM jobs WHERE id = ?`, jobID).Scan(&state, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		log.Printf("jobs: retry scheduling for unknown job %s ignored", jobID)
		return nil
	}
	if err != nil {
		return err
	}
	if state != StateFailed {
		log.Printf("jobs: retry scheduling for %s in state %s ignored (not failed)", jobID, state)
		return nil
	}
	if attempts >= MaxRetries(kind) {
		// Exhausted: dead-letter via the legal failed->dead transition,
		// firing the terminal hook (MarkDead does both).
		return s.MarkDead(ctx, jobID)
	}
	next := s.Now().Add(Backoff(attempts + 1)).UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx,
		`UPDATE jobs SET next_attempt_at = ?, updated_at = ? WHERE id = ? AND state = ?`,
		next, s.now(), jobID, StateFailed)
	return err
}

// Requeue resets a dead or failed job back to pending with attempts cleared
// for a fresh run (spec §6.4). NOTE: this intentionally bypasses transition():
// dead->pending and failed->pending are #10-owned edges that are not in the
// shared legal map, so the UPDATE carries its own state guard and fires no
// hooks. Jobs in any other state are a logged no-op.
func (s *Store) Requeue(ctx context.Context, jobID string) error {
	job, ok, err := s.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if !ok {
		log.Printf("jobs: requeue of unknown job %s ignored", jobID)
		return nil
	}
	if job.State != StateDead && job.State != StateFailed {
		log.Printf("jobs: requeue of %s in state %s ignored", jobID, job.State)
		return nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, attempts = 0, next_attempt_at = NULL, updated_at = ?
		 WHERE id = ? AND state IN (?, ?)`,
		StatePending, s.now(), jobID, StateDead, StateFailed)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		log.Printf("jobs: requeue of %s raced out of dead/failed; ignored", jobID)
	}
	return nil
}

// RetryPicker re-dispatches failed jobs whose backoff has elapsed (spec
// §6.4). Re-dispatch reuses the SAME job id and its existing foreman_id;
// foreman matching is #12/#8's concern.
type RetryPicker struct {
	store *Store
	// Tick is the loop interval; default 10s.
	Tick time.Duration
	// Hold, when set and true, skips the pass (usage-limit pause, S5);
	// due retries wait and are not consumed.
	Hold func(ctx context.Context) bool
}

// NewRetryPicker creates a picker over store.
func NewRetryPicker(store *Store) *RetryPicker {
	return &RetryPicker{store: store, Tick: 10 * time.Second}
}

// Pump runs one pick pass: failed jobs with next_attempt_at <= now get
// attempts++, next_attempt_at cleared, and are re-dispatched (failed ->
// dispatched) to their previous foreman. A foreman-pinned job (§6.7)
// retries to its pin by construction: its previous foreman IS the pin
// (the pin is the only foreman it ever dispatched to), so retries never
// consult the Matcher. Returns how many were re-dispatched.
func (p *RetryPicker) Pump(ctx context.Context) (int, error) {
	if p.Hold != nil && p.Hold(ctx) {
		return 0, nil
	}
	rows, err := p.store.db.QueryContext(ctx,
		`SELECT id, attempts, next_attempt_at, foreman_id FROM jobs
		 WHERE state = ? AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?`,
		StateFailed, p.store.now())
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id      string
		foreman string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var attempts int
		var due string
		if err := rows.Scan(&c.id, &attempts, &due, &c.foreman); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	redispatched := 0
	for _, c := range candidates {
		res, err := p.store.db.ExecContext(ctx,
			`UPDATE jobs SET attempts = attempts + 1, next_attempt_at = NULL, updated_at = ?
			 WHERE id = ? AND state = ?`,
			p.store.now(), c.id, StateFailed)
		if err != nil {
			return redispatched, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // raced out of failed between the scan and this update
		}
		if err := p.store.Dispatch(ctx, c.id, c.foreman); err != nil {
			return redispatched, err
		}
		log.Printf("jobs: retrying %s to %s", c.id, c.foreman)
		redispatched++
	}
	return redispatched, nil
}

// Run pumps every Tick until ctx is done.
func (p *RetryPicker) Run(ctx context.Context) error {
	tick := p.Tick
	if tick <= 0 {
		tick = 10 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if _, err := p.Pump(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// ListDead returns dead-lettered jobs newest first (spec §6.9).
func (s *Store) ListDead(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, spec, state, foreman_id, attempts,
		IFNULL(next_attempt_at, ''), IFNULL(last_error, ''), usage, results, over_budget, created_at, updated_at,
		IFNULL(held_reason, '')
		FROM jobs WHERE state = ? ORDER BY created_at DESC LIMIT ?`, StateDead, limit)
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

// DeadLetterHandler mounts the dead-letter API (spec §6.4, §6.9):
// GET /v1/deadletter and POST /v1/deadletter/{id}/requeue.
func (s *Store) DeadLetterHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/deadletter", s.handleDeadLetterList)
	mux.HandleFunc("POST /v1/deadletter/{id}/requeue", s.handleDeadLetterRequeue)
	return mux
}

func (s *Store) handleDeadLetterList(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be an integer in [1, 1000]", false)
			return
		}
		limit = n
	}
	dead, err := s.ListDead(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	if dead == nil {
		dead = []Job{}
	}
	writeJSON(w, http.StatusOK, dead)
}

func (s *Store) handleDeadLetterRequeue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok, err := s.Get(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	} else if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such job", false)
		return
	}
	if err := s.Requeue(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	job, _, err := s.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, job)
}
