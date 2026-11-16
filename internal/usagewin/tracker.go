package usagewin

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"time"
)

// Source names where a reading came from.
type Source string

const (
	SourceEndpoint Source = "endpoint" // GET /api/oauth/usage
	SourceStream   Source = "stream"   // rate_limit_event from a claude run
	SourceOwn      Source = "own"      // usage_events sums vs configured caps
)

// StaleAfter: a reading older than this is stale (not used for state).
const StaleAfter = 10 * time.Minute

// Window identifies a usage window.
type Window string

const (
	Win5h Window = "five_hour"
	Win7d Window = "seven_day"
)

func (w Window) dur() time.Duration {
	if w == Win5h {
		return FiveHour
	}
	return SevenDay
}

// Reading is one observation of a window.
type Reading struct {
	Utilization float64 // percent 0-100
	ResetsAt    time.Time
	Status      string // allowed | allowed_warning | rejected | ""
	ObservedAt  time.Time
}

// OwnSums gives the factory's own token sums for the trailing 5h and 7d.
type OwnSums func(ctx context.Context, now time.Time) (tok5h, tok7d int64, err error)

// Config tunes the tracker.
type Config struct {
	// F is the share of the budget the factory may use (default 1.0).
	F float64
	// Own, Cap5h, Cap7d enable the own-accounting fallback (caps in tokens;
	// a window with cap <= 0 has no fallback).
	Own          OwnSums
	Cap5h, Cap7d int64
	Now          func() time.Time
}

// Tracker is the state cache. Safe for concurrent use.
type Tracker struct {
	cfg Config
	mu  sync.Mutex
	r   map[Window]map[Source]Reading
}

// NewTracker builds a Tracker (F<=0 -> 1.0, Now nil -> time.Now).
func NewTracker(cfg Config) *Tracker {
	if cfg.F <= 0 {
		cfg.F = 1.0
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Tracker{cfg: cfg, r: map[Window]map[Source]Reading{}}
}

// Set stores a reading (last write wins per window+source).
func (t *Tracker) Set(w Window, src Source, rd Reading) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.r[w] == nil {
		t.r[w] = map[Source]Reading{}
	}
	t.r[w][src] = rd
}

// rateLimitInfo is rate_limit_event.rate_limit_info (fractions 0-1, epoch s).
type rateLimitInfo struct {
	Status         string `json:"status"`
	RateLimitType  string `json:"rateLimitType"`
	UnifiedWindows map[string]struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *float64 `json:"resetsAt"`
		Status      string   `json:"status"`
	} `json:"unifiedWindows"`
}

// IngestRateLimit feeds a raw rate_limit_info JSON (from a stream-json run)
// as a stream reading. Malformed input and windows that already ended are
// ignored. It reports whether any window was recorded.
func (t *Tracker) IngestRateLimit(raw string) bool {
	var info rateLimitInfo
	if raw == "" || json.Unmarshal([]byte(raw), &info) != nil {
		return false
	}
	now := t.cfg.Now()
	got := false
	for _, w := range []Window{Win5h, Win7d} {
		e, ok := info.UnifiedWindows[string(w)]
		if !ok || e.Utilization == nil || e.ResetsAt == nil {
			continue
		}
		reset := time.Unix(int64(*e.ResetsAt), 0).UTC()
		if !reset.After(now) {
			continue
		}
		status := e.Status
		if info.Status == "rejected" && info.RateLimitType == string(w) {
			status = "rejected"
		}
		t.Set(w, SourceStream, Reading{Utilization: math.Round(*e.Utilization*10000) / 100, ResetsAt: reset, Status: status, ObservedAt: now})
		got = true
	}
	return got
}

// WindowView is one window of GET /v1/usage.
type WindowView struct {
	Utilization *float64   `json:"utilization_pct"`
	ResetsAt    *time.Time `json:"resets_at"`
	Pace        *float64   `json:"pace"`
	State       State      `json:"state"`
	Source      Source     `json:"source"`
	AgeS        *int64     `json:"age_s"`
	Stale       bool       `json:"stale"`
	Status      string     `json:"status,omitempty"`
}

