package foremanapp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// intakeGate is the auto-update drain gate (spec §12.4, contract C6). It
// counts live work - spawned workers, the running brain session, and
// job.request dispatches between their intake check and their spawn - and
// pauses NEW intake while an update waits for that work to finish.
// Paused intake leaves envelopes unacked in their inbox (redelivered after
// resume or restart); nothing live is ever interrupted.
//
// pause and the begin* checks share one lock, so after pause returns no
// new work can begin and busy() is an exact upper bound: an idle sample
// taken after pause stays idle.
type intakeGate struct {
	mu          sync.Mutex
	paused      bool
	deferred    int // envelopes refused since the last pause
	dispatching int
	workers     int
	brain       int
}

// beginDispatch admits one job.request dispatch; false while paused (the
// caller leaves the envelope unacked). Pair with endDispatch.
func (g *intakeGate) beginDispatch() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		g.deferred++
		return false
	}
	g.dispatching++
	return true
}

func (g *intakeGate) endDispatch() {
	g.mu.Lock()
	g.dispatching--
	g.mu.Unlock()
}

// beginBrain admits one brain session; false while paused (the queue copy
// stays unacked in the durable brain queue). Pair with endBrain.
func (g *intakeGate) beginBrain() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		g.deferred++
		return false
	}
	g.brain++
	return true
}

func (g *intakeGate) endBrain() {
	g.mu.Lock()
	g.brain--
	g.mu.Unlock()
}

// workerStarted / workerDone bracket one spawned worker's lifetime.
// Workers spawned by live sessions (spawn_worker, spawn_child) are never
// refused: the session that spawns them is itself live work.
func (g *intakeGate) workerStarted() {
	g.mu.Lock()
	g.workers++
	g.mu.Unlock()
}

func (g *intakeGate) workerDone() {
	g.mu.Lock()
	g.workers--
	g.mu.Unlock()
}

func (g *intakeGate) pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.paused {
		g.paused = true
		g.deferred = 0
	}
}

// resume reopens intake and reports whether anything was refused while
// paused (the caller then re-delivers it).
func (g *intakeGate) resume() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	wasPaused := g.paused
	g.paused = false
	d := g.deferred
	g.deferred = 0
	return wasPaused && d > 0
}

func (g *intakeGate) isPaused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

// busy reports whether any live work remains, with a description.
func (g *intakeGate) busy() (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var parts []string
	if g.workers > 0 {
		parts = append(parts, fmt.Sprintf("%d live worker(s)", g.workers))
	}
	if g.brain > 0 {
		parts = append(parts, "live brain session")
	}
	if g.dispatching > 0 {
		parts = append(parts, fmt.Sprintf("%d dispatch(es) in flight", g.dispatching))
	}
	if len(parts) == 0 {
		return false, "idle"
	}
	return true, strings.Join(parts, ", ")
}

// resumeIntake reopens intake after a drain and re-delivers deferred
// factory envelopes without waiting for new traffic.
func (a *App) resumeIntake() {
	if a.gate.resume() && a.client != nil {
		a.client.Resync()
	}
}

// requestExit asks the process to exit with code (the Updater's Exit):
// cmd/foreman shuts down gracefully and exits with it. Only the first
// request counts.
func (a *App) requestExit(code int) {
	select {
	case a.exitCh <- code:
	default:
	}
}

// ExitRequested delivers the exit code the self-updater asked for: 0 after
// a successful swap, non-zero after a rollback (spec §12).
func (a *App) ExitRequested() <-chan int { return a.exitCh }

// MarkListening records the bound listener address; the post-update
// confirm self-check (spec §12.5) dials it.
func (a *App) MarkListening(addr string) {
	a.listenMu.Lock()
	a.listenAddr = addr
	a.listenMu.Unlock()
}

// Updater returns the self-updater (test/integration accessor).
func (a *App) Updater() *Updater { return a.updater }

// Build returns the build id the foreman reports (spec §12.2).
func (a *App) Build() string { return a.build }

// selfCheck is the confirm self-check (spec §12.5): both stores answer and
// the listener accepts connections.
func (a *App) selfCheck(ctx context.Context) error {
	if _, err := a.inbox.Summaries(ctx); err != nil {
		return fmt.Errorf("inbox store: %w", err)
	}
	if _, _, err := a.auth.Identify(ctx, a.brainToken); err != nil {
		return fmt.Errorf("auth store: %w", err)
	}
	a.listenMu.Lock()
	addr := a.listenAddr
	a.listenMu.Unlock()
	if addr == "" {
		return errors.New("listener not up")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listener %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 2*time.Second)
	if err != nil {
		return fmt.Errorf("listener: %w", err)
	}
	conn.Close()
	return nil
}
