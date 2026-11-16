package foremanapp_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

// TestSpawnWorkerOutlivesCallerContext reproduces the brain-MCP spawn bug
// (foreman:server report, 2026-11-15): on the spawn_worker path SpawnWorker's
// caller ctx is the MCP HTTP request context, which net/http cancels the
// moment the tool call returns. The harness derived the worker's run context
// from it, so the freshly spawned opencode child was killed instantly while
// spawn_worker had already returned {job_id} - the dispatch silently looked
// done and no work ever happened. The spawned worker must outlive the
// caller's request context and still report.
func TestSpawnWorkerOutlivesCallerContext(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	forEnv := newForeman(t, fac, token)

	// The fake worker sleeps past the spawn call, writes a marker, and
	// prints the summary the harness fallback reports.
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	fakeBin := fakeOpencode(t, fmt.Sprintf("sleep 1; touch %s; printf survived", marker))

	var mut sync.Mutex
	var reported []wire.JobResult
	orig := forEnv.a.BuildHarnessConfig
	forEnv.a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		c := orig(spec, bearer)
		c.OpenCODEBin = fakeBin
		c.ResultURL = forEnv.ts.URL + "/v1/inboxes/foreman:alpha/messages"
		c.MCPURL = forEnv.ts.URL + "/v1/mcp/worker"
		c.Timeout = 15 * time.Second
		c.PostRetryBackoff = 10 * time.Millisecond
		c.Post = func(_ context.Context, _, _ string, res wire.JobResult) error {
			mut.Lock()
			defer mut.Unlock()
			reported = append(reported, res)
			return nil
		}
		return c
	}

	spec := wire.NewJobSpec("sleep, then mark", "factory:main")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := forEnv.a.SpawnWorker(ctx, spec); err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}
	// spawn_worker's HTTP handler has returned: its request context is
	// dead from here on. The spawned worker must still run to completion.
	cancel()

	waitFor(t, "worker ran to completion after caller ctx cancel", 8*time.Second, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})

	waitFor(t, "worker report after caller ctx cancel", 8*time.Second, func() bool {
		mut.Lock()
		defer mut.Unlock()
		return len(reported) == 1
	})
	mut.Lock()
	res := reported[0]
	mut.Unlock()
	if res.Status != wire.StatusDone || res.Summary != "survived" {
		t.Fatalf("report = %+v, want done/survived", res)
	}
}

// TestSpawnFailureSurfacesAsFailedJob is the loud-failure e2e: a worker that
// dies (here: the harness cannot deliver its report) must surface as a
// failed job at the factory - never a silent running-forever fake success.
func TestSpawnFailureSurfacesAsFailedJob(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	forEnv := newForeman(t, fac, token)

	fakeBin := fakeOpencode(t, `echo boom >&2; exit 1`)
	orig := forEnv.a.BuildHarnessConfig
	forEnv.a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		c := orig(spec, bearer)
		c.OpenCODEBin = fakeBin
		c.ResultURL = forEnv.ts.URL + "/v1/inboxes/foreman:alpha/messages"
		c.MCPURL = forEnv.ts.URL + "/v1/mcp/worker"
		c.Timeout = 15 * time.Second
		c.PostRetryBackoff = 10 * time.Millisecond
		// Report delivery is down, so the harness cannot land its own
		// crash result: Run errors after its retries. The spawner must
		// still surface the failure to the factory.
		c.Post = func(_ context.Context, _, _ string, _ wire.JobResult) error {
			return errors.New("post down")
		}
		return c
	}

	spec := wire.NewJobSpec("crash the worker", "factory")
	ctx := fac.ctx
	if err := fac.a.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := fac.a.Jobs().Dispatch(ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	var job jobs.Job
	waitFor(t, "job failed at the factory", 10*time.Second, func() bool {
		_ = fac.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
		// jobs.ApplyResult sets the state and last_error in two
		// statements: wait for both, or a poll in between flakes.
		return job.State == jobs.StateFailed && job.LastError != ""
	})
	if job.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed", job.State)
	}
	if !strings.Contains(job.LastError, "spawn failure") {
		t.Errorf("last_error = %q, want the spawn failure surfaced", job.LastError)
	}
}

