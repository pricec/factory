package foremanapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"

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
	log.Printf("foremanapp: spawning worker for %s (model %q)", spec.ID, hcfg.Model)
	go func() {
		if err := workerharness.Run(ctx, hcfg); err != nil {
			log.Printf("foremanapp: worker %s: %v", spec.ID, err)
		} else {
			log.Printf("foremanapp: worker %s: finished", spec.ID)
		}
	}()

	for _, event := range []string{"accepted", "started"} {
		if err := a.postJobEvent(ctx, spec.ID, event); err != nil {
			log.Printf("foremanapp: job.event %s for %s: %v", event, spec.ID, err)
		}
	}
	return nil
}

// postJobEvent posts a job.event envelope {job_id, event} to the factory's
// main inbox with the paired bearer (spec §6.2).
func (a *App) postJobEvent(ctx context.Context, jobID, event string) error {
	payload, err := json.Marshal(map[string]string{"event": event})
	if err != nil {
		return err
	}
	env := wire.MessageEnvelope{
		Type:    "job.event",
		JobID:   jobID,
		From:    a.inboxID,
		Payload: payload,
	}
	return a.poster.PostToParent(ctx, env)
}

// defaultHarnessConfig builds the harness config for one job (spec §8.1):
// the persisted spec path, the loopback ResultURL and MCPURL (see
// localBaseURL - workers are local in-process children), the per-job token,
// DataDir as the per-job root, and the model from the job's tier (spec
// §6.5): flash (and no-tier jobs - the cost-biased default) run GLM 5.3
// Flash, `tier: "default"` runs GLM 5.2, `tier: "hardest"` runs GLM 5.3.
func (a *App) defaultHarnessConfig(spec wire.JobSpec, bearer string) workerharness.Config {
	base := localBaseURL(a.cfg.Addr)
	model := a.cfg.WorkerFlashModel
	switch spec.Tier {
	case wire.TierDefault:
		model = a.cfg.WorkerModel
	case wire.TierHardest:
		model = a.cfg.WorkerHardestModel
	}
	return workerharness.Config{
		JobPath:   filepath.Join(a.dataDir, jobsDirName, spec.ID, "job.json"),
		ResultURL: base + "/v1/inboxes/" + url.PathEscape(a.inboxID) + "/messages",
		MCPURL:    base + "/v1/mcp/worker",
		Token:     bearer,
		WorkDir:   a.dataDir,
		Model:     model,
	}
}
