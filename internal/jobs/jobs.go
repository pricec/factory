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

// Job states (spec §6.1). cancelled and superseded are the cancel API's
// terminal states (2026-11-15 amendment, spec §6.9): a real state change
// recorded through the transition machinery, never a direct DB write.
const (
	StatePending    = "pending"
	StateDispatched = "dispatched"
	StateRunning    = "running"
	StateDone       = "done"
	StateFailed     = "failed"
	StateTimedOut   = "timed_out"
	StateDead       = "dead"
	StateCancelled  = "cancelled"
	StateSuperseded = "superseded"
	// StateHeld is the token-throttle dispatch gate's queue (S3): a job
	// created while usage was ORANGE/RED (or spaced/capped) waits here
	// until the releaser moves it to pending. Non-terminal and persisted,
	// so held jobs survive a factory restart.
	StateHeld = "held"
)

// legal transitions (spec §6.1, with #10's retry/dead edges and the
// cancel/supersede edges from every non-terminal state).
var legal = map[string]map[string]bool{
	StateHeld:       {StatePending: true, StateCancelled: true, StateSuperseded: true},
	StatePending:    {StateDispatched: true, StateCancelled: true, StateSuperseded: true},
	StateDispatched: {StateRunning: true, StateCancelled: true, StateSuperseded: true},
	StateRunning:    {StateDone: true, StateFailed: true, StateTimedOut: true, StateCancelled: true, StateSuperseded: true},
	StateFailed:     {StateDispatched: true, StateDead: true, StateCancelled: true, StateSuperseded: true},
}

// Terminal reports whether state admits no further transitions.
func Terminal(state string) bool {
	return state == StateDone || state == StateTimedOut || state == StateDead ||
		state == StateCancelled || state == StateSuperseded
}

// AllStates is every state, for validation and tests.
var AllStates = []string{
	StatePending, StateDispatched, StateRunning, StateDone, StateFailed,
	StateTimedOut, StateDead, StateCancelled, StateSuperseded, StateHeld,
}

