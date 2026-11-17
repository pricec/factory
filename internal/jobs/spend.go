package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// spendDayLayout is the YYYY-MM-DD day format for spend queries.
const spendDayLayout = "2006-01-02"

// SpendTotals is a day's spend summary (GET /v1/spend, spec §6.9): the sum
// of recorded usage.cost_usd and the number of jobs created that day.
// The session-ledger fields (2026-11-16 amendment) are additive: the
// per-role aggregates from the sessions ledger (spec §6.5 reporting)
// plus its own total, which counts coordinator sessions (planner,
// foreman brain) that no JobResult ever carries - total_usd (jobs)
// and SessionTotalUSD (ledger) measure overlapping but different
// sources and are reported side by side, never merged.
type SpendTotals struct {
	Total float64 `json:"total_usd"`
	Jobs  int     `json:"jobs"`
	// SessionTotalUSD is the day's ledger sum across all roles.
	SessionTotalUSD float64 `json:"session_total_usd"`
	// Roles is one aggregate per pinned role, zero rows included.
	Roles []RoleTotals `json:"roles"`
}

// DailySpend sums usage.cost_usd over jobs created on day (YYYY-MM-DD,
// spec §6.5: daily spend = sum of usage.cost_usd over jobs created today).
// The usage JSON is parsed in Go - SQLite's JSON1 support is not
// guaranteed. Jobs with no recorded usage count toward Jobs and contribute
// zero. day must be a validated YYYY-MM-DD string: the HTTP layer
// validates external input, internal callers pass store-derived days.
func (s *Store) DailySpend(ctx context.Context, day string) (SpendTotals, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT usage FROM jobs WHERE created_at LIKE ?`, day+"%")
	if err != nil {
		return SpendTotals{}, err
	}
	defer rows.Close()
	var totals SpendTotals
	for rows.Next() {
		var usage sql.NullString
		if err := rows.Scan(&usage); err != nil {
			return totals, err
		}
		totals.Jobs++
		if !usage.Valid || usage.String == "" || usage.String == "null" {
			continue
		}
		var u wire.Usage
		if err := unmarshalJSON([]byte(usage.String), &u); err != nil {
			return totals, fmt.Errorf("jobs: usage on day %s: %w", day, err)
		}
		totals.Total += u.CostUSD
	}
	return totals, rows.Err()
}

// MarkOverBudget sets the over_budget flag on a job (spec §6.5 proxy 4:
// usage from report_result is recorded on the job; cost above
// max_cost_usd flags it). Guarded: the job must exist; its state is not
// changed.
func (s *Store) MarkOverBudget(ctx context.Context, jobID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET over_budget = 1, updated_at = ? WHERE id = ?`,
		s.now(), jobID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("jobs: mark over_budget: no such job %s", jobID)
	}
	return nil
}

// SpendHandler serves GET /v1/spend?day= (spec §6.9): the day's spend
// totals. An empty day defaults to the store clock's today; a malformed
// day is a 400. The cap and the breaker live at create_job, not here.
func (s *Store) SpendHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/spend", s.handleSpend)
	return mux
}

func (s *Store) handleSpend(w http.ResponseWriter, r *http.Request) {
	day := r.URL.Query().Get("day")
	if day == "" {
		day = s.Now().UTC().Format(spendDayLayout)
	} else if _, err := time.Parse(spendDayLayout, day); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "day must be YYYY-MM-DD", false)
		return
	}
	totals, err := s.DailySpend(r.Context(), day)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	// Session-ledger aggregates (additive, 2026-11-16 amendment): a
	// ledger read error degrades to zero rows, never fails the endpoint.
	totals.Roles, err = s.SessionTotals(r.Context(), day)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	for _, r := range totals.Roles {
		totals.SessionTotalUSD += r.TotalUSD
	}
	writeJSON(w, http.StatusOK, totals)
}

// CapError is the daily spend breaker refusal (spec §6.5, §10); the MCP
// surface maps Code() to the wire error code.
type CapError struct {
	Day   string
	Spend float64
	Cap   float64
}

func (e *CapError) Error() string {
	return fmt.Sprintf("daily spend $%.2f exceeds cap $%.2f", e.Spend, e.Cap)
}

// Code is the pinned wire error code (spec §10).
func (e *CapError) Code() string { return "spend_cap_exceeded" }

// RefuseIfSpendCap is the create_job breaker check (spec §6.5): it sums
// today's spend and refuses when it is above capUSD, unless override
// (FACTORY_SPEND_OVERRIDE=1). Exactly at the cap is allowed. The refusal
// is a *CapError.
func (s *Store) RefuseIfSpendCap(ctx context.Context, capUSD float64, override bool) error {
	if override {
		return nil
	}
	day := s.Now().UTC().Format(spendDayLayout)
	totals, err := s.DailySpend(ctx, day)
	if err != nil {
		return err
	}
	if totals.Total > capUSD {
		return &CapError{Day: day, Spend: totals.Total, Cap: capUSD}
	}
	return nil
}
