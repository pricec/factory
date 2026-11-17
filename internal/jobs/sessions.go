package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// The session-usage ledger (spec §6.5 reporting amendment, 2026-11-16):
// one row per completed LLM child - factory planner, foreman brain,
// worker - recorded from the opencode session store (post-run read,
// provider-reported cost + token breakdown). Foreman sessions arrive
// as `session.usage` envelopes (spec §3.2) posted to factory:main;
// planner sessions are recorded in-process by the factory. The ledger
// is additive to the jobs' own `usage` field: /v1/spend's per-role
// totals and GET /v1/sessions read it, and a worker session backfills
// its job's empty `usage` so post-hoc job accounting (spec §6.5 proxy
// 4) sees runs whose agent never self-reported.

// Session is one ledger row (the recording of one wire.SessionUsage).
type Session struct {
	ID               string  `json:"id"`
	Role             string  `json:"role"`
	ForemanID        string  `json:"foreman_id"`
	JobID            string  `json:"job_id"`
	Model            string  `json:"model"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	DurationMs       int64   `json:"duration_ms"`
	CreatedAt        string  `json:"created_at"`
}

// RoleTotals aggregates one role's sessions for a day (spec §6.5
// reporting amendment): the additive `roles` array of /v1/spend.
type RoleTotals struct {
	Role             string  `json:"role"`
	Sessions         int     `json:"sessions"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalUSD         float64 `json:"total_usd"`
}

// RecordSession upserts one ledger row. The opencode session id is the
// primary key, so duplicate envelope deliveries (inbox at-least-once)
// replace rather than double-count. The row is stamped with the store
// clock at record time.
func (s *Store) RecordSession(ctx context.Context, u wire.SessionUsage) error {
	if err := u.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, role, foreman_id, job_id, model,
			input_tokens, output_tokens, reasoning_tokens,
			cache_read_tokens, cache_write_tokens, cost_usd,
			duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			role = excluded.role,
			foreman_id = excluded.foreman_id,
			job_id = excluded.job_id,
			model = excluded.model,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			reasoning_tokens = excluded.reasoning_tokens,
			cache_read_tokens = excluded.cache_read_tokens,
			cache_write_tokens = excluded.cache_write_tokens,
			cost_usd = excluded.cost_usd,
			duration_ms = excluded.duration_ms`,
		u.SessionID, u.Role, u.ForemanID, u.JobID, u.Model,
		u.InputTokens, u.OutputTokens, u.ReasoningTokens,
		u.CacheReadTokens, u.CacheWriteTokens, u.CostUSD,
		u.DurationMs, s.now())
	return err
}

// sessionRoles is the pinned role order for totals and listings (spec
// §6.5): coordinator roles first, then the worker ladder.
var sessionRoles = []string{
	wire.SessionRolePlanner,
	wire.SessionRoleForemanBrain,
	wire.SessionRoleWorker,
}

// SessionTotals returns the day's (YYYY-MM-DD) per-role aggregates from
// the ledger, one row per pinned role (zero rows included), plus the
// session count.
func (s *Store) SessionTotals(ctx context.Context, day string) ([]RoleTotals, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, COUNT(*), SUM(input_tokens), SUM(output_tokens),
		       SUM(reasoning_tokens), SUM(cache_read_tokens),
		       SUM(cache_write_tokens), SUM(cost_usd)
		FROM sessions WHERE created_at LIKE ?
		GROUP BY role`, day+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byRole := make(map[string]*RoleTotals, len(sessionRoles))
	for rows.Next() {
		var t RoleTotals
		if err := rows.Scan(&t.Role, &t.Sessions, &t.InputTokens,
			&t.OutputTokens, &t.ReasoningTokens, &t.CacheReadTokens,
			&t.CacheWriteTokens, &t.TotalUSD); err != nil {
			return nil, err
		}
		byRole[t.Role] = &t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]RoleTotals, 0, len(sessionRoles))
	for _, role := range sessionRoles {
		if t, ok := byRole[role]; ok {
			out = append(out, *t)
			continue
		}
		out = append(out, RoleTotals{Role: role})
	}
	return out, nil
}

// ListSessions returns the day's (YYYY-MM-DD) ledger rows, newest
// first, optionally filtered by role; limit <= 0 means 200, capped at
// 500. day may be "" for all days (the sessions log view).
func (s *Store) ListSessions(ctx context.Context, day, role string, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	q := `
		SELECT id, role, foreman_id, job_id, model, input_tokens,
		       output_tokens, reasoning_tokens, cache_read_tokens,
		       cache_write_tokens, cost_usd, duration_ms, created_at
		FROM sessions`
	args := []any{}
	conds := []string{}
	if day != "" {
		conds = append(conds, "created_at LIKE ?")
		args = append(args, day+"%")
	}
	if role != "" {
		conds = append(conds, "role = ?")
		args = append(args, role)
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
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.Role, &s.ForemanID, &s.JobID,
			&s.Model, &s.InputTokens, &s.OutputTokens,
			&s.ReasoningTokens, &s.CacheReadTokens,
			&s.CacheWriteTokens, &s.CostUSD, &s.DurationMs,
			&s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// BackfillJobUsage fills a job's `usage` when it is still empty (spec
// §6.5 proxy 4): worker agents that never self-report leave the job's
// accounting at zero, so the session ledger's measured row backfills
// it. An existing usage (agent-reported or an earlier backfill) always
// wins; a missing job is ok=false, never an error.
func (s *Store) BackfillJobUsage(ctx context.Context, jobID string, u wire.Usage) error {
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE jobs SET usage = ?, updated_at = ?
		WHERE id = ? AND (usage IS NULL OR usage = '' OR usage = 'null')`,
		string(data), s.now(), jobID)
	return err
}

// SessionsHandler serves the session-ledger HTTP surface (spec §6.9):
// GET /v1/sessions?day=&role=&limit=. The day defaults to the store
// clock's today; a malformed day is a 400. Unauthenticated like the
// rest of the control plane.
func (s *Store) SessionsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	return mux
}

func (s *Store) handleListSessions(w http.ResponseWriter, r *http.Request) {
	day := r.URL.Query().Get("day")
	if day == "" {
		day = s.Now().UTC().Format(spendDayLayout)
	} else if _, err := time.Parse(spendDayLayout, day); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "day must be YYYY-MM-DD", false)
		return
	}
	sessions, err := s.ListSessions(r.Context(), day, r.URL.Query().Get("role"), 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}
