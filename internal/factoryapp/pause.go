// Token throttle S5 (automatic pause on a hard usage-limit rejection): any
// claude run's rejection (worker job, planner wake, foreman brain - all
// reach recordUsage) trips the gate into PAUSED; the pause persists, a
// loop re-probes the usage endpoint at reset time + jitter and resumes
// (or extends on a backoff while the endpoint still reports exhausted);
// the rejected job is requeued held without burning a retry; non-chat
// planner wakes wait for the resume.
package factoryapp

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"time"

	"github.com/pricec/factory/internal/usagewin"
	"github.com/pricec/factory/internal/wire"
)

const (
	// pauseSettingKey persists the pause (tasks settings table).
	pauseSettingKey = "throttle_pause"
	// pauseTick is the resume loop's cadence.
	pauseTick = 10 * time.Second
	// pauseFallback is the last-resort pause when no reset time is known.
	pauseFallback = 15 * time.Minute
	// pauseJitterMax is the random slack added to a reset time.
	pauseJitterMax = 30 * time.Second
	// exhaustedPct: utilization at or above this counts as exhausted.
	exhaustedPct = 99.5
	// wakePollEvery is how often a deferred wake re-checks the pause.
	wakePollEvery = 5 * time.Second
	// alertPriorityPause pages like RED; resume is P2 (24h).
	alertPriorityPause  = 1
	alertPriorityResume = 2
)

var pauseBackoff = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}

func (o settingsOverrides) LoadPause(ctx context.Context) (string, bool, error) {
	return o.s.GetSetting(ctx, pauseSettingKey)
}

func (o settingsOverrides) SavePause(ctx context.Context, raw string) error {
	return o.s.SetSetting(ctx, pauseSettingKey, raw)
}

func (a *App) clock() time.Time {
	if a.jobs != nil && a.jobs.Now != nil {
		return a.jobs.Now()
	}
	return time.Now()
}

func (a *App) jitter() time.Duration {
	if a.pauseJitter != nil {
		return a.pauseJitter()
	}
	return time.Duration(rand.Int63n(int64(pauseJitterMax)))
}

// probeUsage forces a fresh read of GET /api/oauth/usage (the re-probe).
func (a *App) probeUsage(ctx context.Context) error {
	if a.probe != nil {
		return a.probe(ctx)
	}
	if a.usagePoll == nil {
		return errors.New("usage poller disabled")
	}
	return a.usagePoll.PollOnce(ctx)
}

// onRejection handles one detected hard rejection: pause (or extend), and
// remember the job so its result requeues instead of failing.
func (a *App) onRejection(ctx context.Context, rej usagewin.Rejection, jobID string) {
	if jobID != "" {
		a.limitHit.Store(jobID, true)
	}
	if a.gate == nil {
		return
	}
	now := a.clock()
	until, estimated := rej.ResetsAt, false
	if until.IsZero() {
		until, estimated = a.fallbackReset(ctx, rej.Window, now)
	}
	until = until.Add(a.jitter())
	a.gate.Trip(ctx, usagewin.WindowLabel(rej.Window), until, estimated)
}

// fallbackReset: the usage endpoint's resets_at for the exhausted window
// (or, when unknown, the most utilized window), else now+15m (estimated).
func (a *App) fallbackReset(ctx context.Context, win usagewin.Window, now time.Time) (time.Time, bool) {
	snap := a.usage.Snapshot(ctx)
	var best *usagewin.WindowView
	for _, w := range []usagewin.Window{usagewin.Win5h, usagewin.Win7d} {
		v := snap.Windows[w]
		if v.ResetsAt == nil || !v.ResetsAt.After(now) || v.Stale || v.Utilization == nil {
			continue
		}
		if win != "" && w != win {
			continue
		}
		if best == nil || *v.Utilization > *best.Utilization {
			vv := v
			best = &vv
		}
	}
	if best != nil {
		return *best.ResetsAt, false
	}
	return now.Add(pauseFallback), true
}

// requeueLimited is the job.result hook: a failed result caused by the
// usage limit parks the job held (no retry burned, no failed state).
// handled is true when the result was consumed.
func (a *App) requeueLimited(ctx context.Context, res wire.JobResult) (handled bool) {
	if res.Status != wire.StatusFailed || res.Error == nil || res.JobID == "" {
		return false
	}
	_, marked := a.limitHit.LoadAndDelete(res.JobID)
	if !marked {
		text := res.Error.Message
		if !usagewin.IsLimitText(text) {
			return false
		}
		// No usage event arrived: detect from the failure text itself.
		if rej, ok := usagewin.DetectRejection("", text, a.clock()); ok {
			a.onRejection(ctx, rej, "")
		}
	}
	reason := "usage limit reached"
	if st, ok := a.gate.Pause(ctx); ok {
		reason = st.Reason
	}
	applied, err := a.jobs.RequeueHeld(ctx, res.JobID, reason)
	if err != nil {
		log.Printf("factoryapp: requeue %s on usage limit: %v", res.JobID, err)
		return false
	}
	if !applied {
		return false
	}
	log.Printf("factoryapp: job %s hit the usage limit: requeued held (%s); no retry consumed", res.JobID, reason)
	if taskID, ok, err := a.tasks.TaskForJob(ctx, res.JobID); err == nil && ok {
		a.publishTaskChanged(ctx, taskID)
	}
	return true
}

