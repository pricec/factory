package foremanapp_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pricec/factory/internal/foremanapp"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// TestOrphanRecoveryAgainstRealFactory is the restart path end to end over
// a REAL factory (spec §8.7): a foreman restart leaves one job running with
// a live worker process, one running with nothing behind it, and one owned
// by another foreman. The startup pass must fail only the orphan (crash, so
// the retry arms), leave the live worker and the foreign job running, and
// fail the live worker only after its process is gone.
func TestOrphanRecoveryAgainstRealFactory(t *testing.T) {
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("needs a Linux process table")
	}
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	// Pair through New but never call Run: Run starts its own startup
	// recovery pass, which would race the fixtures below. The explicit
	// ReconcileOrphans calls are the only passes in this test.
	dataDir := t.TempDir()
	fA, err := foremanapp.New(foremanapp.Config{
		FactoryURL:   fac.ts.URL,
		ID:           "alpha",
		DataDir:      dataDir,
		Addr:         "127.0.0.1:0",
		PairingToken: token,
	})
	if err != nil {
		t.Fatalf("foremanapp.New: %v", err)
	}
	t.Cleanup(func() { _ = fA.Close() })
	store := fac.a.Jobs()

	seed := func(owner string) string {
		t.Helper()
		spec := wire.NewJobSpec("orphan recovery fixture", "test")
		if _, err := store.RegisterSpawned(fac.ctx, spec, owner); err != nil {
			t.Fatalf("RegisterSpawned: %v", err)
		}
		if err := store.ApplyEvent(fac.ctx, spec.ID, "started"); err != nil {
			t.Fatalf("ApplyEvent started: %v", err)
		}
		return spec.ID
	}
	state := func(id string) jobs.Job {
		t.Helper()
		job, ok, err := store.Get(fac.ctx, id)
		if err != nil || !ok {
			t.Fatalf("Get %s: ok=%v err=%v", id, ok, err)
		}
		return job
	}

	live := seed("foreman:alpha")
	orphan := seed("foreman:alpha")
	foreign := seed("foreman:beta")

	// The live worker: a real child whose cwd is its job directory, exactly
	// as workerharness execChild starts a worker.
	liveDir := filepath.Join(dataDir, "jobs", live)
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "60")
	child.Dir = liveDir
	if err := child.Start(); err != nil {
		t.Fatalf("start live worker stand-in: %v", err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })

	rec, err := fA.ReconcileOrphans(fac.ctx)
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if len(rec.Live) != 1 || rec.Live[0] != live {
		t.Fatalf("Live = %v, want [%s]: a live worker must be left running", rec.Live, live)
	}
	if len(rec.Orphaned) != 1 || rec.Orphaned[0] != orphan {
		t.Fatalf("Orphaned = %v, want [%s]", rec.Orphaned, orphan)
	}

	waitFor(t, "orphan failed with retry armed", 5*time.Second, func() bool {
		j := state(orphan)
		return j.State == jobs.StateFailed && j.NextAttempt != "" && j.Attempts == 0
	})
	if got := state(orphan); got.LastError == "" {
		t.Errorf("orphan last_error empty: the failure must be loud in the registry")
	}
	if got := state(live).State; got != jobs.StateRunning {
		t.Errorf("live worker state = %s, want running", got)
	}
	if got := state(foreign).State; got != jobs.StateRunning {
		t.Errorf("foreign job state = %s, want running: another foreman's job must not be touched", got)
	}

	// The worker exits; its job is now a genuine orphan and the next pass
	// fails it. The foreign job still stays running.
	_ = child.Process.Kill()
	_, _ = child.Process.Wait()
	rec, err = fA.ReconcileOrphans(fac.ctx)
	if err != nil {
		t.Fatalf("second ReconcileOrphans: %v", err)
	}
	if len(rec.Orphaned) != 1 || rec.Orphaned[0] != live {
		t.Fatalf("second pass Orphaned = %v, want [%s]", rec.Orphaned, live)
	}
	waitFor(t, "formerly live job failed once its process exited", 5*time.Second, func() bool {
		return state(live).State == jobs.StateFailed
	})
	if got := state(foreign).State; got != jobs.StateRunning {
		t.Errorf("foreign job state = %s after second pass, want running", got)
	}
}
