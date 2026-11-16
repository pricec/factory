package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// usageDDL is the append-only usage_events table: one row per captured CLI
// run (worker, planner or brain). jobs.usage stays the per-job rollup.
const usageDDL = `
CREATE TABLE IF NOT EXISTS usage_events (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	ts           TEXT NOT NULL,
	kind         TEXT NOT NULL,
	job_id       TEXT NOT NULL DEFAULT '',
	task_id      TEXT NOT NULL DEFAULT '',
	foreman_id   TEXT NOT NULL DEFAULT '',
	model        TEXT NOT NULL DEFAULT '',
	tier         TEXT NOT NULL DEFAULT '',
	input        INTEGER NOT NULL DEFAULT 0,
	output       INTEGER NOT NULL DEFAULT 0,
	cache_create INTEGER NOT NULL DEFAULT 0,
	cache_read   INTEGER NOT NULL DEFAULT 0,
	cost_usd     REAL NOT NULL DEFAULT 0,
	session_id   TEXT NOT NULL DEFAULT '',
	limit_raw    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_usage_events_ts ON usage_events(ts);
CREATE INDEX IF NOT EXISTS idx_usage_events_job ON usage_events(job_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_session
	ON usage_events(kind, job_id, session_id) WHERE session_id != '' AND job_id != '';
`

// RecordUsage appends one usage event (append-only; a redelivered event
// with the same kind+job+session is a no-op) and, for events tied to a
// job, refreshes jobs.usage as the sum over that job's events. The
// harness-captured rollup therefore overrides anything the agent reported.
func (s *Store) RecordUsage(ctx context.Context, ev wire.UsageEvent) error {
	switch ev.Kind {
	case wire.UsageKindWorker, wire.UsageKindPlanner, wire.UsageKindBrain:
	default:
		return fmt.Errorf("jobs: usage event kind %q invalid", ev.Kind)
	}
	if ev.ForemanID == "" && ev.JobID != "" {
		if j, ok, err := s.Get(ctx, ev.JobID); err == nil && ok {
			ev.ForemanID = j.ForemanID
		}
	}
	// limit_raw holds the rate_limit_info JSON when the run had one (the
	// stream-json signal), else the raw limit-error text.
	limitRaw := ev.Limit
	if ev.RateLimit != "" {
		limitRaw = ev.RateLimit
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_events (ts, kind, job_id, task_id, foreman_id, model, tier,
			input, output, cache_create, cache_read, cost_usd, session_id, limit_raw)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		s.now(), ev.Kind, ev.JobID, ev.TaskID, ev.ForemanID, ev.Model, ev.Tier,
		ev.Input, ev.Output, ev.CacheCreate, ev.CacheRead, ev.CostUSD, ev.SessionID, limitRaw)
	if err != nil {
		return fmt.Errorf("jobs: record usage: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 || ev.JobID == "" || ev.Kind != wire.UsageKindWorker {
		return nil
	}
	return s.refreshUsageRollup(ctx, ev.JobID)
}

// jobRollup sums a job's worker usage events; ok is false when none exist.
func (s *Store) jobRollup(ctx context.Context, jobID string) (u wire.Usage, ok bool, err error) {
	var n int
	var in, out sql.NullInt64
	var cost sql.NullFloat64
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), SUM(input + cache_create + cache_read), SUM(output), SUM(cost_usd)
		 FROM usage_events WHERE job_id = ? AND kind = ?`, jobID, wire.UsageKindWorker).Scan(&n, &in, &out, &cost)
	if err != nil || n == 0 {
		return u, false, err
	}
	return wire.Usage{InputTokens: int(in.Int64), OutputTokens: int(out.Int64), CostUSD: cost.Float64}, true, nil
}

func (s *Store) refreshUsageRollup(ctx context.Context, jobID string) error {
	u, ok, err := s.jobRollup(ctx, jobID)
	if err != nil || !ok {
		return err
	}
	b, err := json.Marshal(u)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE jobs SET usage = ? WHERE id = ?`, string(b), jobID)
	return err
}

// UsageEvents returns the job's recorded events (oldest first); a test and
// diagnostics seam.
func (s *Store) UsageEvents(ctx context.Context, jobID string) ([]wire.UsageEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, job_id, task_id, foreman_id, model, tier, input, output, cache_create,
			cache_read, cost_usd, session_id, limit_raw FROM usage_events WHERE job_id = ? ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wire.UsageEvent
	for rows.Next() {
		var e wire.UsageEvent
		if err := rows.Scan(&e.Kind, &e.JobID, &e.TaskID, &e.ForemanID, &e.Model, &e.Tier, &e.Input,
			&e.Output, &e.CacheCreate, &e.CacheRead, &e.CostUSD, &e.SessionID, &e.Limit); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UsageSums is the factory's own spend over a time window.
type UsageSums struct {
	Tokens  int64   `json:"tokens"` // input + output + cache_create (cache reads excluded)
	CostUSD float64 `json:"cost_usd"`
}

// UsageSince sums usage_events with ts >= since (RFC3339 UTC text sorts
// lexically, matching how ts is written by s.now()).
func (s *Store) UsageSince(ctx context.Context, since time.Time) (UsageSums, error) {
	var tok sql.NullInt64
	var cost sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(input + output + cache_create), SUM(cost_usd) FROM usage_events WHERE ts >= ?`,
		since.UTC().Format(time.RFC3339)).Scan(&tok, &cost)
	return UsageSums{Tokens: tok.Int64, CostUSD: cost.Float64}, err
}
