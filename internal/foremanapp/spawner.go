package foremanapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"

	"github.com/pricec/factory/internal/ocusage"
	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

// jobsDirName is the per-job directory root under DataDir (spec §8.2).
const jobsDirName = "jobs"

// spawnerFunc adapts a spawn function to routing.Spawner (one
// implementation serves both the routing table and the mcpforeman
// spawn_worker seam, spec §3.3, §9.3).
type spawnerFunc func(ctx context.Context, spec wire.JobSpec) error

func (f spawnerFunc) Spawn(ctx context.Context, spec wire.JobSpec) error { return f(ctx, spec) }

// SpawnWorker mints the per-job worker credential (spec §7.3), persists the
// dispatched JobSpec, and runs the harness in a background goroutine
// (spec §8): it returns immediately - spawn never blocks. accepted and
// started job.event envelopes are posted to the factory (spec §6.2).
//
// ctx is the CALLER's context (the mcpforeman spawn_worker HTTP request or
// the inbox consumer loop). It is used only for the synchronous work here;
// the worker itself runs under a.workerCtx. Deriving the worker's context
// from ctx killed spawned workers instantly on the spawn_worker path:
// net/http cancels the MCP request context the moment the tool call
// returns, the harness's run timeout inherited the cancellation, and the
// freshly started opencode child was killed while the dispatch had already
// returned {job_id} - a silent fake success (2026-11-15 bug report).
func (a *App) SpawnWorker(ctx context.Context, spec wire.JobSpec) error {
	bearer, err := a.auth.MintWorkerToken(ctx, a.inboxID, spec.ID)
	if err != nil {
		return fmt.Errorf("mint worker token for %s: %w", spec.ID, err)
	}
	jobDir := filepath.Join(a.dataDir, jobsDirName, spec.ID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return fmt.Errorf("job dir for %s: %w", spec.ID, err)
	}
	specPath := filepath.Join(jobDir, "job.json")
	data, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal spec for %s: %w", spec.ID, err)
	}
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		return fmt.Errorf("write spec for %s: %w", spec.ID, err)
	}

	hcfg := a.BuildHarnessConfig(spec, bearer)
	hcfg.Cancel = a.registerCancel(spec.ID)
	log.Printf("foremanapp: spawning worker for %s (model %q, runner %s)", spec.ID, hcfg.Model, workerharness.RunnerFor(hcfg.Model))

	// Child-spawn state (spec §9.2, 2026-11-15 sub-spawning amendment):
	// recorded before the events and the harness goroutine, so the
	// consumer's copy of the child's terminal job.result into the parent's
	// per-job inbox can never race the spawn.
	a.recordSpawnState(spec)
	a.markSpawned(spec.ID)
	a.setWorkerLive(spec.ID, true)

	// Registry visibility (spec §6.2, 2026-11-15 spawn-visibility
	// amendment): foreman-local spawns mint their JobSpec here, so the
	// factory never created a registry row for them - brain-spawned jobs
	// existed only in this foreman's local jobs/ store and their results
	// were ignored there as unknown. The accepted event therefore carries
	// the full JobSpec (the factory registers insert-if-absently), and
	// both events post BEFORE the harness goroutine starts: the row always
	// exists (dispatched -> running) before any worker report can arrive,
	// so the loud §8.5 failure path applies at the factory like any worker
	// report instead of being dropped as unknown. The posts stay
	// synchronous in the spawn call exactly as before - spawn never waits
	// on the worker, and event failures are logged, never fatal.
	for _, event := range []string{"accepted", "started"} {
		if err := a.postJobEvent(ctx, spec, event); err != nil {
			log.Printf("foremanapp: job.event %s for %s: %v", event, spec.ID, err)
		}
	}
	// Live-work count for the auto-update drain (spec §12.4): registered
	// before the goroutine starts, released when the harness returns.
	a.gate.workerStarted()
	go a.runWorker(spec, hcfg)
	return nil
}

