// Package throttle is the token-throttle dispatch gate (slice S3): it turns
// the live subscription usage state from internal/usagewin into a decision
// per unit of NEW work - admit now, or hold. It never changes a job's
// model or tier, never kills running work, and fails OPEN (GREEN) when the
// usage reading is stale or missing.
//
// Operator rules (2026-11-16, override the plan):
//   - P0 (task priority 0) always passes, in every state.
//   - Operator chat wakes always pass, in every state.
//   - GREEN: no change. YELLOW: >= 60s between starts of non-P0 jobs.
//     ORANGE: hold non-P0 jobs with job priority >= 5, and cap concurrent
//     non-P0 worker jobs at 2. RED: hold every non-P0 job.
package throttle

import (
	"fmt"
	"time"

	"github.com/pricec/factory/internal/usagewin"
)

// Rule constants.
const (
	// YellowSpacing is the minimum gap between starts of non-P0 jobs in YELLOW.
	YellowSpacing = 60 * time.Second
	// OrangeHoldPriority: in ORANGE non-P0 jobs with priority >= this are held.
	OrangeHoldPriority = 5
	// OrangeMaxActive caps concurrently active (dispatched/running) non-P0
	// worker jobs in ORANGE.
	OrangeMaxActive = 2
)

// Kind is the unit of work being gated.
type Kind string

const (
	// KindJob is a new worker job (create_job, POST /v1/jobs, schedules,
	// or a held job up for release).
	KindJob Kind = "job"
	// KindChatWake is a planner wake caused by an operator chat message.
	// It always passes; S3 does not gate any wake (S4 wraps the
	// WakeLimiter), the kind exists so the rule table states it.
	KindChatWake Kind = "chat_wake"
)

// Request describes one unit of new work.
type Request struct {
	Kind Kind
	// P0 is true when the work belongs to a task with priority 0.
	P0 bool
	// JobPriority is the job's priority (1 most urgent .. 9).
	JobPriority int
}

// Load is the factory's current non-P0 load, supplied by the caller.
type Load struct {
	// ActiveNonP0 counts dispatched/running non-P0 worker jobs.
	ActiveNonP0 int
	// SinceLastStart is the time since the last admitted non-P0 job start;
	// HasLastStart is false when none happened since boot.
	SinceLastStart time.Duration
	HasLastStart   bool
	// QueuedAhead counts held non-P0 jobs that release before this one
	// (priority <= this job's, older). Zero for releaser decisions, which
	// walk the queue in order.
	QueuedAhead int
}

// Rule decides one request under level (the gate's effective state).
// UNKNOWN is treated as GREEN (fail open). It returns hold and a short
// why (empty when admitted).
func Rule(level usagewin.State, req Request, load Load) (hold bool, why string) {
	if req.Kind == KindChatWake || req.P0 {
		return false, ""
	}
	switch level {
	case usagewin.Red:
		return true, "RED holds all non-P0 work"
	case usagewin.Orange:
		if req.JobPriority >= OrangeHoldPriority {
			return true, fmt.Sprintf("ORANGE holds job priority >= %d (job has %d)", OrangeHoldPriority, req.JobPriority)
		}
		if load.ActiveNonP0 >= OrangeMaxActive {
			return true, fmt.Sprintf("ORANGE caps active non-P0 jobs at %d (%d active)", OrangeMaxActive, load.ActiveNonP0)
		}
	case usagewin.Yellow:
		if load.HasLastStart && load.SinceLastStart < YellowSpacing {
			return true, fmt.Sprintf("YELLOW spaces non-P0 starts %s apart (last %s ago)",
				YellowSpacing, load.SinceLastStart.Truncate(time.Second))
		}
	default: // GREEN, UNKNOWN: no change
		return false, ""
	}
	if load.QueuedAhead > 0 {
		return true, fmt.Sprintf("queued behind %d held job(s)", load.QueuedAhead)
	}
	return false, ""
}
