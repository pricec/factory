// Package jobs implements the persisted job state machine, foremen
// registry, and liveness (spec §6.1-§6.3): pending -> dispatched ->
// running -> terminal, illegal transitions logged and ignored, sweeper
// timeouts, and the control-plane HTTP API.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/pricec/factory/internal/wire"
	_ "modernc.org/sqlite"
)

// Job states (spec §6.1).
const (
	StatePending    = "pending"
	StateDispatched = "dispatched"
	StateRunning    = "running"
	StateDone       = "done"
	StateFailed     = "failed"
	StateTimedOut   = "timed_out"
	StateDead       = "dead"
)

// legal transitions (spec §6.1, with #10's retry/dead edges).
var legal = map[string]map[string]bool{
	StatePending:    {StateDispatched: true},
	StateDispatched: {StateRunning: true},
	StateRunning:    {StateDone: true, StateFailed: true, StateTimedOut: true},
	StateFailed:     {StateDispatched: true, StateDead: true},
}

// Terminal reports whether state admits no further transitions.
func Terminal(state string) bool {
	return state == StateDone || state == StateTimedOut || state == StateDead
}

// AllStates is every state, for validation and tests.
var AllStates = []string{StatePending, StateDispatched, StateRunning, StateDone, StateFailed, StateTimedOut, StateDead}

const (
	// timeoutGrace is the sweeper's grace past a job's timeout_s (spec §6.2).
	timeoutGrace = 60 * time.Second
	// offlineAfter is how long since last_seen before a foreman is marked
	// offline (spec §6.3). 90s covers the foreman's 60s capabilities push
	// cadence plus jitter and one missed push, so a healthy foreman never
	// flickers offline between pushes; a truly dead foreman still goes
	// offline within 90s.
	offlineAfter = 90 * time.Second
)

// Store is the jobs + foremen database. Safe for concurrent use.
type Store struct {
	db *sql.DB
	// Now is the clock; injectable for sweeper/liveness tests.
	Now func() time.Time
	// onTerminal, if set, fires exactly once per terminal transition
	// (spec §6.1; #11 registers InvalidateJob, #10 registers retry
	// scheduling via it).
	onTerminal func(jobID, finalState string)
}

const dsnSuffix = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"

// Open opens (creating if needed) the jobs database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+dsnSuffix)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, Now: func() time.Time { return time.Now().UTC() }}, nil
}

func migrate(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS jobs (
	id              TEXT PRIMARY KEY,
	spec            TEXT NOT NULL,
	state           TEXT NOT NULL,
	foreman_id      TEXT NOT NULL DEFAULT '',
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt_at TEXT,
	last_error      TEXT,
	usage           TEXT,
	results         TEXT NOT NULL DEFAULT '[]',
	over_budget     INTEGER NOT NULL DEFAULT 0,
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_state ON jobs(state);
CREATE INDEX IF NOT EXISTS idx_jobs_foreman ON jobs(foreman_id);
CREATE TABLE IF NOT EXISTS foremen (
	id           TEXT PRIMARY KEY,
	capabilities TEXT NOT NULL DEFAULT '{}',
	last_seen    TEXT,
	online       INTEGER NOT NULL DEFAULT 0
);
`
	_, err := db.Exec(ddl)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// SetOnTerminal installs the terminal-transition hook (spec §6.1).
func (s *Store) SetOnTerminal(fn func(jobID, finalState string)) { s.onTerminal = fn }

func (s *Store) now() string { return s.Now().UTC().Format(time.RFC3339) }

// Job is the persisted record (spec §6.1, §6.9).
type Job struct {
	ID          string          `json:"id"`
	State       string          `json:"state"`
	ForemanID   string          `json:"foreman_id"`
	Attempts    int             `json:"attempts"`
	NextAttempt string          `json:"next_attempt_at,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	Usage       json.RawMessage `json:"usage,omitempty"`
	Results     json.RawMessage `json:"results"`
	OverBudget  bool            `json:"over_budget"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	Spec        wire.JobSpec    `json:"spec"`
}

// Create stores spec as a pending job (spec §6.1).
func (s *Store) Create(ctx context.Context, spec wire.JobSpec) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, spec, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		spec.ID, string(specJSON), StatePending, now, now)
	return err
}

// Dispatch moves a pending (or, for retries, failed) job to dispatched
// under foremanID (spec §6.1).
func (s *Store) Dispatch(ctx context.Context, id, foremanID string) error {
	_, applied, err := s.transition(ctx, id, StateDispatched)
	if err != nil {
		return err
	}
	if !applied {
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE jobs SET foreman_id = ?, updated_at = ? WHERE id = ?`,
		foremanID, s.now(), id)
	return err
}

// ApplyEvent applies a job.event envelope payload (spec §6.2): started maps
// to the dispatched->running transition; accepted is informational.
// Unknown events and illegal transitions are logged and ignored.
func (s *Store) ApplyEvent(ctx context.Context, jobID, event string) error {
	switch event {
	case "started":
		_, _, err := s.transition(ctx, jobID, StateRunning)
		return err
	case "accepted":
		_, err := s.db.ExecContext(ctx,
			`UPDATE jobs SET updated_at = ? WHERE id = ?`, s.now(), jobID)
		return err
	}
	log.Printf("jobs: ignoring unknown job.event %q for %s", event, jobID)
	return nil
}

