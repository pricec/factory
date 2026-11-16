package throttle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/pricec/factory/internal/usagewin"
)

// Mode is the gate's operating mode.
type Mode string

const (
	// ModeAuto gates on the live usage state.
	ModeAuto Mode = "auto"
	// ModeOff admits everything (kill switch / operator override).
	ModeOff Mode = "off"
)

// Defaults.
const (
	// DefaultHysteresis: the effective level drops only after the raw state
	// stayed below it this long.
	DefaultHysteresis = 10 * time.Minute
	// DefaultOverride is how long an `off` override lasts without `until`.
	DefaultOverride = time.Hour
	// MaxOverride bounds an override's lifetime: it always expires.
	MaxOverride = 7 * 24 * time.Hour
	// inflightTTL: an admitted job counts toward the ORANGE concurrency
	// cap from admission until it shows up as dispatched/running (or this
	// long passes - e.g. it pends for lack of a foreman).
	inflightTTL = 2 * time.Minute
	// failOpenLogEvery re-logs a continuing fail-open condition.
	failOpenLogEvery = 10 * time.Minute
)

// histParams tunes the projected-exhaust estimate per window: the trailing
// lookback for the burn rate, the minimum span of samples before a
// projection is trusted, and the sampling period. The 7d window uses a 24h
// lookback (the plan's weekly smoothing) so one burst cannot bump it.
var histParams = map[usagewin.Window]struct {
	lookback, minSpan, every time.Duration
}{
	usagewin.Win5h: {30 * time.Minute, 10 * time.Minute, 30 * time.Second},
	usagewin.Win7d: {24 * time.Hour, 2 * time.Hour, 5 * time.Minute},
}

// Override is a persisted operator override (POST /v1/throttle).
type Override struct {
	Mode  Mode      `json:"mode"`
	Until time.Time `json:"until"`
}

// OverrideStore persists the override across restarts (optional).
type OverrideStore interface {
	LoadOverride(ctx context.Context) (raw string, ok bool, err error)
	SaveOverride(ctx context.Context, raw string) error
}

// Config wires a Gate.
type Config struct {
	// Snapshot reads the live usage state (usagewin.Tracker.Snapshot).
	Snapshot func(ctx context.Context) usagewin.Snapshot
	// Now is the clock (nil -> time.Now).
	Now func() time.Time
	// EnvOff is the FACTORY_THROTTLE=off kill switch: always admit.
	EnvOff bool
	// Hysteresis overrides DefaultHysteresis (<= 0 -> default).
	Hysteresis time.Duration
	// Overrides persists the operator override (nil -> process-local).
	Overrides OverrideStore
	// OnAlert is called on entering ORANGE or RED, once per (level,
	// window, reset) - i.e. deduped per usage window.
	OnAlert func(ctx context.Context, level usagewin.State, title, body string)
	// Logf logs (nil -> log.Printf).
	Logf func(format string, args ...any)
}

// Eval is one evaluation of the gate.
type Eval struct {
	Mode          Mode       `json:"mode"`
	ModeSource    string     `json:"mode_source"` // default | env | override
	OverrideUntil *time.Time `json:"override_until,omitempty"`
	// Raw is the worst usable window state after the projected-exhaust
	// bump; UNKNOWN when failing open.
	Raw usagewin.State `json:"raw_state"`
	// Level is the effective state the rules use (hysteresis applied;
	// GREEN when failing open).
	Level    usagewin.State `json:"level"`
	FailOpen bool           `json:"fail_open"`
	// Summary names the driving window, e.g. "5h 88%, resets 23:30Z".
	Summary string `json:"summary,omitempty"`
	// Bumped lists windows whose state was raised by the projection.
	Bumped []usagewin.Window `json:"bumped,omitempty"`
}

type sample struct {
	t    time.Time
	util float64
}

type winHist struct {
	resetsAt time.Time
	s        []sample
}

// Gate is the dispatch gate. Safe for concurrent use; Admit calls are
// serialized so the YELLOW spacing and ORANGE cap hold under concurrency.
type Gate struct {
	cfg Config

	mu         sync.Mutex
	level      usagewin.State
	belowSince time.Time
	peakBelow  usagewin.State
	hist       map[usagewin.Window]*winHist
	lastStart  time.Time
	inflight   map[string]time.Time
	alerted    map[string]time.Time // dedupe key -> window reset (pruned after)
	override   *Override
	loadedOv   bool
	failOpen   bool
	failLogged time.Time
}

