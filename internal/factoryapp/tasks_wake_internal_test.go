// Unit tests for the per-task planner wake dispatcher (spec §9.5.2):
// parallel across tasks, serialized + coalesced within a task, no lost
// wakers, never-silent failures, and the WakeLimiter seam.
package factoryapp

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// wakeRecorder is a fake session body: it records every session, tracks
// per-task and global concurrency, and can block sessions on a gate.
type wakeRecorder struct {
	mu          sync.Mutex
	sessions    map[string][][]string // task -> per-session env ids
	running     map[string]int
	maxPerTask  int
	global      int
	maxGlobal   int
	failures    map[string][]string
	gate        chan struct{} // nil = never block
	started     chan string   // receives the task id as each session starts (buffered)
	errFor      func(taskID string) error
	panicFor    func(taskID string) bool
	sessionWait time.Duration
}

func newWakeRecorder() *wakeRecorder {
	return &wakeRecorder{
		sessions: map[string][][]string{},
		running:  map[string]int{},
		failures: map[string][]string{},
		started:  make(chan string, 1024),
	}
}

func (r *wakeRecorder) run(ctx context.Context, taskID string, envs []wire.MessageEnvelope) error {
	ids := make([]string, len(envs))
	for i, e := range envs {
		ids[i] = e.ID
	}
	r.mu.Lock()
	r.sessions[taskID] = append(r.sessions[taskID], ids)
	r.running[taskID]++
	if r.running[taskID] > r.maxPerTask {
		r.maxPerTask = r.running[taskID]
	}
	r.global++
	if r.global > r.maxGlobal {
		r.maxGlobal = r.global
	}
	gate := r.gate
	r.mu.Unlock()
	r.started <- taskID
	defer func() {
		r.mu.Lock()
		r.running[taskID]--
		r.global--
		r.mu.Unlock()
	}()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.sessionWait > 0 {
		time.Sleep(r.sessionWait)
	}
	if r.panicFor != nil && r.panicFor(taskID) {
		panic("boom")
	}
	if r.errFor != nil {
		return r.errFor(taskID)
	}
	return nil
}

func (r *wakeRecorder) fail(_ context.Context, taskID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[taskID] = append(r.failures[taskID], err.Error())
}

func (r *wakeRecorder) dispatcher() *wakeDispatcher {
	return &wakeDispatcher{limiter: unlimitedWakes{}, run: r.run, fail: r.fail}
}

func envsOf(ids ...string) []wire.MessageEnvelope {
	out := make([]wire.MessageEnvelope, len(ids))
	for i, id := range ids {
		out[i] = wire.MessageEnvelope{ID: id, Type: "chat.message"}
	}
	return out
}

func waitStarted(t *testing.T, r *wakeRecorder, n int) []string {
	t.Helper()
	var got []string
	for len(got) < n {
		select {
		case id := <-r.started:
			got = append(got, id)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d sessions started: %v", len(got), n, got)
		}
	}
	return got
}

// TestWakeParallelAcrossTasks: two tasks' sessions run at the same time
// (no cap in v1, DECISION 1).
func TestWakeParallelAcrossTasks(t *testing.T) {
	r := newWakeRecorder()
	r.gate = make(chan struct{})
	d := r.dispatcher()
	ctx := context.Background()

	d.Dispatch(ctx, "task_a", envsOf("m1"))
	d.Dispatch(ctx, "task_b", envsOf("m2"))
	started := waitStarted(t, r, 2) // both started while the gate is shut
	sort.Strings(started)
	if strings.Join(started, ",") != "task_a,task_b" {
		t.Fatalf("started = %v", started)
	}
	if !d.Busy("task_a") || !d.Busy("task_b") || d.Busy("task_c") {
		t.Errorf("Busy: a=%v b=%v c=%v", d.Busy("task_a"), d.Busy("task_b"), d.Busy("task_c"))
	}
	close(r.gate)
	d.Wait()
	if r.maxGlobal != 2 {
		t.Errorf("max global concurrency = %d, want 2", r.maxGlobal)
	}
	if d.Busy("task_a") || d.Busy("task_b") {
		t.Error("tasks still busy after Wait")
	}
}

