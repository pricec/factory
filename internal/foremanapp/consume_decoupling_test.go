package foremanapp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/foremanapp"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

// sessionRecorder is the brain Exec seam shared by the decoupling tests:
// it records every session's prompt and completion in order, and lets
// each test decide when (and with what error) a session returns.
type sessionRecorder struct {
	mu        sync.Mutex
	prompts   []string
	completed int
	behavior  func(ctx context.Context, call int) error // nil = success
}

func (s *sessionRecorder) exec(ctx context.Context, prompt string) (string, error) {
	s.mu.Lock()
	call := len(s.prompts) + 1
	s.prompts = append(s.prompts, prompt)
	behavior := s.behavior
	s.mu.Unlock()
	var err error
	if behavior != nil {
		err = behavior(ctx, call)
	}
	s.mu.Lock()
	s.completed++
	s.mu.Unlock()
	return "brain stdout", err
}

func (s *sessionRecorder) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts)
}

func (s *sessionRecorder) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

func (s *sessionRecorder) completions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.completed
}

// TestBrainSessionDoesNotBlockHotPaths (spec §3.3, 2026-11-15 amendment):
// while a brain session is in flight - with the 30m bound it can hold the
// queue for half an hour - the hot paths must still progress: job.request
// spawns a worker, the worker's job.result forwards to the factory, and
// the job reaches a terminal state at the factory. Before the brain queue
// these all queued behind the LLM session inside the consume loop.
func TestBrainSessionDoesNotBlockHotPaths(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	fEnv := newForeman(t, fac, token)

	// A brain session that stays in flight until the test releases it:
	// the stand-in for a long (up to 30m) LLM turn.
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rec := &sessionRecorder{behavior: func(ctx context.Context, call int) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	fEnv.a.Brain().Exec = rec.exec

	// A brain.wake lands in the foreman's local inbox; the consume loop
	// must hand it to the durable brain queue and ack it WITHOUT waiting
	// for the session.
	wake := wire.MessageEnvelope{
		Type:    "brain.wake",
		From:    "factory:planner",
		Payload: json.RawMessage(`{"reason":"hot-path test"}`),
	}
	if _, err := fEnv.a.Inbox().Post(fEnv.ctx, "foreman:alpha", wake); err != nil {
		t.Fatalf("post brain.wake: %v", err)
	}

	// Wait until the drain loop is inside the session.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("brain session never started")
	}

	// The source envelope is acked at enqueue (the drain loop may start
	// the session a hair before the ack lands - poll for it), and the
	// queue copy is unacked in the brain queue while the session runs.
	var qenvs []wire.MessageEnvelope
	waitFor(t, "source inbox drained (acked at enqueue)", 10*time.Second, func() bool {
		envs, err := fEnv.a.Inbox().Get(fEnv.ctx, "foreman:alpha", 50, 0)
		if err != nil {
			t.Fatalf("foreman inbox get: %v", err)
		}
		qenvs, err = fEnv.a.Inbox().Get(fEnv.ctx, fEnv.a.BrainInboxID(), 50, 0)
		if err != nil {
			t.Fatalf("brain queue get: %v", err)
		}
		return len(envs) == 0 && len(qenvs) == 1
	})

	// Now run the FULL hot path while the session is still in flight:
	// create + dispatch a real job, let the foreman spawn a fake worker,
	// and let the fallback result forward back to the factory.
	fakeBin := fakeOpencode(t, `printf 'fake worker summary'`)
	orig := fEnv.a.BuildHarnessConfig
	fEnv.a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		c := orig(spec, bearer)
		c.OpenCODEBin = fakeBin
		c.ResultURL = fEnv.ts.URL + "/v1/inboxes/foreman:alpha/messages"
		c.MCPURL = fEnv.ts.URL + "/v1/mcp/worker"
		c.Timeout = 10 * time.Second
		c.PostRetryBackoff = 10 * time.Millisecond
		return c
	}
	spec := wire.NewJobSpec("count the widgets", "factory")
	ctx := context.Background()
	if err := fac.a.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := fac.a.Jobs().Dispatch(ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// The job must reach a terminal state at the factory even though the
	// brain session never returned. This is the regression: the consume
	// loop used to sit inside the LLM session here.
	var job jobs.Job
	waitFor(t, "job done while brain session in flight", 15*time.Second, func() bool {
		got, ok, err := fac.a.Jobs().Get(ctx, spec.ID)
		if err != nil || !ok {
			return false
		}
		job = got
		return job.State == jobs.StateDone
	})
	if rec.completions() != 0 {
		t.Fatalf("brain session completed %d times during the job flow, want 0 (still in flight)", rec.completions())
	}

	// Release the session: the queue copy acks and the queue drains.
	close(release)
	waitFor(t, "brain queue drained after release", 10*time.Second, func() bool {
		qenvs, err := fEnv.a.Inbox().Get(ctx, fEnv.a.BrainInboxID(), 50, 0)
		if err != nil {
			t.Fatalf("brain queue get: %v", err)
		}
		return len(qenvs) == 0
	})
	if got := rec.calls(); got != 1 {
		t.Errorf("brain sessions = %d, want 1 (no loss, no double-handling)", got)
	}
	if !strings.Contains(rec.recorded()[0], "brain.wake") {
		t.Errorf("brain prompt = %q, want the wake envelope rendered", rec.recorded()[0])
	}
}

