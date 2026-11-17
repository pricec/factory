package factoryapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"

	"github.com/pricec/factory/internal/wire"
)

// WakeLimiter is the concurrency seam at the planner wake dispatch point
// (spec §9.5.2; operator DECISION 1: per-task wakes run in parallel with
// no cap in v1, throttling comes later). Acquire blocks until a session
// may start and returns its release func; an error means the session
// does not run (the task gets the failure note unless ctx is done).
type WakeLimiter interface {
	Acquire(ctx context.Context) (release func(), err error)
}

// unlimitedWakes is the v1 default WakeLimiter: never blocks.
type unlimitedWakes struct{}

func (unlimitedWakes) Acquire(context.Context) (func(), error) { return func() {}, nil }

// SemaphoreWakeLimiter caps concurrent planner sessions at its capacity.
// Not wired by default (v1 is unlimited); it is the ready-made cap the
// seam exists for, and the unit-tested reference implementation.
type SemaphoreWakeLimiter chan struct{}

// NewSemaphoreWakeLimiter returns a limiter admitting n concurrent
// sessions (n < 1 is treated as 1).
func NewSemaphoreWakeLimiter(n int) SemaphoreWakeLimiter {
	if n < 1 {
		n = 1
	}
	return make(SemaphoreWakeLimiter, n)
}

// Acquire implements WakeLimiter.
func (s SemaphoreWakeLimiter) Acquire(ctx context.Context) (func(), error) {
	select {
	case s <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// wakeDispatcher runs planner sessions per task (spec §9.5.2):
//   - sessions for DIFFERENT tasks run in parallel (each in its own
//     goroutine, gated only by the WakeLimiter);
//   - sessions for the SAME task never overlap: envelopes arriving while
//     a task's session runs are coalesced into ONE follow-up session that
//     starts when the current one ends;
//   - no waker is lost: the hand-off between "session ending" and "new
//     envelope arriving" is decided under one mutex, so an arrival either
//     lands in pending (and the running driver picks it up) or finds the
//     task idle (and starts a new driver);
//   - a failed, refused or panicking session calls fail (the brain.reply
//     note INTO the task), never silently;
//   - the in-flight set (current + pending message ids per task) is
//     persisted through persist on every change, so a restart re-runs
//     wakes that were interrupted (recoverWakes).
type wakeDispatcher struct {
	limiter WakeLimiter
	run     func(ctx context.Context, taskID string, envs []wire.MessageEnvelope) error
	fail    func(ctx context.Context, taskID string, err error)
	persist func(inflight map[string][]string)
	// defer_ (S5): after a non-chat session fails, reports true when the
	// usage-limit pause explains it and has now ended - run it again.
	defer_ func(ctx context.Context, err error) bool

	mu    sync.Mutex
	tasks map[string]*taskWakeState
	wg    sync.WaitGroup
}

type taskWakeState struct {
	current []wire.MessageEnvelope
	pending []wire.MessageEnvelope
}

// Dispatch schedules a planner session for taskID over envs: started now
// when the task is idle, else coalesced into the task's follow-up wake.
func (d *wakeDispatcher) Dispatch(ctx context.Context, taskID string, envs []wire.MessageEnvelope) {
	if len(envs) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tasks == nil {
		d.tasks = map[string]*taskWakeState{}
	}
	if st, busy := d.tasks[taskID]; busy {
		st.pending = appendNew(st.pending, st.current, envs)
		d.persistLocked()
		return
	}
	st := &taskWakeState{current: envs}
	d.tasks[taskID] = st
	d.persistLocked()
	d.wg.Add(1)
	go d.drive(ctx, taskID, st)
}

// appendNew appends the envelopes of add not already in dst or cur (by
// id): a re-delivered envelope is coalesced, not run twice.
func appendNew(dst, cur, add []wire.MessageEnvelope) []wire.MessageEnvelope {
	seen := map[string]bool{}
	for _, e := range cur {
		seen[e.ID] = true
	}
	for _, e := range dst {
		seen[e.ID] = true
	}
	for _, e := range add {
		if e.ID != "" && seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		dst = append(dst, e)
	}
	return dst
}

// drive runs taskID's sessions back to back until nothing is pending.
func (d *wakeDispatcher) drive(ctx context.Context, taskID string, st *taskWakeState) {
	defer d.wg.Done()
	for {
		d.mu.Lock()
		envs := st.current
		d.mu.Unlock()

		d.session(ctx, taskID, envs)

		d.mu.Lock()
		if ctx.Err() != nil {
			// Shutdown: keep the persisted in-flight set as is, so the
			// next start re-runs the interrupted and pending wakes.
			delete(d.tasks, taskID)
			d.mu.Unlock()
			return
		}
		if len(st.pending) == 0 {
			delete(d.tasks, taskID)
			d.persistLocked()
			d.mu.Unlock()
			return
		}
		st.current, st.pending = st.pending, nil
		d.persistLocked()
		d.mu.Unlock()
	}
}

// session runs one planner session under the limiter. Every failure mode
// - limiter refusal, session error, panic - reaches fail unless the
// process is shutting down (then the persisted set recovers it).
func (d *wakeDispatcher) session(ctx context.Context, taskID string, envs []wire.MessageEnvelope) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("factoryapp: planner: session for %s panicked: %v", taskID, r)
			d.fail(ctx, taskID, fmt.Errorf("planner session panicked: %v", r))
		}
	}()
	chat := wakeIsChat(envs)
	ctx = context.WithValue(ctx, wakeKey{}, chat)
	for {
		d.mu.Lock()
		limiter := d.limiter
		d.mu.Unlock()
		if limiter == nil {
			limiter = unlimitedWakes{}
		}
		release, err := limiter.Acquire(ctx)
		if err != nil {
			if ctx.Err() == nil {
				d.fail(ctx, taskID, fmt.Errorf("planner wake limiter: %w", err))
			}
			return
		}
		err = func() error {
			if release != nil {
				defer release() // also on panic: never leak a limiter slot
			}
			return d.run(ctx, taskID, envs)
		}()
		if err == nil || ctx.Err() != nil {
			return
		}
		if !chat && d.defer_ != nil && d.defer_(ctx, err) {
			continue // the usage limit paused us mid-wake: run again after the resume
		}
		d.fail(ctx, taskID, err)
		return
	}
}

