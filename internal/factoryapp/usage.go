package factoryapp

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/usagewin"
	"github.com/pricec/factory/internal/wire"
)

// UsageConfig configures the usage-window tracker (S2, observe-only).
type UsageConfig struct {
	// Poll enables the live endpoint poller in Run (off in tests).
	Poll bool
	// BaseInterval / FastInterval override the 120s GREEN / 60s YELLOW+ poll.
	BaseInterval, FastInterval time.Duration
	// F is the factory's share of the budget (<=0 -> 1.0).
	F float64
	// Cap5h / Cap7d are optional token caps enabling the own-accounting
	// fallback; zero leaves the state UNKNOWN when no live reading exists.
	Cap5h, Cap7d int64
	// CredsPath overrides ~/.claude/.credentials.json.
	CredsPath string
	// Client overrides the HTTP client (tests).
	Client interface {
		Do(*http.Request) (*http.Response, error)
	}
}

// LoadUsageConfig reads env: FACTORY_USAGE_POLL (default on; "off"/"0"
// disables), FACTORY_USAGE_POLL_S, FACTORY_USAGE_F, FACTORY_CLAUDE_5H_TOKENS,
// FACTORY_CLAUDE_WEEK_TOKENS.
func LoadUsageConfig() UsageConfig {
	c := UsageConfig{Poll: true}
	if v := os.Getenv("FACTORY_USAGE_POLL"); v == "off" || v == "0" {
		c.Poll = false
	}
	if n, err := strconv.Atoi(os.Getenv("FACTORY_USAGE_POLL_S")); err == nil && n > 0 {
		c.BaseInterval = time.Duration(n) * time.Second
		c.FastInterval = c.BaseInterval / 2
	}
	if f, err := strconv.ParseFloat(os.Getenv("FACTORY_USAGE_F"), 64); err == nil && f > 0 && f <= 1 {
		c.F = f
	}
	c.Cap5h, _ = strconv.ParseInt(os.Getenv("FACTORY_CLAUDE_5H_TOKENS"), 10, 64)
	c.Cap7d, _ = strconv.ParseInt(os.Getenv("FACTORY_CLAUDE_WEEK_TOKENS"), 10, 64)
	return c
}

func (a *App) initUsage(cfg Config) {
	u := cfg.Usage
	a.usage = usagewin.NewTracker(usagewin.Config{
		F: u.F, Cap5h: u.Cap5h, Cap7d: u.Cap7d, Now: cfg.Now,
		Own: func(ctx context.Context, now time.Time) (int64, int64, error) {
			s5, err := a.jobs.UsageSince(ctx, now.Add(-usagewin.FiveHour))
			if err != nil {
				return 0, 0, err
			}
			s7, err := a.jobs.UsageSince(ctx, now.Add(-usagewin.SevenDay))
			return s5.Tokens, s7.Tokens, err
		},
	})
	if u.Poll {
		a.usagePoll = &usagewin.Poller{
			Tracker: a.usage, Client: u.Client, CredsPath: u.CredsPath,
			Base: u.BaseInterval, Fast: u.FastInterval, Now: cfg.Now,
		}
	}
}

// recordUsage appends the event and feeds any rate_limit_info to the state
// cache as a stream reading.
func (a *App) recordUsage(ctx context.Context, ev wire.UsageEvent) error {
	// A hard rejection pauses dispatch (S5). Trip BEFORE the reading is
	// ingested so the pause alert is the one alert for the event.
	if rej, ok := usagewin.DetectRejection(ev.RateLimit, ev.Limit, a.clock()); ok && a.usage != nil {
		a.onRejection(ctx, rej, ev.JobID)
	}
	if ev.RateLimit != "" && a.usage != nil {
		a.usage.IngestRateLimit(ev.RateLimit)
	}
	return a.jobs.RecordUsage(ctx, ev)
}

type usageResponse struct {
	usagewin.Snapshot
	Spend map[string]jobs.UsageSums `json:"factory_spend"`
	// Gate is the S3 dispatch gate: mode, effective level, held count.
	Gate *gateView `json:"gate,omitempty"`
}

// handleUsage serves GET /v1/usage (unauthenticated UI-surface class, like
// /v1/spend): per-window utilization, resets_at, pace, state, source, age,
// plus the factory's own spend (5h, 7d, today UTC) from usage_events.
func (a *App) handleUsage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	if a.jobs.Now != nil {
		now = a.jobs.Now()
	}
	now = now.UTC()
	resp := usageResponse{Snapshot: a.usage.Snapshot(r.Context()), Spend: map[string]jobs.UsageSums{}}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for name, since := range map[string]time.Time{
		"5h": now.Add(-usagewin.FiveHour), "7d": now.Add(-usagewin.SevenDay), "today": today,
	} {
		sums, err := a.jobs.UsageSince(r.Context(), since)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		resp.Spend[name] = sums
	}
	if a.gate != nil {
		gv, err := a.gateStatus(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		resp.Gate = &gv
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUsageEvents serves GET /v1/usage/events?day=&kind=&limit= (the
// UI-surface class, like /v1/usage): the per-run usage_events log, newest
// first. day is a UTC YYYY-MM-DD (default today, "all" for every day);
// kind is worker|planner|brain or empty; limit defaults to 200, max 500.
func (a *App) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	day := q.Get("day")
	switch day {
	case "":
		now := time.Now()
		if a.jobs.Now != nil {
			now = a.jobs.Now()
		}
		day = now.UTC().Format("2006-01-02")
	case "all":
		day = ""
	default:
		if _, err := time.Parse("2006-01-02", day); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "day must be YYYY-MM-DD or all")
			return
		}
	}
	kind := q.Get("kind")
	switch kind {
	case "", wire.UsageKindWorker, wire.UsageKindPlanner, wire.UsageKindBrain:
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "kind must be worker, planner or brain")
		return
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		limit = n
	}
	evs, err := a.jobs.ListUsageEvents(r.Context(), day, kind, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, evs)
}