// PausePass is one resume-loop step (exported for tests): when the pause's
// until has passed it re-probes the usage endpoint; still exhausted ->
// extend (to the reported reset + jitter, else a backoff step); cleared (or
// the probe itself failed after the known reset time) -> resume. Returns
// true when the pause ended.
func (a *App) PausePass(ctx context.Context) bool {
	st, ok := a.gate.Pause(ctx)
	if !ok {
		return false
	}
	now := a.clock()
	if now.Before(st.Until) {
		return false
	}
	if err := a.probeUsage(ctx); err != nil {
		log.Printf("factoryapp: pause re-probe failed (%v); resuming (a still-rejected run re-pauses)", err)
	} else if exhausted, resets := a.windowExhausted(ctx, st.Window, now); exhausted {
		next, estimated := resets.Add(a.jitter()), false
		if resets.IsZero() {
			step := st.Probes
			if step >= len(pauseBackoff) {
				step = len(pauseBackoff) - 1
			}
			next, estimated = now.Add(pauseBackoff[step]), true
		}
		a.gate.Extend(ctx, next, estimated)
		return false
	}
	if a.gate.Resume(ctx) {
		// Let the releaser act at once rather than waiting for its tick.
		if n, err := a.ReleasePass(ctx); err != nil {
			log.Printf("factoryapp: release after resume: %v", err)
		} else if n > 0 {
			log.Printf("factoryapp: resume released %d held job(s)", n)
		}
		return true
	}
	return false
}

// windowExhausted reports whether the pause window (either window when
// unknown) is still exhausted per the freshest reading, and its reset.
func (a *App) windowExhausted(ctx context.Context, label string, now time.Time) (bool, time.Time) {
	snap := a.usage.Snapshot(ctx)
	var resets time.Time
	exhausted := false
	for _, w := range []usagewin.Window{usagewin.Win5h, usagewin.Win7d} {
		if label != "usage" && usagewin.WindowLabel(w) != label {
			continue
		}
		v := snap.Windows[w]
		if v.Stale || v.Utilization == nil || v.ResetsAt == nil || !v.ResetsAt.After(now) {
			continue
		}
		if *v.Utilization >= exhaustedPct || v.Status == "rejected" {
			exhausted = true
			if resets.IsZero() || v.ResetsAt.After(resets) {
				resets = *v.ResetsAt
			}
		}
	}
	return exhausted, resets
}

// pauseLoop runs PausePass every pauseTick until ctx is done.
func (a *App) pauseLoop(ctx context.Context) {
	t := time.NewTicker(pauseTick)
	defer t.Stop()
	for {
		a.PausePass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- planner wakes ----

type wakeKey struct{}

// pausedWakes wraps the WakeLimiter: while paused, non-chat wakes wait
// (polling in memory, no claude run) until the pause ends; chat wakes go
// straight through. The inner limiter keeps its own semantics.
type pausedWakes struct {
	a     *App
	inner WakeLimiter
}

func (p pausedWakes) Acquire(ctx context.Context) (func(), error) {
	if chat, _ := ctx.Value(wakeKey{}).(bool); !chat {
		if err := p.a.waitResume(ctx); err != nil {
			return nil, err
		}
	}
	return p.inner.Acquire(ctx)
}

// waitResume blocks while the gate is paused (and not overridden).
func (a *App) waitResume(ctx context.Context) error {
	every := a.wakePoll
	if every <= 0 {
		every = wakePollEvery
	}
	for a.gate != nil && !a.gate.Dispatching(ctx) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
	return nil
}

// wakeIsChat reports whether the wake carries operator chat.
func wakeIsChat(envs []wire.MessageEnvelope) bool {
	for _, e := range envs {
		if e.Type == "chat.message" {
			return true
		}
	}
	return false
}

// deferWake is the wake dispatcher's limit hook: a non-chat session that
// failed because the limit paused the factory waits for the resume and
// reports true (run it again); anything else reports false.
func (a *App) deferWake(ctx context.Context, err error) bool {
	if a.gate == nil || err == nil || a.gate.Dispatching(ctx) {
		return false
	}
	log.Printf("factoryapp: planner wake deferred until the usage limit resumes: %v", err)
	return a.waitResume(ctx) == nil
}
