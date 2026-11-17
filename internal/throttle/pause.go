package throttle

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pricec/factory/internal/usagewin"
)

// Paused is the gate level above RED (token throttle S5): a hard usage-limit
// rejection was seen, so NO new work is dispatched - not even P0 - except
// operator chat wakes (which are attempted and fail fast if the account is
// still rejected). Running jobs are never touched; models/tiers never change.
const Paused usagewin.State = "PAUSED"

// PauseState is the persisted pause (survives a restart): a known
// rejection must not fail open just because the process restarted.
type PauseState struct {
	// Until is when the gate next re-probes (reset time + jitter, or a
	// backoff step while the usage endpoint still reports exhausted).
	Until time.Time `json:"until"`
	// Window is the exhausted window: "5h", "7d" or "usage" (unknown).
	Window string `json:"window"`
	// Reason is the operator-facing text, also the held_reason of held jobs,
	// e.g. "usage limit reached: 5h window, resumes 23:30Z".
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
	// Probes counts consecutive extensions (the re-probe backoff step).
	Probes int `json:"probes,omitempty"`
}

// PauseStore persists the pause state ("" = none).
type PauseStore interface {
	LoadPause(ctx context.Context) (raw string, ok bool, err error)
	SavePause(ctx context.Context, raw string) error
}

// PauseReason formats the held_reason / pause reason.
func PauseReason(window string, until time.Time, now time.Time, estimated bool) string {
	layout := "15:04Z"
	if until.Sub(now) > 20*time.Hour {
		layout = "Jan 2 15:04Z"
	}
	s := fmt.Sprintf("usage limit reached: %s window, resumes %s", window, until.UTC().Format(layout))
	if estimated {
		s += " (estimated; will re-probe)"
	}
	return s
}

func (g *Gate) loadPauseLocked(ctx context.Context) {
	if g.pauseLoaded {
		return
	}
	g.pauseLoaded = true
	if g.cfg.Pauses == nil {
		return
	}
	raw, ok, err := g.cfg.Pauses.LoadPause(ctx)
	if err != nil {
		g.cfg.Logf("throttle: load pause: %v", err)
		return
	}
	if !ok || raw == "" {
		return
	}
	var p PauseState
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Until.IsZero() {
		g.cfg.Logf("throttle: ignoring unreadable persisted pause %q", raw)
		return
	}
	g.pause = &p
	g.cfg.Logf("throttle: restored PAUSED state after restart: %s (until %s)", p.Reason, p.Until.Format(time.RFC3339))
}

func (g *Gate) savePauseLocked(ctx context.Context) {
	if g.cfg.Pauses == nil {
		return
	}
	raw := ""
	if g.pause != nil {
		b, _ := json.Marshal(g.pause)
		raw = string(b)
	}
	if err := g.cfg.Pauses.SavePause(ctx, raw); err != nil {
		g.cfg.Logf("throttle: save pause: %v", err)
	}
}

// Pause returns the current pause state (ok false when not paused). It
// reports the state even while an operator override disables the gate.
func (g *Gate) Pause(ctx context.Context) (PauseState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loadPauseLocked(ctx)
	if g.pause == nil {
		return PauseState{}, false
	}
	return *g.pause, true
}

// Dispatching reports whether the pickers may dispatch (pending / retry
// jobs): false only while paused and not overridden.
func (g *Gate) Dispatching(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loadPauseLocked(ctx)
	if g.pause == nil {
		return true
	}
	return g.modeLocked(ctx) == ModeOff
}

// Trip enters (or extends) the pause after a hard rejection. A later
// until extends; an earlier one never shortens an existing pause. It
// returns true when this call started the pause (one alert per pause).
func (g *Gate) Trip(ctx context.Context, window string, until time.Time, estimated bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loadPauseLocked(ctx)
	now := g.cfg.Now()
	reason := PauseReason(window, until, now, estimated)
	if g.pause != nil {
		if until.After(g.pause.Until) {
			g.pause.Until, g.pause.Reason = until.UTC(), reason
			if window != "usage" {
				g.pause.Window = window
			}
			g.savePauseLocked(ctx)
		}
		return false
	}
	g.pause = &PauseState{Until: until.UTC(), Window: window, Reason: reason, Since: now.UTC()}
	g.savePauseLocked(ctx)
	g.cfg.Logf("throttle: PAUSED: %s", reason)
	if g.cfg.OnPause != nil {
		title := "Factory paused: " + reason
		body := fmt.Sprintf("A claude run was rejected by the Anthropic usage limit (%s window).\nNo new jobs are dispatched until the limit resets (about %s); held jobs release automatically, most urgent first. Operator chat replies are still attempted; running jobs are not killed; models and tiers are unchanged.\nOverride: POST /v1/throttle {\"mode\":\"off\",\"until\":\"<RFC3339>\"}.",
			window, until.UTC().Format(time.RFC3339))
		g.cfg.OnPause(ctx, title, body)
	}
	return true
}

// Extend moves an existing pause's until (the endpoint still reports
// exhausted at the re-probe) and bumps the backoff counter.
func (g *Gate) Extend(ctx context.Context, until time.Time, estimated bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loadPauseLocked(ctx)
	if g.pause == nil {
		return
	}
	g.pause.Until = until.UTC()
	g.pause.Probes++
	g.pause.Reason = PauseReason(g.pause.Window, until, g.cfg.Now(), estimated)
	g.savePauseLocked(ctx)
	g.cfg.Logf("throttle: pause extended: %s", g.pause.Reason)
}

// Resume leaves the pause (the re-probe says the limit has cleared) and
// raises the one resume alert. The hysteresis dwell is reset: the window
// really reset, so the level follows the fresh reading at once (later
// drops use the normal hysteresis again).
func (g *Gate) Resume(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loadPauseLocked(ctx)
	if g.pause == nil {
		return false
	}
	p := *g.pause
	g.pause = nil
	g.savePauseLocked(ctx)
	g.level, g.belowSince, g.peakBelow = "", time.Time{}, ""
	g.cfg.Logf("throttle: RESUMED after %s pause (%s)", g.cfg.Now().Sub(p.Since).Truncate(time.Second), p.Reason)
	if g.cfg.OnResume != nil {
		g.cfg.OnResume(ctx,
			fmt.Sprintf("Factory resumed: %s window usage limit cleared", p.Window),
			fmt.Sprintf("The usage limit that paused dispatch at %s has cleared (re-probe OK). Held jobs are releasing, most urgent first, under the normal throttle rules.", p.Since.UTC().Format(time.RFC3339)))
	}
	return true
}
