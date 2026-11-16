// Package usagewin tracks the Claude subscription's 5h and 7d usage windows
// (token-throttle slice S2, observe-only): pure pace/state functions, a
// reading cache fed by the live oauth usage endpoint and by the CLI's
// rate_limit_event, and the poller. Nothing here throttles anything.
package usagewin

import "time"

// State is the throttle-relevant pressure level.
type State string

const (
	Unknown State = "UNKNOWN"
	Green   State = "GREEN"
	Yellow  State = "YELLOW"
	Orange  State = "ORANGE"
	Red     State = "RED"
)

func (s State) rank() int {
	switch s {
	case Green:
		return 1
	case Yellow:
		return 2
	case Orange:
		return 3
	case Red:
		return 4
	}
	return 0
}

// Worse returns the more severe of two states (UNKNOWN is the least severe,
// so a known state always wins over an unknown one).
func Worse(a, b State) State {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// Window durations.
const (
	FiveHour = 5 * time.Hour
	SevenDay = 7 * 24 * time.Hour
)

// Thresholds (plan section 3). Pace bands are upper-inclusive.
const (
	paceYellow = 1.10
	paceOrange = 1.30
	paceRed    = 1.60
	// utilOrange / utilRed are percent-of-window absolute triggers.
	utilOrange = 85.0
	utilRed    = 95.0
	// minElapsedFrac floors the elapsed fraction so the first minutes of a
	// window don't produce absurd paces; minPaceUtil ignores pace until the
	// window has a few percent used.
	minElapsedFrac = 0.05
	minPaceUtil    = 5.0
)

// WindowStart is resets_at minus the window length.
func WindowStart(resetsAt time.Time, window time.Duration) time.Time {
	return resetsAt.Add(-window)
}

// Pace is used / allowed, with allowed = f * elapsedFraction (f is the
// factory's share of the budget, 1.0 = all of it). utilPct is 0-100. The
// elapsed fraction is clamped to [minElapsedFrac, 1]. ok is false when f is
// not positive or resetsAt is zero.
func Pace(utilPct float64, resetsAt, now time.Time, window time.Duration, f float64) (pace float64, ok bool) {
	if f <= 0 || resetsAt.IsZero() {
		return 0, false
	}
	frac := float64(now.Sub(WindowStart(resetsAt, window))) / float64(window)
	frac = min(max(frac, minElapsedFrac), 1)
	return (utilPct / 100) / (f * frac), true
}

// Classify maps utilization (percent), pace (hasPace=false skips the pace
// bands) and the CLI status to a State: RED on status=rejected or
// utilization >= 95%; ORANGE at >85% or pace 1.30-1.60; YELLOW pace
// 1.10-1.30; RED pace > 1.60. Pace bands only apply from 5% utilization.
func Classify(utilPct, pace float64, hasPace bool, status string) State {
	if status == "rejected" || utilPct >= utilRed {
		return Red
	}
	st := Green
	if utilPct > utilOrange {
		st = Orange
	}
	if hasPace && utilPct >= minPaceUtil {
		switch {
		case pace > paceRed:
			st = Worse(st, Red)
		case pace > paceOrange:
			st = Worse(st, Orange)
		case pace >= paceYellow:
			st = Worse(st, Yellow)
		}
	}
	return st
}