// TestWakeSerializedAndCoalesced: envelopes arriving mid-session on the
// same task never start a concurrent session; they coalesce into ONE
// follow-up session (duplicates dropped).
func TestWakeSerializedAndCoalesced(t *testing.T) {
	r := newWakeRecorder()
	r.gate = make(chan struct{})
	d := r.dispatcher()
	ctx := context.Background()

	d.Dispatch(ctx, "task_a", envsOf("m1"))
	waitStarted(t, r, 1)
	d.Dispatch(ctx, "task_a", envsOf("m2"))
	d.Dispatch(ctx, "task_a", envsOf("m3", "m1")) // m1 is the running one: dropped
	d.Dispatch(ctx, "task_a", envsOf("m2"))       // already pending: dropped
	select {
	case id := <-r.started:
		t.Fatalf("a concurrent session started for %s", id)
	case <-time.After(100 * time.Millisecond):
	}
	close(r.gate) // releases the first and (later) the follow-up
	d.Wait()

	got := r.sessions["task_a"]
	if len(got) != 2 {
		t.Fatalf("sessions = %v, want 2 (initial + one coalesced follow-up)", got)
	}
	if fmt.Sprint(got[0]) != "[m1]" || fmt.Sprint(got[1]) != "[m2 m3]" {
		t.Errorf("sessions = %v, want [[m1] [m2 m3]]", got)
	}
	if r.maxPerTask != 1 {
		t.Errorf("max per-task concurrency = %d, want 1", r.maxPerTask)
	}
}

// TestWakeNoLostWakers hammers the dispatcher from many goroutines across
// a few tasks: every dispatched envelope runs in exactly one session, and
// no task ever has two sessions at once.
func TestWakeNoLostWakers(t *testing.T) {
	r := newWakeRecorder()
	r.sessionWait = 200 * time.Microsecond
	d := r.dispatcher()
	ctx := context.Background()

	const workers, perWorker = 16, 60
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for i := 0; i < perWorker; i++ {
				task := fmt.Sprintf("task_%d", rng.Intn(4))
				d.Dispatch(ctx, task, envsOf(fmt.Sprintf("m_%d_%d", w, i)))
				if rng.Intn(4) == 0 {
					time.Sleep(time.Duration(rng.Intn(300)) * time.Microsecond)
				}
			}
		}(w)
	}
	wg.Wait()
	d.Wait()

	seen := map[string]int{}
	for _, sessions := range r.sessions {
		for _, ids := range sessions {
			for _, id := range ids {
				seen[id]++
			}
		}
	}
	if len(seen) != workers*perWorker {
		t.Errorf("delivered %d distinct envelopes, want %d", len(seen), workers*perWorker)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("envelope %s delivered %d times", id, n)
		}
	}
	if r.maxPerTask != 1 {
		t.Errorf("max per-task concurrency = %d, want 1", r.maxPerTask)
	}
	// Drain the started channel so it never fills on reruns.
	for len(r.started) > 0 {
		<-r.started
	}
}

// TestWakeFailuresNeverSilent: a session error, a panic, and a limiter
// refusal each reach fail (the brain.reply note into the task); a
// follow-up wake still runs after a failure.
func TestWakeFailuresNeverSilent(t *testing.T) {
	r := newWakeRecorder()
	r.errFor = func(id string) error {
		if id == "task_err" {
			return errors.New("opencode exploded")
		}
		return nil
	}
	r.panicFor = func(id string) bool { return id == "task_panic" }
	d := r.dispatcher()
	ctx := context.Background()

	d.Dispatch(ctx, "task_err", envsOf("m1"))
	d.Dispatch(ctx, "task_panic", envsOf("m2"))
	d.Dispatch(ctx, "task_ok", envsOf("m3"))
	d.Wait()

	if f := r.failures["task_err"]; len(f) != 1 || !strings.Contains(f[0], "opencode exploded") {
		t.Errorf("task_err failures = %v", f)
	}
	if f := r.failures["task_panic"]; len(f) != 1 || !strings.Contains(f[0], "panicked") {
		t.Errorf("task_panic failures = %v", f)
	}
	if f := r.failures["task_ok"]; len(f) != 0 {
		t.Errorf("task_ok failures = %v", f)
	}
	// The panicking task is not wedged: a new wake runs.
	d.Dispatch(ctx, "task_panic", envsOf("m4"))
	d.Wait()
	if n := len(r.sessions["task_panic"]); n != 2 {
		t.Errorf("task_panic sessions = %d, want 2", n)
	}

	// Limiter refusal (not a shutdown) is a failure too.
	d.limiter = refusingLimiter{}
	d.Dispatch(ctx, "task_refused", envsOf("m5"))
	d.Wait()
	if f := r.failures["task_refused"]; len(f) != 1 || !strings.Contains(f[0], "limiter") {
		t.Errorf("task_refused failures = %v", f)
	}
}

type refusingLimiter struct{}

func (refusingLimiter) Acquire(context.Context) (func(), error) {
	return nil, errors.New("throttled")
}