// ApplyResult applies a job.result (spec §6.2, §6.4): terminal-idempotent -
// results only apply to running jobs; anything else is a logged no-op.
// Usage is overwritten last-wins; per-attempt results are appended.
func (s *Store) ApplyResult(ctx context.Context, res wire.JobResult) error {
	job, ok, err := s.Get(ctx, res.JobID)
	if err != nil || !ok {
		if err != nil {
			return err
		}
		log.Printf("jobs: result for unknown job %s ignored", res.JobID)
		return nil
	}
	if job.State != StateRunning {
		log.Printf("jobs: result for %s in state %s ignored (terminal-idempotent)", res.JobID, job.State)
		return nil
	}

	usageJSON, err := json.Marshal(res.Usage)
	if err != nil {
		return err
	}
	var results []json.RawMessage
	if err := json.Unmarshal(job.Results, &results); err != nil {
		results = nil
	}
	resJSON, err := json.Marshal(res)
	if err != nil {
		return err
	}
	results = append(results, resJSON)

	to := StateFailed
	if res.Status == wire.StatusDone {
		to = StateDone
	}
	lastErr := ""
	if res.Error != nil {
		lastErr = res.Error.Message
	}

	_, applied, err := s.transition(ctx, res.JobID, to)
	if err != nil || !applied {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET usage = ?, results = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		string(usageJSON), string(mustJSON(results)), lastErr, s.now(), res.JobID); err != nil {
		return err
	}
	if Terminal(to) {
		s.fireTerminal(res.JobID, to)
	}
	return nil
}

// MarkDead moves a failed job to dead (#10's exhausted-retries edge,
// exposed now so the transition table is complete).
func (s *Store) MarkDead(ctx context.Context, id string) error {
	_, applied, err := s.transition(ctx, id, StateDead)
	if err != nil {
		return err
	}
	if applied {
		s.fireTerminal(id, StateDead)
	}
	return nil
}

// transition applies from->to if legal, with a compare-and-swap guard.
// Illegal transitions are logged and ignored (never an error, spec §6.1).
func (s *Store) transition(ctx context.Context, id, to string) (from string, applied bool, err error) {
	var state string
	err = s.db.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !legal[state][to] {
		log.Printf("jobs: ignoring illegal transition %s -> %s for %s", state, to, id)
		return state, false, nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		to, s.now(), id, state)
	if err != nil {
		return state, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return state, false, err
	}
	if n == 0 { // raced with another writer; treat as not applied
		return state, false, nil
	}
	return state, true, nil
}

func (s *Store) fireTerminal(jobID, finalState string) {
	if s.onTerminal != nil {
		s.onTerminal(jobID, finalState)
	}
}

// Get returns the full job record; ok is false when absent.
func (s *Store) Get(ctx context.Context, id string) (Job, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, spec, state, foreman_id, attempts,
		IFNULL(next_attempt_at, ''), IFNULL(last_error, ''), usage, results, over_budget, created_at, updated_at
		FROM jobs WHERE id = ?`, id)
	return scanJob(row)
}

// List returns jobs filtered by state and/or foreman, newest first
// (spec §6.9).
func (s *Store) List(ctx context.Context, state, foremanID string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id, spec, state, foreman_id, attempts,
		IFNULL(next_attempt_at, ''), IFNULL(last_error, ''), usage, results, over_budget, created_at, updated_at
		FROM jobs`
	var conds []string
	var args []any
	if state != "" {
		conds = append(conds, "state = ?")
		args = append(args, state)
	}
	if foremanID != "" {
		conds = append(conds, "foreman_id = ?")
		args = append(args, foremanID)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
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

type rowScanner interface{ Scan(dest ...any) error }

func scanJob(row rowScanner) (Job, bool, error) {
	var j Job
	var specJSON, resultsJSON string
	var usage, lastErr, nextAttempt string
	var overBudget int
	var usagePtr sql.NullString
	if err := row.Scan(&j.ID, &specJSON, &j.State, &j.ForemanID, &j.Attempts,
		&nextAttempt, &lastErr, &usagePtr, &resultsJSON, &overBudget, &j.CreatedAt, &j.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, false, nil
		}
		return Job{}, false, err
	}
	if err := json.Unmarshal([]byte(specJSON), &j.Spec); err != nil {
		return Job{}, false, fmt.Errorf("job %s spec: %w", j.ID, err)
	}
	if usagePtr.Valid {
		usage = usagePtr.String
	}
	j.Usage = json.RawMessage(usage)
	j.Results = json.RawMessage(resultsJSON)
	j.NextAttempt = nextAttempt
	j.LastError = lastErr
	j.OverBudget = overBudget != 0
	return j, true, nil
}

// UpsertForeman registers or refreshes a foreman's capabilities (pairing
// and refresh path, spec §6.7; #12 wires the matching, #11 the pairing).
// Every upsert is a liveness signal too (spec §6.3): the conflict branch
// bumps last_seen and marks the foreman online, so the foreman's steady
// 60s capabilities push keeps it online even with no other traffic (the
// 90s offline threshold is comfortably above the 60s push cadence).
func (s *Store) UpsertForeman(ctx context.Context, id string, capabilities json.RawMessage) error {
	if len(capabilities) == 0 {
		capabilities = json.RawMessage("{}")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO foremen (id, capabilities, last_seen, online) VALUES (?, ?, ?, 1)
		 ON CONFLICT(id) DO UPDATE SET capabilities = excluded.capabilities,
			last_seen = excluded.last_seen, online = 1`,
		id, string(capabilities), s.now())
	return err
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