// Snapshot is the tracker's current view.
type Snapshot struct {
	State   State                 `json:"state"`
	F       float64               `json:"f"`
	Windows map[Window]WindowView `json:"windows"`
}

// Snapshot evaluates both windows: the newest fresh live reading (endpoint
// or stream); else own accounting when caps are set; else the last reading
// shown stale with state UNKNOWN.
func (t *Tracker) Snapshot(ctx context.Context) Snapshot {
	now := t.cfg.Now()
	t.mu.Lock()
	rs := map[Window]map[Source]Reading{}
	for w, m := range t.r {
		rs[w] = map[Source]Reading{}
		for s, r := range m {
			rs[w][s] = r
		}
	}
	t.mu.Unlock()

	var own5, own7 int64
	var ownErr error
	if t.cfg.Own != nil && (t.cfg.Cap5h > 0 || t.cfg.Cap7d > 0) {
		own5, own7, ownErr = t.cfg.Own(ctx, now)
	}
	snap := Snapshot{State: Unknown, F: t.cfg.F, Windows: map[Window]WindowView{}}
	for _, w := range []Window{Win5h, Win7d} {
		v := t.view(w, rs[w], now, own5, own7, ownErr)
		snap.Windows[w] = v
		snap.State = Worse(snap.State, v.State)
	}
	return snap
}

func (t *Tracker) view(w Window, m map[Source]Reading, now time.Time, own5, own7 int64, ownErr error) WindowView {
	var best *Reading
	var bestSrc Source
	var newest *Reading // newest of any age
	var newestSrc Source
	for _, src := range []Source{SourceEndpoint, SourceStream} { // endpoint wins ties
		r, ok := m[src]
		if !ok {
			continue
		}
		if newest == nil || r.ObservedAt.After(newest.ObservedAt) {
			newest, newestSrc = &r, src
		}
		if now.Sub(r.ObservedAt) <= StaleAfter && (best == nil || r.ObservedAt.After(best.ObservedAt)) {
			best, bestSrc = &r, src
		}
	}
	if best != nil {
		return t.fromReading(w, *best, bestSrc, now, false, true)
	}
	limit := t.cfg.Cap5h
	used := own5
	if w == Win7d {
		limit, used = t.cfg.Cap7d, own7
	}
	if limit > 0 && t.cfg.Own != nil && ownErr == nil {
		rd := Reading{Utilization: float64(used) / float64(limit) * 100, ObservedAt: now}
		// Own sums are rolling, with no window anchor of their own; reuse
		// the last known reset only for display, never for pace.
		if newest != nil {
			rd.ResetsAt = rollForward(newest.ResetsAt, w.dur(), now)
		}
		return t.fromReading(w, rd, SourceOwn, now, false, false)
	}
	if newest != nil {
		return t.fromReading(w, *newest, newestSrc, now, true, false)
	}
	return WindowView{State: Unknown, Stale: true}
}

// rollForward advances a past reset time by whole windows to the next one.
func rollForward(reset time.Time, d time.Duration, now time.Time) time.Time {
	for !reset.After(now) {
		reset = reset.Add(d)
	}
	return reset
}

func (t *Tracker) fromReading(w Window, r Reading, src Source, now time.Time, stale, usePace bool) WindowView {
	util := r.Utilization
	age := int64(now.Sub(r.ObservedAt) / time.Second)
	v := WindowView{Utilization: &util, Source: src, AgeS: &age, Stale: stale, Status: r.Status}
	if !r.ResetsAt.IsZero() {
		ra := r.ResetsAt
		v.ResetsAt = &ra
	}
	var pace float64
	hasPace := false
	if usePace {
		pace, hasPace = Pace(util, r.ResetsAt, now, w.dur(), t.cfg.F)
		if hasPace {
			v.Pace = &pace
		}
	}
	if stale {
		v.State = Unknown
	} else {
		v.State = Classify(util, pace, hasPace, r.Status)
	}
	return v
}