// runWorker runs the harness for one spawned job under a.workerCtx (never
// the spawner's request context - see SpawnWorker).
//
// It is LOUD on failure: when the harness returns an error while the foreman
// is still up, a failed job.result is posted into the foreman's own inbox
// (spec §8.5 fallback analog), which the router forwards to the factory
// byte-identically - the factory transitions the running job to failed with
// the spawn error (spec §6.2) and the retry policy re-arms it (spec §6.4).
// A worker that dies instantly therefore surfaces as a visible job failure,
// never a silent fake success. The factory applies results
// terminal-idempotently, so a synthesized report after one the harness
// itself delivered is ignored as a duplicate. No report is synthesized when
// the foreman itself is stopping (a.workerCtx done): the App is going away.
func (a *App) runWorker(spec wire.JobSpec, hcfg workerharness.Config) {
	defer a.gate.workerDone()
	// Revoke after the harness has posted everything (result fallback and
	// post-exit usage). Also covers a result forwarded while live (the
	// consumer skipped its revoke) and harness failures.
	defer func() {
		a.setWorkerLive(spec.ID, false)
		a.clearCancel(spec.ID)
		a.revokeJobCredential(context.Background(), spec.ID)
	}()
	// Credentials (spec §13.2): fetched before the child starts; a job
	// promised secrets never runs without them. Values stay in hcfg
	// (process memory) and are dropped when this function returns; Go
	// strings are not zeroed - best effort only (spec §13.2).
	select {
	case <-hcfg.Cancel:
		log.Printf("foremanapp: worker %s: cancelled before start", spec.ID)
		return
	default:
	}
	if len(spec.Secrets) > 0 {
		env, err := a.fetchJobSecrets(a.workerCtx, spec)
		if err != nil {
			if a.workerCtx.Err() != nil {
				log.Printf("foremanapp: worker %s: secrets fetch aborted (foreman stopping)", spec.ID)
				return
			}
			log.Printf("foremanapp: worker %s: %v", spec.ID, err)
			a.postSpawnFailure(spec, err)
			return
		}
		hcfg.SecretEnv = env
		defer func() { hcfg.SecretEnv = nil }()
	}
	if err := workerharness.Run(a.workerCtx, hcfg); err != nil {
		if errors.Is(err, workerharness.ErrCancelled) {
			// Cancelled/superseded at the factory: stopped on purpose, so
			// no failed result (it would only be ignored, never retried).
			log.Printf("foremanapp: worker %s: stopped by job.cancel", spec.ID)
			return
		}
		if a.workerCtx.Err() != nil {
			log.Printf("foremanapp: worker %s: %v (foreman stopping)", spec.ID, err)
			return
		}
		log.Printf("foremanapp: worker %s: %v", spec.ID, err)
		a.postSpawnFailure(spec, err)
		return
	}
	log.Printf("foremanapp: worker %s: finished", spec.ID)
}

// noOutputSummary is the fallback summary when there is nothing to report
// (spec §8.5, §11 - the workerharness pinned twin).
const noOutputSummary = "(no output)"

// postSpawnFailure posts a failed job.result for spec into the foreman's
// own inbox as the worker identity ("worker:{job_id}", the §3.2 sender
// shape the harness fallback uses): the local loop forwards it to the
// factory's main inbox, where it is applied like any worker report.
func (a *App) postSpawnFailure(spec wire.JobSpec, cause error) {
	res := wire.JobResult{
		JobID:   spec.ID,
		Status:  wire.StatusFailed,
		Summary: noOutputSummary,
		Error: &wire.JobError{
			Kind:    wire.KindOther,
			Message: fmt.Sprintf("spawn failure for %s: %v", spec.ID, cause),
		},
	}
	if err := res.Validate(spec.ResultSchema); err != nil {
		log.Printf("foremanapp: spawn failure report for %s: invalid: %v", spec.ID, err)
		return
	}
	payload, err := json.Marshal(res)
	if err != nil {
		log.Printf("foremanapp: spawn failure report for %s: %v", spec.ID, err)
		return
	}
	env := wire.MessageEnvelope{
		Type:    "job.result",
		JobID:   spec.ID,
		From:    "worker:" + spec.ID,
		Payload: payload,
	}
	if _, err := a.inbox.Post(a.workerCtx, a.inboxID, env); err != nil {
		log.Printf("foremanapp: spawn failure report for %s: %v", spec.ID, err)
	}
}