// TestBrainFailureDoesNotBlockQueue (spec §3.3, 2026-11-15 amendment): a
// brain session that fails or times out must neither drop the following
// envelopes nor stall them - every queued envelope still gets its session,
// the hot path still forwards, and everything acks. The failing error text
// mirrors the runner's timeout message.
func TestBrainFailureDoesNotBlockQueue(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	fEnv := newForeman(t, fac, token)

	rec := &sessionRecorder{behavior: func(_ context.Context, _ int) error {
		return errors.New("brain session exceeded 30m0s")
	}}
	fEnv.a.Brain().Exec = rec.exec

	// Two brain-delegated envelopes straddling a hot-path result: the
	// second session must still run after the first fails.
	for _, n := range []int{1, 2} {
		payload, err := json.Marshal(map[string]int{"n": n})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fEnv.a.Inbox().Post(fEnv.ctx, "foreman:alpha", wire.MessageEnvelope{
			Type:    "brain.wake",
			From:    "factory:planner",
			Payload: payload,
		}); err != nil {
			t.Fatalf("post brain.wake %d: %v", n, err)
		}
	}
	res := wire.MessageEnvelope{
		Type:  "job.result",
		JobID: "job_hot",
		From:  "foreman:alpha",
		Payload: json.RawMessage(
			`{"job_id":"job_hot","status":"done","summary":"hot path ok","result":{"x":1},"usage":{"input_tokens":10,"output_tokens":5,"cost_usd":0.01}}`),
	}
	if _, err := fEnv.a.Inbox().Post(fEnv.ctx, "foreman:alpha", res); err != nil {
		t.Fatalf("post job.result: %v", err)
	}

	// Both failing sessions ran - the first failure did not block the
	// second envelope (poison envelopes are consumed, not retried
	// forever).
	waitFor(t, "both brain sessions attempted", 10*time.Second, func() bool {
		return rec.calls() == 2
	})

	// The hot path forwarded the result to the factory-side factory:main
	// (byte-identical payload), unaffected by the failing brain.
	delivered := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		envs, err := fac.a.Inbox().Recent(ctx, "factory:main", 50)
		if err != nil {
			t.Fatalf("factory inbox recent: %v", err)
		}
		for _, e := range envs {
			if e.Type == "job.result" && e.JobID == "job_hot" &&
				strings.Contains(string(e.Payload), `"summary":"hot path ok"`) {
				return true
			}
		}
		return false
	}
	drained := func() bool {
		envs, err := fEnv.a.Inbox().Get(fEnv.ctx, "foreman:alpha", 50, 0)
		if err != nil {
			t.Fatalf("foreman inbox get: %v", err)
		}
		qenvs, err := fEnv.a.Inbox().Get(fEnv.ctx, fEnv.a.BrainInboxID(), 50, 0)
		if err != nil {
			t.Fatalf("brain queue get: %v", err)
		}
		return len(envs) == 0 && len(qenvs) == 0
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if delivered() && drained() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("failing brain did not stay out of the way (delivered=%v drained=%v)", delivered(), drained())
}