// New builds a Gate.
func New(cfg Config) *Gate {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Hysteresis <= 0 {
		cfg.Hysteresis = DefaultHysteresis
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &Gate{
		cfg:      cfg,
		hist:     map[usagewin.Window]*winHist{},
		inflight: map[string]time.Time{},
		alerted:  map[string]time.Time{},
	}
}

// Evaluate computes the current gate state (and fires ORANGE/RED alerts).
func (g *Gate) Evaluate(ctx context.Context) Eval {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.evaluateLocked(ctx)
}

// Admit decides whether one unit of new work may start now. jobID keys
// the in-flight accounting for the ORANGE cap. active returns the ids of
// dispatched/running NON-P0 jobs (called only when the cap matters; an
// error fails open). queuedAhead is the caller's count of held jobs that
// would release first (0 for the releaser). On hold, reason is the
// operator-facing held_reason.
func (g *Gate) Admit(ctx context.Context, jobID string, req Request, active func(ctx context.Context) ([]string, error), queuedAhead int) (hold bool, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ev := g.evaluateLocked(ctx)
	now := g.cfg.Now()
	level := ev.Level
	if ev.Mode == ModeOff {
		level = usagewin.Green
	}
	g.pruneInflight(now, nil) // TTL prune on every call: bounded in all states
	load := Load{QueuedAhead: queuedAhead}
	if !g.lastStart.IsZero() {
		load.HasLastStart, load.SinceLastStart = true, now.Sub(g.lastStart)
	}
	if level == usagewin.Orange && !req.P0 && req.Kind == KindJob && active != nil {
		ids, err := active(ctx)
		if err != nil {
			g.cfg.Logf("throttle: active-job count failed, failing open: %v", err)
		}
		g.pruneInflight(now, ids)
		set := map[string]bool{}
		for _, id := range ids {
			set[id] = true
		}
		for id := range g.inflight {
			set[id] = true
		}
		delete(set, jobID)
		load.ActiveNonP0 = len(set)
	}
	hold, why := Rule(level, req, load)
	if hold {
		return true, fmt.Sprintf("usage %s: %s; %s", level, ev.Summary, why)
	}
	if !req.P0 && req.Kind == KindJob {
		g.lastStart = now
		if jobID != "" {
			g.inflight[jobID] = now
		}
	}
	return false, ""
}

// pruneInflight drops in-flight entries that now show as active (counted
// from the DB) or that aged out.
func (g *Gate) pruneInflight(now time.Time, active []string) {
	for _, id := range active {
		delete(g.inflight, id)
	}
	for id, t := range g.inflight {
		if now.Sub(t) > inflightTTL {
			delete(g.inflight, id)
		}
	}
}

// SetOverride applies POST /v1/throttle: mode auto clears any override;
// mode off disables the gate until `until` (zero -> now+1h; capped at
// now+7d; must be in the future).
func (g *Gate) SetOverride(ctx context.Context, mode Mode, until time.Time) (Eval, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loadOverrideLocked(ctx)
	now := g.cfg.Now()
	switch mode {
	case ModeAuto:
		g.override = nil
	case ModeOff:
		if until.IsZero() {
			until = now.Add(DefaultOverride)
		}
		if !until.After(now) {
			return Eval{}, errors.New("until must be in the future")
		}
		if until.Sub(now) > MaxOverride {
			until = now.Add(MaxOverride)
		}
		g.override = &Override{Mode: ModeOff, Until: until.UTC()}
	default:
		return Eval{}, fmt.Errorf("mode must be auto or off, got %q", mode)
	}
	g.saveOverrideLocked(ctx)
	if g.override != nil {
		g.cfg.Logf("throttle: operator override off until %s", g.override.Until.Format(time.RFC3339))
	} else {
		g.cfg.Logf("throttle: operator override cleared (auto)")
	}
	return g.evaluateLocked(ctx), nil
}

func (g *Gate) loadOverrideLocked(ctx context.Context) {
	if g.loadedOv {
		return
	}
	g.loadedOv = true
	if g.cfg.Overrides == nil {
		return
	}
	raw, ok, err := g.cfg.Overrides.LoadOverride(ctx)
	if err != nil {
		g.cfg.Logf("throttle: load override: %v", err)
		return
	}
	if !ok || raw == "" {
		return
	}
	var ov Override
	if err := json.Unmarshal([]byte(raw), &ov); err != nil || ov.Mode != ModeOff {
		return
	}
	g.override = &ov
}

func (g *Gate) saveOverrideLocked(ctx context.Context) {
	if g.cfg.Overrides == nil {
		return
	}
	raw := ""
	if g.override != nil {
		b, _ := json.Marshal(g.override)
		raw = string(b)
	}
	if err := g.cfg.Overrides.SaveOverride(ctx, raw); err != nil {
		g.cfg.Logf("throttle: save override: %v", err)
	}
}

func (g *Gate) evaluateLocked(ctx context.Context) Eval {
	now := g.cfg.Now()
	g.loadOverrideLocked(ctx)
	ev := Eval{Mode: ModeAuto, ModeSource: "default"}
	if g.override != nil && !now.Before(g.override.Until) {
		g.cfg.Logf("throttle: operator override expired at %s; back to auto", g.override.Until.Format(time.RFC3339))
		g.override = nil
		g.saveOverrideLocked(ctx)
	}
	switch {
	case g.cfg.EnvOff:
		ev.Mode, ev.ModeSource = ModeOff, "env"
	case g.override != nil:
		u := g.override.Until
		ev.Mode, ev.ModeSource, ev.OverrideUntil = ModeOff, "override", &u
	}

	var snap usagewin.Snapshot
	if g.cfg.Snapshot != nil {
		snap = g.cfg.Snapshot(ctx)
	}
	raw := usagewin.Unknown
	var drive usagewin.Window
	var driveView usagewin.WindowView
	driveBumped := false
	usable := 0
	for _, w := range []usagewin.Window{usagewin.Win5h, usagewin.Win7d} {
		v := snap.Windows[w]
		if !usableView(v) {
			continue
		}
		usable++
		st := v.State
		bumped := g.observe(w, v, now)
		if bumped {
			st = bump(st)
			ev.Bumped = append(ev.Bumped, w)
		}
		if drive == "" || rank(st) > rank(raw) {
			raw, drive, driveView, driveBumped = usagewin.Worse(raw, st), w, v, bumped
		}
	}

	if usable == 0 {
		ev.FailOpen, ev.Raw, ev.Level = true, usagewin.Unknown, usagewin.Green
		ev.Summary = "usage reading stale or unavailable (failing open)"
		if !g.failOpen || now.Sub(g.failLogged) >= failOpenLogEvery {
			g.cfg.Logf("throttle: usage reading stale or unavailable; failing OPEN (treating as GREEN)")
			g.failLogged = now
		}
		g.failOpen = true
		g.level, g.belowSince, g.peakBelow = usagewin.Green, time.Time{}, ""
		return ev
	}
	if g.failOpen {
		g.cfg.Logf("throttle: usage reading available again (%s)", raw)
		g.failOpen = false
	}
	ev.Raw = raw
	ev.Summary = summarize(drive, driveView, driveBumped, now)

	prev := g.level
	g.applyHysteresis(raw, now)
	ev.Level = g.level
	if rank(g.level) > rank(raw) {
		ev.Summary += fmt.Sprintf("; holding %s until %s below", g.level, g.cfg.Hysteresis)
	}
	if rank(g.level) > rank(prev) && (g.level == usagewin.Orange || g.level == usagewin.Red) {
		g.alertLocked(ctx, g.level, drive, driveView, ev, now)
	}
	return ev
}

// applyHysteresis escalates at once and drops only after the raw state has
// stayed below the effective level for the hysteresis period; it then drops
// to the worst raw state seen during that period.
func (g *Gate) applyHysteresis(raw usagewin.State, now time.Time) {
	if rank(raw) >= rank(g.level) {
		g.level, g.belowSince, g.peakBelow = raw, time.Time{}, ""
		return
	}
	if g.belowSince.IsZero() {
		g.belowSince, g.peakBelow = now, raw
	} else {
		g.peakBelow = usagewin.Worse(g.peakBelow, raw)
	}
	if now.Sub(g.belowSince) < g.cfg.Hysteresis {
		return
	}
	g.level = g.peakBelow
	if rank(raw) < rank(g.level) {
		g.belowSince, g.peakBelow = now, raw
	} else {
		g.belowSince, g.peakBelow = time.Time{}, ""
	}
}

func (g *Gate) alertLocked(ctx context.Context, level usagewin.State, w usagewin.Window, v usagewin.WindowView, ev Eval, now time.Time) {
	for k, reset := range g.alerted {
		if now.After(reset) {
			delete(g.alerted, k)
		}
	}
	reset := now.Add(usagewin.FiveHour)
	if v.ResetsAt != nil {
		reset = *v.ResetsAt
	}
	key := fmt.Sprintf("%s|%s|%s", level, w, reset.UTC().Format(time.RFC3339))
	if _, done := g.alerted[key]; done {
		return
	}
	g.alerted[key] = reset
	action := "jobs from non-P0 tasks with job priority >= 5 are held; at most 2 non-P0 worker jobs run at once"
	if level == usagewin.Red {
		action = "every new non-P0 job is held"
	}
	title := fmt.Sprintf("Factory usage %s: %s", level, ev.Summary)
	body := fmt.Sprintf("Subscription usage entered %s (%s).\nThrottle: %s. Task-priority-0 work and operator chat always pass; running jobs are untouched; held jobs release automatically.\nMode: %s (%s). Override: POST /v1/throttle {\"mode\":\"off\",\"until\":\"<RFC3339>\"}.",
		level, ev.Summary, action, ev.Mode, ev.ModeSource)
	if g.cfg.OnAlert != nil {
		g.cfg.OnAlert(ctx, level, title, body)
	}
	g.cfg.Logf("throttle: entered %s: %s", level, ev.Summary)
}

// observe records a sample for w and reports whether the trailing burn
// rate projects exhaustion before the window resets.
func (g *Gate) observe(w usagewin.Window, v usagewin.WindowView, now time.Time) bool {
	p := histParams[w]
	if v.ResetsAt == nil || v.Utilization == nil {
		return false
	}
	h := g.hist[w]
	if h == nil || absDur(h.resetsAt.Sub(*v.ResetsAt)) > time.Minute {
		h = &winHist{resetsAt: *v.ResetsAt}
		g.hist[w] = h
	}
	obs := now
	if v.AgeS != nil {
		obs = now.Add(-time.Duration(*v.AgeS) * time.Second)
	}
	if n := len(h.s); n == 0 || obs.Sub(h.s[n-1].t) >= p.every {
		h.s = append(h.s, sample{t: obs, util: *v.Utilization})
	}
	cut := now.Add(-p.lookback)
	i := 0
	for i < len(h.s)-1 && h.s[i].t.Before(cut) {
		i++
	}
	h.s = h.s[i:]
	return projectsExhaust(h.s, h.resetsAt, p.minSpan)
}

// projectsExhaust: burn = (last-first)/span over the samples; true when
// span >= minSpan, burn > 0 and last + (100-util)/burn is before reset.
func projectsExhaust(s []sample, reset time.Time, minSpan time.Duration) bool {
	if len(s) < 2 {
		return false
	}
	first, last := s[0], s[len(s)-1]
	span := last.t.Sub(first.t)
	if span < minSpan {
		return false
	}
	burn := (last.util - first.util) / span.Seconds() // pct per second
	if burn <= 0 {
		return false
	}
	left := math.Max(0, 100-last.util)
	eta := last.t.Add(time.Duration(left / burn * float64(time.Second)))
	return eta.Before(reset)
}

func usableView(v usagewin.WindowView) bool {
	if v.Stale || v.State == usagewin.Unknown || v.State == "" || v.Utilization == nil {
		return false
	}
	// Live sources only (operator decision: no hand-configured caps).
	return v.Source == usagewin.SourceEndpoint || v.Source == usagewin.SourceStream
}

func summarize(w usagewin.Window, v usagewin.WindowView, bumped bool, now time.Time) string {
	name := "5h"
	if w == usagewin.Win7d {
		name = "7d"
	}
	parts := []string{fmt.Sprintf("%s %.0f%%", name, *v.Utilization)}
	if v.ResetsAt != nil {
		r := v.ResetsAt.UTC()
		layout := "15:04Z"
		if r.Sub(now) > 20*time.Hour {
			layout = "Jan 2 15:04Z"
		}
		parts = append(parts, "resets "+r.Format(layout))
	}
	s := strings.Join(parts, ", ")
	if bumped {
		s += " (projected to run out before reset)"
	}
	return s
}

func bump(s usagewin.State) usagewin.State {
	switch s {
	case usagewin.Green:
		return usagewin.Yellow
	case usagewin.Yellow:
		return usagewin.Orange
	case usagewin.Orange, usagewin.Red:
		return usagewin.Red
	}
	return s
}

func rank(s usagewin.State) int {
	switch s {
	case usagewin.Green:
		return 1
	case usagewin.Yellow:
		return 2
	case usagewin.Orange:
		return 3
	case usagewin.Red:
		return 4
	}
	return 0
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
