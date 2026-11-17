package auditor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Record is the persisted state of one fingerprint (audit_findings in
// jobs.db, the factory's existing sqlite persistence).
type Record struct {
	Fingerprint string
	TaskID      string
	Title       string
	// Metric is the value the finding was last FILED at (the 2x rule's
	// baseline); LastMetric the latest observation.
	Metric     float64
	LastMetric float64
	FiledAt    string
	LastSeen   string
	TimesFiled int
	// Mode is "proposal" or "autofix" (what the last filing did).
	Mode string
}

// TaskState is what dedup needs to know about a finding's task.
type TaskState struct {
	Exists      bool
	Open        bool
	CloseReason string
}

// Decision is dedup's verdict for one finding.
type Decision struct {
	File   bool
	Reason string
}

// Decide applies the dedup rule (spec §6.12): file a new fingerprint; never
// re-file one whose task is still open; re-file one whose task was closed
// (by the operator, the planner or the reaper) only when the metric is at
// least WorsenFactor times the metric it was last filed at. A record whose
// task vanished is filed again.
func Decide(f Finding, prior *Record, task TaskState) Decision {
	if prior == nil || prior.TaskID == "" {
		return Decision{File: true, Reason: "new finding"}
	}
	if !task.Exists {
		return Decision{File: true, Reason: "previous task " + prior.TaskID + " no longer exists"}
	}
	if task.Open {
		return Decision{Reason: "already open as " + prior.TaskID}
	}
	base := prior.Metric
	if base <= 0 {
		base = 1
	}
	if f.Metric >= WorsenFactor*base {
		return Decision{File: true, Reason: fmt.Sprintf("worsened %.1fx since %s was closed (%s -> %s)",
			f.Metric/base, prior.TaskID, trimFloat(prior.Metric), trimFloat(f.Metric))}
	}
	return Decision{Reason: fmt.Sprintf("closed as %s (%s); metric %s < %.0fx %s",
		prior.TaskID, orDefault(task.CloseReason, "closed"), trimFloat(f.Metric), WorsenFactor, trimFloat(prior.Metric))}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

const dedupDDL = `
CREATE TABLE IF NOT EXISTS audit_findings (
	fingerprint TEXT PRIMARY KEY,
	task_id     TEXT NOT NULL DEFAULT '',
	title       TEXT NOT NULL DEFAULT '',
	metric      REAL NOT NULL DEFAULT 0,
	last_metric REAL NOT NULL DEFAULT 0,
	filed_at    TEXT NOT NULL DEFAULT '',
	last_seen   TEXT NOT NULL DEFAULT '',
	times_filed INTEGER NOT NULL DEFAULT 0,
	mode        TEXT NOT NULL DEFAULT ''
);`

// FindingStore persists Records in the given database (jobs.db).
type FindingStore struct {
	DB  *sql.DB
	Now func() time.Time
	// ReadOnly skips the table creation (dry run on a read-only handle);
	// a missing table then reads as "no records".
	ReadOnly bool
}

func (s *FindingStore) now() string {
	if s.Now != nil {
		return s.Now().UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// Ensure creates the table (idempotent).
func (s *FindingStore) Ensure(ctx context.Context) error {
	if s.ReadOnly {
		return nil
	}
	_, err := s.DB.ExecContext(ctx, dedupDDL)
	return err
}

// Get returns the record for fp, nil when absent.
func (s *FindingStore) Get(ctx context.Context, fp string) (*Record, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	var r Record
	err := s.DB.QueryRowContext(ctx,
		`SELECT fingerprint, task_id, title, metric, last_metric, filed_at, last_seen, times_filed, mode
		 FROM audit_findings WHERE fingerprint = ?`, fp).
		Scan(&r.Fingerprint, &r.TaskID, &r.Title, &r.Metric, &r.LastMetric, &r.FiledAt, &r.LastSeen, &r.TimesFiled, &r.Mode)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil && s.ReadOnly:
		return nil, nil // no table yet on a never-audited database
	case err != nil:
		return nil, err
	}
	return &r, nil
}

// Seen records an observation that was NOT filed (keeps the baseline).
func (s *FindingStore) Seen(ctx context.Context, f Finding) error {
	if s.ReadOnly {
		return nil
	}
	_, err := s.DB.ExecContext(ctx,
		`UPDATE audit_findings SET last_seen = ?, last_metric = ? WHERE fingerprint = ?`,
		s.now(), f.Metric, f.Fingerprint)
	return err
}

// Filed records that f was filed under taskID (new baseline metric).
func (s *FindingStore) Filed(ctx context.Context, f Finding, taskID, mode string) error {
	if s.ReadOnly {
		return nil
	}
	if err := s.Ensure(ctx); err != nil {
		return err
	}
	now := s.now()
	_, err := s.DB.ExecContext(ctx, `
INSERT INTO audit_findings (fingerprint, task_id, title, metric, last_metric, filed_at, last_seen, times_filed, mode)
VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)
ON CONFLICT(fingerprint) DO UPDATE SET task_id = excluded.task_id, title = excluded.title,
	metric = excluded.metric, last_metric = excluded.last_metric, filed_at = excluded.filed_at,
	last_seen = excluded.last_seen, times_filed = audit_findings.times_filed + 1, mode = excluded.mode`,
		f.Fingerprint, taskID, f.Title, f.Metric, f.Metric, now, now, mode)
	return err
}

// Rebind points fp at a different task (auto-fix converted to proposal).
func (s *FindingStore) Rebind(ctx context.Context, fp, taskID, mode string) error {
	if s.ReadOnly {
		return nil
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE audit_findings SET task_id = ?, mode = ? WHERE fingerprint = ?`, taskID, mode, fp)
	return err
}

// RunJobs lists the jobs of audit run `run` (the audit job itself, whose
// id is the run id, plus every fix job whose payload names the run) as the
// Meter sees them. Unfinished fix jobs count at least their max_cost_usd
// (the reservation they were started with).
func RunJobs(ctx context.Context, db *sql.DB, run string) ([]RunJob, error) {
	rows, err := db.QueryContext(ctx, `
SELECT j.id, j.state, IFNULL(json_extract(j.usage, '$.cost_usd'), 0),
       IFNULL((SELECT SUM(u.cost_usd) FROM usage_events u WHERE u.job_id = j.id), 0),
       IFNULL(json_extract(j.spec, '$.budget.max_cost_usd'), 0), j.id = ?
FROM jobs j
WHERE j.id = ? OR (json_extract(j.spec, '$.created_by') = ? AND json_extract(j.spec, '$.payload.audit_run') = ?)`,
		run, run, CreatedBy, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunJob
	for rows.Next() {
		var j RunJob
		var state string
		var rollup, events, maxCost float64
		var isAudit bool
		if err := rows.Scan(&j.ID, &state, &rollup, &events, &maxCost, &isAudit); err != nil {
			return nil, err
		}
		j.CostUSD = max(rollup, events)
		j.Terminal = state == "done" || state == "timed_out" || state == "dead" || state == "cancelled" || state == "superseded"
		if !isAudit {
			j.ReserveUSD = maxCost
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