// TestBrainQueueSurvivesRestart (spec §3.3, 2026-11-15 amendment): the
// brain queue is durable. A foreman that dies (or is stopped) with a
// session in flight leaves the queue copy unacked; a restart on the same
// data dir redelivers it - the envelope is never lost. The wake may
// legitimately be handled up to TWICE across the restart: a shutdown
// landing between the durable enqueue and the source ack leaves the
// source unacked, and the restart redelivers + re-enqueues it
// (at-least-once, §4 - the same window a crash between spawn_worker and
// ack gives a job.request). Both sessions carry the original payload.
func TestBrainQueueSurvivesRestart(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	dataDir := t.TempDir()

	// First boot: a brain session that hangs until the context is
	// canceled - the process-death stand-in (the session never completes,
	// so the queue copy is never acked).
	started := make(chan struct{})
	var once sync.Once
	app1, err := foremanapp.New(foremanapp.Config{
		FactoryURL:   fac.ts.URL,
		ID:           "alpha",
		DataDir:      dataDir,
		Addr:         "127.0.0.1:0",
		PairingToken: token,
	})
	if err != nil {
		t.Fatalf("foremanapp.New (boot 1): %v", err)
	}
	app1.Brain().Exec = func(ctx context.Context, prompt string) (string, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ts1 := httptest.NewServer(app1.Handler())
	runDone := make(chan struct{})
	go func() { _ = app1.Run(ctx1); close(runDone) }()

	wake := wire.MessageEnvelope{
		Type:    "brain.wake",
		From:    "factory:planner",
		Payload: json.RawMessage(`{"reason":"restart test"}`),
	}
	if _, err := app1.Inbox().Post(context.Background(), "foreman:alpha", wake); err != nil {
		t.Fatalf("post brain.wake: %v", err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("brain session never started")
	}

	// Kill the foreman mid-session (graceful shutdown of a dead session):
	// the queue copy must remain unacked.
	cancel1()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("foreman Run did not stop after cancel")
	}
	ts1.Close()
	if err := app1.Close(); err != nil {
		t.Fatalf("close boot 1: %v", err)
	}

	// Second boot on the SAME data dir: the queued wake must be
	// redelivered and handled exactly once.
	app2, err := foremanapp.New(foremanapp.Config{
		FactoryURL: fac.ts.URL,
		ID:         "alpha",
		DataDir:    dataDir,
		Addr:       "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("foremanapp.New (boot 2): %v", err)
	}
	rec := &sessionRecorder{}
	app2.Brain().Exec = rec.exec
	ctx2, cancel2 := context.WithCancel(context.Background())
	ts2 := httptest.NewServer(app2.Handler())
	go func() { _ = app2.Run(ctx2) }()
	t.Cleanup(func() {
		cancel2()
		ts2.Close()
		_ = app2.Close()
	})

	waitFor(t, "redelivered wake handled after restart", 10*time.Second, func() bool {
		return rec.calls() >= 1
	})
	waitFor(t, "brain queue drained after restart", 10*time.Second, func() bool {
		qenvs, err := app2.Inbox().Get(context.Background(), app2.BrainInboxID(), 50, 0)
		if err != nil {
			t.Fatalf("brain queue get: %v", err)
		}
		return len(qenvs) == 0
	})
	envs, err := app2.Inbox().Get(context.Background(), "foreman:alpha", 50, 0)
	if err != nil {
		t.Fatalf("foreman inbox get: %v", err)
	}
	if len(envs) != 0 {
		t.Errorf("source inbox holds %d unacked after restart, want 0 (redelivery consumed)", len(envs))
	}
	// At-least-once: at least one session (no loss), at most two (a
	// shutdown between enqueue and source ack re-enqueues the redelivered
	// source copy once), and every session saw the original payload.
	if got := rec.calls(); got < 1 || got > 2 {
		t.Errorf("brain sessions after restart = %d, want 1 or 2 (no loss, bounded redelivery)", got)
	}
	for i, p := range rec.recorded() {
		if !strings.Contains(p, "restart test") {
			t.Errorf("session %d prompt = %q, want the original wake payload", i+1, p)
		}
	}
}

// TestBrainQueueFIFOAndNoDuplication (spec §3.3, 2026-11-15 amendment):
// queued brain-delegated envelopes are handled one at a time in arrival
// order, each exactly once, and a hot-path envelope interleaved among
// them is dropped (job.event) without touching the brain.
func TestBrainQueueFIFOAndNoDuplication(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	fEnv := newForeman(t, fac, token)

	rec := &sessionRecorder{}
	fEnv.a.Brain().Exec = rec.exec

	// Interleave: wake 1, event (hot: drop), wake 2, wake 3.
	for _, n := range []int{1, 2, 3} {
		if n == 2 {
			if _, err := fEnv.a.Inbox().Post(fEnv.ctx, "foreman:alpha", wire.MessageEnvelope{
				Type:    "job.event",
				JobID:   "job_evt",
				From:    "factory:main",
				Payload: json.RawMessage(`{"event":"accepted"}`),
			}); err != nil {
				t.Fatalf("post job.event: %v", err)
			}
		}
		payload, err := json.Marshal(map[string]int{"n": n})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fEnv.a.Inbox().Post(fEnv.ctx, "foreman:alpha", wire.MessageEnvelope{
			Type:    "brain.wake",
			From:    "factory:planner",
			Payload: payload,
		}); err != nil {
			t.Fatalf("post brain.wake %d: %v", n, err)
		}
	}

	waitFor(t, "all three wake sessions ran", 10*time.Second, func() bool {
		return rec.calls() == 3
	})
	// Serial one-at-a-time draining: FIFO order, no duplicates.
	prompts := rec.recorded()
	for i, want := range []string{`"n":1`, `"n":2`, `"n":3`} {
		if !strings.Contains(prompts[i], want) {
			t.Errorf("session %d prompt = %q, want it to carry %s (FIFO order)", i+1, prompts[i], want)
		}
	}
	if rec.completions() != 3 {
		t.Errorf("completions = %d, want 3", rec.completions())
	}
	waitFor(t, "inbox and queue drained", 10*time.Second, func() bool {
		envs, err := fEnv.a.Inbox().Get(fEnv.ctx, "foreman:alpha", 50, 0)
		if err != nil {
			t.Fatalf("foreman inbox get: %v", err)
		}
		qenvs, err := fEnv.a.Inbox().Get(fEnv.ctx, fEnv.a.BrainInboxID(), 50, 0)
		if err != nil {
			t.Fatalf("brain queue get: %v", err)
		}
		return len(envs) == 0 && len(qenvs) == 0
	})
}