// TestSpawnWorkerVisibleInFactoryRegistry closes the spawn-visibility gap
// (planner report, 2026-11-15): foreman-locally-spawned jobs - the brain-MCP
// spawn_worker seam, which mints its JobSpec on the foreman and never went
// through a factory dispatch - existed only in the foreman's local jobs/
// store. GET /v1/jobs and read_result never saw them; their accepted/
// started events and forwarded results were ignored as unknown. The spawned
// job must appear in the factory registry with the ordinary lifecycle:
// registered (dispatched) by the accepted event's carried spec, running via
// started, done via the forwarded result - attributed to the spawning
// foreman, exactly one registry row.
func TestSpawnWorkerVisibleInFactoryRegistry(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	forEnv := newForeman(t, fac, token)

	fakeBin := fakeOpencode(t, `printf registry-visible`)
	orig := forEnv.a.BuildHarnessConfig
	forEnv.a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		c := orig(spec, bearer)
		c.OpenCODEBin = fakeBin
		c.ResultURL = forEnv.ts.URL + "/v1/inboxes/foreman:alpha/messages"
		c.MCPURL = forEnv.ts.URL + "/v1/mcp/worker"
		c.Timeout = 15 * time.Second
		c.PostRetryBackoff = 10 * time.Millisecond
		return c
	}

	// No factory-side Create/Dispatch: the registry row can only come from
	// the spawn's own visibility events.
	task := "brain-local spawn: must be visible in the registry"
	spec := wire.NewJobSpec(task, "foreman:alpha")
	if err := forEnv.a.SpawnWorker(context.Background(), spec); err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	var job jobs.Job
	waitFor(t, "job done at the factory", 15*time.Second, func() bool {
		_ = fac.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
		return job.State == jobs.StateDone
	})
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done", job.State)
	}
	if job.ForemanID != "foreman:alpha" {
		t.Errorf("foreman_id = %q, want foreman:alpha (the spawning foreman)", job.ForemanID)
	}
	if job.Spec.Task != task {
		t.Errorf("registry spec task = %q, want the carried spec's %q", job.Spec.Task, task)
	}

	// Exactly one registry entry: GET /v1/jobs lists it once.
	var list []jobs.Job
	if status := fac.do("GET", "/v1/jobs", "", nil, &list); status != 200 {
		t.Fatalf("GET /v1/jobs = %d, want 200", status)
	}
	rows := 0
	for _, j := range list {
		if j.ID == spec.ID {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("GET /v1/jobs holds %d rows for %s, want 1", rows, spec.ID)
	}
}

// TestSpawnFailureVisibleInFactoryRegistry is the failure-path twin of the
// spawn-visibility fix (planner report, 2026-11-15): a foreman-local spawn
// whose harness fails (here: report delivery is down, so the §8.5 fallback
// synthesizes a loud failed result into the foreman's inbox) must be visible
// at the factory as failed - the registered row exists (accepted event with
// spec) before the harness can report, so the synthesized result applies
// instead of being dropped as unknown. Unlike
// TestSpawnFailureSurfacesAsFailedJob, NO factory-side row is pre-created:
// registration must come entirely from the spawn events.
func TestSpawnFailureVisibleInFactoryRegistry(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	forEnv := newForeman(t, fac, token)

	fakeBin := fakeOpencode(t, `echo boom >&2; exit 1`)
	orig := forEnv.a.BuildHarnessConfig
	forEnv.a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		c := orig(spec, bearer)
		c.OpenCODEBin = fakeBin
		c.ResultURL = forEnv.ts.URL + "/v1/inboxes/foreman:alpha/messages"
		c.MCPURL = forEnv.ts.URL + "/v1/mcp/worker"
		c.Timeout = 15 * time.Second
		c.PostRetryBackoff = 10 * time.Millisecond
		// Report delivery is down, so the harness cannot land its own
		// crash result: Run errors after its retries and the spawner
		// synthesizes the loud failed job.result (spec §8.5).
		c.Post = func(_ context.Context, _, _ string, _ wire.JobResult) error {
			return errors.New("post down")
		}
		return c
	}

	spec := wire.NewJobSpec("brain-local spawn: harness failure must be visible", "foreman:alpha")
	if err := forEnv.a.SpawnWorker(fac.ctx, spec); err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	var job jobs.Job
	waitFor(t, "locally-spawned job failed at the factory", 15*time.Second, func() bool {
		_ = fac.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
		// jobs.ApplyResult sets the state and last_error in two
		// statements: wait for both, or a poll in between flakes.
		return job.State == jobs.StateFailed && job.LastError != ""
	})
	if job.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed", job.State)
	}
	if job.ForemanID != "foreman:alpha" {
		t.Errorf("foreman_id = %q, want foreman:alpha", job.ForemanID)
	}
	if !strings.Contains(job.LastError, "spawn failure") {
		t.Errorf("last_error = %q, want the synthesized spawn failure surfaced", job.LastError)
	}
}