// Busy reports whether taskID has a session running or queued.
func (d *wakeDispatcher) Busy(taskID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.tasks[taskID]
	return ok
}

// Wait blocks until every running driver has returned.
func (d *wakeDispatcher) Wait() { d.wg.Wait() }

// persistLocked hands the in-flight snapshot to persist. d.mu held.
func (d *wakeDispatcher) persistLocked() {
	if d.persist == nil {
		return
	}
	snap := make(map[string][]string, len(d.tasks))
	for id, st := range d.tasks {
		ids := make([]string, 0, len(st.current)+len(st.pending))
		for _, e := range st.current {
			ids = append(ids, e.ID)
		}
		for _, e := range st.pending {
			ids = append(ids, e.ID)
		}
		snap[id] = ids
	}
	d.persist(snap)
}

// keyPlannerInflight is the settings key holding the persisted in-flight
// wake set: JSON {task_id: [message ids]} (spec §9.5.2).
const keyPlannerInflight = "planner_wake_inflight"

// persistInflight writes the in-flight wake set to the settings table.
func (a *App) persistInflight(inflight map[string][]string) {
	data, err := json.Marshal(inflight)
	if err != nil {
		log.Printf("factoryapp: planner: persist in-flight wakes: %v", err)
		return
	}
	if err := a.tasks.SetSetting(context.Background(), keyPlannerInflight, string(data)); err != nil {
		log.Printf("factoryapp: planner: persist in-flight wakes: %v", err)
	}
}

// recoverWakes re-dispatches wakes that were in flight when the process
// last stopped (spec §9.5.2): their envelopes were already acked, so
// without this a restart mid-session would lose them.
func (a *App) recoverWakes(ctx context.Context) {
	raw, ok, err := a.tasks.GetSetting(ctx, keyPlannerInflight)
	if err != nil {
		log.Printf("factoryapp: planner: read in-flight wakes: %v", err)
		return
	}
	if !ok || raw == "" {
		return
	}
	var inflight map[string][]string
	if err := json.Unmarshal([]byte(raw), &inflight); err != nil {
		log.Printf("factoryapp: planner: in-flight wakes %q: %v", raw, err)
		return
	}
	taskIDs := make([]string, 0, len(inflight))
	for id := range inflight {
		taskIDs = append(taskIDs, id)
	}
	sort.Strings(taskIDs)
	var envs []wire.MessageEnvelope
	for _, id := range taskIDs {
		for _, mid := range inflight[id] {
			env, found, err := a.inbox.GetMessage(ctx, mid)
			if err != nil || !found {
				log.Printf("factoryapp: planner: recover wake %s for %s: found=%v err=%v", mid, id, found, err)
				continue
			}
			if env.ThreadID == "" {
				env.ThreadID = id
			}
			envs = append(envs, env)
		}
	}
	if len(envs) == 0 {
		a.persistInflight(map[string][]string{})
		return
	}
	log.Printf("factoryapp: planner: recovering %d in-flight wake envelope(s) across %d task(s)", len(envs), len(taskIDs))
	a.dispatchWakers(ctx, envs)
}