// postJobEvent posts a job.event envelope {job_id, event} to the factory's
// main inbox with the paired bearer (spec §6.2). The accepted event also
// carries the full JobSpec (2026-11-15 spawn-visibility amendment): the
// factory's insert-if-absent registration (jobs.RegisterSpawned) makes a
// foreman-local spawn visible in the job registry. started carries no
// spec: the row it transitions must already exist (created by the factory
// at dispatch, or by the accepted event's registration).
func (a *App) postJobEvent(ctx context.Context, spec wire.JobSpec, event string) error {
	p := struct {
		Event string        `json:"event"`
		Spec  *wire.JobSpec `json:"spec,omitempty"`
	}{Event: event}
	if event == "accepted" {
		s := spec
		p.Spec = &s
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	env := wire.MessageEnvelope{
		Type:    "job.event",
		JobID:   spec.ID,
		From:    a.inboxID,
		Payload: payload,
	}
	return a.poster.PostToParent(ctx, env)
}

// opencodeUsageReader is the worker runs' opencode usage capture: the
// host's opencode database (opencode prints no usage; the claude route
// parses stdout). A package var so tests never exec a real opencode.
var opencodeUsageReader ocusage.Reader = ocusage.ReaderFor("")

// defaultHarnessConfig builds the harness config for one job (spec §8.1):
// the persisted spec path, the loopback ResultURL and MCPURL (see
// localBaseURL - workers are local in-process children), the per-job token,
// DataDir as the per-job root, and the model: the job's explicit `model`
// when set (spec §5.1, 2026-11-15 amendment - it overrides the tier),
// else the settings-synced tier mapping (spec §6.5, settings.go; panel >
// env > pinned default). Defaults: flash (and no-tier jobs - the cost-biased default) run GLM 5.3
// Flash, `tier: "default"` runs GLM 5.3 Flash at max reasoning effort,
// `tier: "hardest"` runs GLM 5.3. The model string also routes the
// runner (§8.4): a claude-* override (e.g.
// FACTORY_WORKER_HARDEST_MODEL=claude-opus-4-6) runs that tier via the
// Claude Code CLI; the pinned GLM defaults are unchanged.
func (a *App) defaultHarnessConfig(spec wire.JobSpec, bearer string) workerharness.Config {
	base := localBaseURL(a.cfg.Addr)
	model := a.workerModelForTier(spec.Tier)
	if spec.Model != "" {
		model = spec.Model
	}
	return workerharness.Config{
		JobPath:   filepath.Join(a.dataDir, jobsDirName, spec.ID, "job.json"),
		ResultURL: base + "/v1/inboxes/" + url.PathEscape(a.inboxID) + "/messages",
		MCPURL:    base + "/v1/mcp/worker",
		Token:     bearer,
		WorkDir:   a.dataDir,
		Model:     model,
		// opencode runs print no usage: read it back from the host's
		// opencode database (the claude route parses stdout instead).
		ReadUsage: opencodeUsageReader,
	}
}

// workerModelForTier resolves the tier's worker model from the
// settings-synced modelCfg (set by New). An App built without New (bare
// &App{} in tests) has no modelCfg; it falls back to the static cfg
// models - the pre-settings behaviour.
func (a *App) workerModelForTier(tier string) string {
	if a.modelCfg != nil {
		return a.modelCfg.WorkerForTier(tier)
	}
	switch tier {
	case wire.TierDefault:
		return a.cfg.WorkerModel
	case wire.TierHardest:
		return a.cfg.WorkerHardestModel
	}
	return a.cfg.WorkerFlashModel
}