// TestWakeShutdownKeepsInflight: a session interrupted by shutdown posts
// no failure note and leaves the persisted in-flight set intact (so the
// next start re-runs it), while a clean completion clears it.
func TestWakeShutdownKeepsInflight(t *testing.T) {
	r := newWakeRecorder()
	r.gate = make(chan struct{})
	var mu sync.Mutex
	var snaps []map[string][]string
	d := r.dispatcher()
	d.persist = func(m map[string][]string) {
		mu.Lock()
		defer mu.Unlock()
		snaps = append(snaps, m)
	}
	ctx, cancel := context.WithCancel(context.Background())

	d.Dispatch(ctx, "task_a", envsOf("m1"))
	waitStarted(t, r, 1)
	d.Dispatch(ctx, "task_a", envsOf("m2"))
	cancel()
	d.Wait()

	if len(r.failures) != 0 {
		t.Errorf("shutdown produced failure notes: %v", r.failures)
	}
	mu.Lock()
	last := snaps[len(snaps)-1]
	mu.Unlock()
	if fmt.Sprint(last["task_a"]) != "[m1 m2]" {
		t.Errorf("last persisted snapshot = %v, want task_a:[m1 m2]", last)
	}

	// Clean run: the final snapshot is empty.
	r2 := newWakeRecorder()
	d2 := r2.dispatcher()
	snaps = nil
	d2.persist = d.persist
	d2.Dispatch(context.Background(), "task_b", envsOf("m3"))
	d2.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(snaps) < 2 || len(snaps[0]["task_b"]) != 1 || len(snaps[len(snaps)-1]) != 0 {
		t.Errorf("snapshots = %v, want [task_b:[m3]] ... then {}", snaps)
	}
}

// countingLimiter is a fake cap: a counting semaphore that records the
// peak number of holders.
type countingLimiter struct {
	sem      chan struct{}
	holders  atomic.Int32
	peak     atomic.Int32
	acquires atomic.Int32
}

func (l *countingLimiter) Acquire(ctx context.Context) (func(), error) {
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	l.acquires.Add(1)
	n := l.holders.Add(1)
	for {
		p := l.peak.Load()
		if n <= p || l.peak.CompareAndSwap(p, n) {
			break
		}
	}
	return func() { l.holders.Add(-1); <-l.sem }, nil
}

// TestWakeLimiterSeam: a fake cap of 2 bounds concurrent sessions across
// 6 tasks; every session still runs.
func TestWakeLimiterSeam(t *testing.T) {
	r := newWakeRecorder()
	r.sessionWait = 20 * time.Millisecond
	lim := &countingLimiter{sem: make(chan struct{}, 2)}
	d := r.dispatcher()
	d.limiter = lim
	for i := 0; i < 6; i++ {
		d.Dispatch(context.Background(), fmt.Sprintf("task_%d", i), envsOf(fmt.Sprintf("m%d", i)))
	}
	d.Wait()
	if lim.peak.Load() != 2 {
		t.Errorf("peak concurrent sessions = %d, want 2 (the cap)", lim.peak.Load())
	}
	if lim.acquires.Load() != 6 || len(r.sessions) != 6 {
		t.Errorf("acquires = %d, sessions = %d; want 6 each", lim.acquires.Load(), len(r.sessions))
	}
	if r.maxGlobal > 2 {
		t.Errorf("max global concurrency = %d, want <= 2", r.maxGlobal)
	}
}

// TestSemaphoreWakeLimiter: the shipped cap implementation admits n,
// blocks the n+1st until a release, honours ctx, and release is
// idempotent.
func TestSemaphoreWakeLimiter(t *testing.T) {
	l := NewSemaphoreWakeLimiter(0) // clamps to 1
	rel, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Acquire = %v, want deadline exceeded", err)
	}
	rel()
	rel() // idempotent: must not free a second slot
	rel2, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	if _, err := l.Acquire(ctx2); err == nil {
		t.Fatal("double release freed an extra slot")
	}
	rel2()
	if rel, err := (unlimitedWakes{}).Acquire(context.Background()); err != nil || rel == nil {
		t.Errorf("unlimited Acquire: nil release or err %v", err)
	}
}

func TestAppendNew(t *testing.T) {
	got := appendNew(envsOf("a"), envsOf("cur"), envsOf("a", "b", "cur", "b", "c"))
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Errorf("appendNew = %v, want a,b,c", ids)
	}
	if d := (&wakeDispatcher{}); true {
		d.Dispatch(context.Background(), "task_x", nil) // empty: no-op, no panic
		if d.Busy("task_x") {
			t.Error("empty dispatch marked the task busy")
		}
	}
}