const (
	// timeoutGrace is the sweeper's grace past a job's timeout_s (spec §6.2).
	timeoutGrace = 60 * time.Second
	// offlineAfter is how long since last_seen before a foreman is marked
	// offline (spec §6.3). 90s covers the foreman's 60s capabilities push
	// cadence plus jitter and one missed push, so a healthy foreman never
	// flickers offline between pushes; a truly dead foreman still goes
	// offline within 90s.
	offlineAfter = 90 * time.Second
	// reconcileGrace is how long a running job must have been quiet
	// before a foreman that does not claim it is believed (spec §6.2):
	// covers a result still in flight when the foreman's list was built.
	reconcileGrace = 2 * time.Minute
	// offlineGrace is how long a foreman must have been unseen before
	// the jobs the server still has running on it are reaped (spec §6.2).
	offlineGrace = 10 * time.Minute
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
	// onCancelled, if set, fires after Cancel moves a dispatched or running
	// job to cancelled/superseded (the worker may still be alive).
	onCancelled func(job Job)
	// admission, if set, is the dispatch gate consulted by every create
	// path (S3 token throttle; see Admit).
	admission Admission
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
CREATE TABLE IF NOT EXISTS secret_grants (
	job_id     TEXT NOT NULL,
	name       TEXT NOT NULL,
	granted_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_secret_grants_job ON secret_grants(job_id);
`
	if _, err := db.Exec(ddl); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "jobs", "held_reason", "TEXT"); err != nil {
		return err
	}
	if _, err := db.Exec(mergeDDL); err != nil {
		return err
	}
	if _, err := db.Exec(settingsDDL); err != nil {
		return err
	}
	if _, err := db.Exec(usageDDL); err != nil {
		return err
	}
	return addColumnIfMissing(db, "usage_events", "duration_ms", "INTEGER NOT NULL DEFAULT 0")
}

// addColumnIfMissing runs ALTER TABLE ... ADD COLUMN unless the column is
// already present (SQLite has no ADD COLUMN IF NOT EXISTS).
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl)
	return err
}

// DB exposes the underlying database for read-mostly collaborators that
// keep their own tables in jobs.db (the auditor's snapshot and
// fingerprints, spec §6.12).
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// SetOnTerminal installs the terminal-transition hook (spec §6.1).
func (s *Store) SetOnTerminal(fn func(jobID, finalState string)) { s.onTerminal = fn }

// SetOnCancelled installs the hook fired when a dispatched or running job
// is cancelled or superseded, so its worker can be stopped.
func (s *Store) SetOnCancelled(fn func(job Job)) { s.onCancelled = fn }

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
	// HeldReason explains why a held job waits (S3 dispatch gate), e.g.
	// "usage ORANGE: 5h 88%, resets 23:30Z". Empty outside StateHeld.
	HeldReason string `json:"held_reason,omitempty"`
	// GrantedSecrets are the NAMES of credentials handed to the job's
	// foreman (spec §13.2 audit); never values. Filled by Get only.
	GrantedSecrets []string     `json:"granted_secrets,omitempty"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      string       `json:"updated_at"`
	Spec           wire.JobSpec `json:"spec"`
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

// RegisterSpawned makes a foreman-spawned job visible in the registry
// (spec §6.2, 2026-11-15 spawn-visibility amendment): foreman-local spawns
// (the §9.3 spawn_worker seam) mint their JobSpec on the foreman, so the
// factory never created a row for them and their events and forwarded
// results were ignored as unknown - the jobs existed only in the foreman's
// local jobs/ store. The foreman's accepted job.event carries the spec;
// this insert-if-absent path creates the row in state dispatched - the
// state the factory itself would have set had it dispatched the job -
// attributed to the spawning foreman. The subsequent started event drives
// dispatched->running and the forwarded job.result applies
// terminal-idempotently: the ordinary lifecycle from there. Insert-if-
// absent, never an update: re-delivered accepted events (at-least-once
// posting, §4) and jobs the factory created itself are no-ops, so
// registration can neither duplicate nor clobber a row. It returns whether
// it created the row.
func (s *Store) RegisterSpawned(ctx context.Context, spec wire.JobSpec, foremanID string) (bool, error) {
	if err := spec.Validate(); err != nil {
		return false, fmt.Errorf("register spawned job: %w", err)
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return false, fmt.Errorf("register spawned job %s: %w", spec.ID, err)
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, spec, state, foreman_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		spec.ID, string(specJSON), StateDispatched, foremanID, now, now)
	if err != nil {
		return false, fmt.Errorf("register spawned job %s: %w", spec.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("register spawned job %s: %w", spec.ID, err)
	}
	return n > 0, nil
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

	// Harness-captured usage (usage_events) always overrides whatever the
	// agent reported on the result.
	resUsage := res.Usage
	if u, ok, err := s.jobRollup(ctx, res.JobID); err != nil {
		return err
	} else if ok {
		resUsage = &u
	}
	usageJSON, err := json.Marshal(resUsage)
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
	} else if res.Error != nil && res.Error.Kind == wire.KindSuperseded {
		// A worker standing down as a duplicate/superseded is not a
		// failure and must never be retried (spec §6.4).
		to = StateSuperseded
	}
	lastErr := ""
	if res.Error != nil {
		lastErr = res.Error.Message
	}

	if !legal[StateRunning][to] {
		log.Printf("jobs: ignoring illegal transition %s -> %s for %s", StateRunning, to, res.JobID)
		return nil
	}
	// State and result payload land in ONE statement (compare-and-swap on
	// running): a reader that sees the terminal state must also see the
	// results and last_error. Two statements let a poller observe done
	// with empty results.
	upd, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, usage = ?, results = ?, last_error = ?, updated_at = ? WHERE id = ? AND state = ?`,
		to, string(usageJSON), string(mustJSON(results)), lastErr, s.now(), res.JobID, StateRunning)
	if err != nil {
		return err
	}
	if n, err := upd.RowsAffected(); err != nil || n == 0 { // raced with another writer
		return err
	}
	// A harness kill at the live spend cap flags the job over budget (the
	// post-hoc check is strictly '>' and the live kill fires at '>=').
	if res.Error != nil && res.Error.Reason == wire.ReasonBudget {
		if err := s.MarkOverBudget(ctx, res.JobID); err != nil {
			return err
		}
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
		IFNULL(next_attempt_at, ''), IFNULL(last_error, ''), usage, results, over_budget, created_at, updated_at,
		IFNULL(held_reason, '')
		FROM jobs WHERE id = ?`, id)
	j, ok, err := scanJob(row)
	if err != nil || !ok {
		return j, ok, err
	}
	j.GrantedSecrets, err = s.GrantedSecrets(ctx, id)
	return j, true, err
}

// List returns jobs filtered by state and/or foreman, newest first
// (spec §6.9).
func (s *Store) List(ctx context.Context, state, foremanID string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id, spec, state, foreman_id, attempts,
		IFNULL(next_attempt_at, ''), IFNULL(last_error, ''), usage, results, over_budget, created_at, updated_at,
		IFNULL(held_reason, '')
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
		&nextAttempt, &lastErr, &usagePtr, &resultsJSON, &overBudget, &j.CreatedAt, &j.UpdatedAt, &j.HeldReason); err != nil {
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
