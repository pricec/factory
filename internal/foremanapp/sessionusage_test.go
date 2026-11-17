package foremanapp_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pricec/factory/internal/ocusage"
	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

// TestWorkerSessionUsageReported drives the full session-accounting
// loop (spec §3.2, §6.5 reporting amendment): a spawned worker's
// harness reads the sessions opencode recorded for the job directory,
// the spawner posts one session.usage envelope per session to the
// factory, the factory's consumer records the ledger row and
// backfills the job's still-empty usage - so the factory sees worker
// spend even when the agent never self-reported (last night's live
// gap: 17 jobs, $0 recorded).
func TestWorkerSessionUsageReported(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	forEnv := newForeman(t, fac, token)

	// A job in the factory's store, driven to running so the fallback
	// result applies cleanly.
	spec := wire.NewJobSpec("fake work", "factory")
	if err := fac.a.Jobs().Create(fac.ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := fac.a.Jobs().Dispatch(fac.ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if err := fac.a.Jobs().ApplyEvent(fac.ctx, spec.ID, "started"); err != nil {
		t.Fatalf("ApplyEvent: %v", err)
	}

	fakeBin := fakeOpencode(t, `printf 'done fast'`)
	orig := forEnv.a.BuildHarnessConfig
	forEnv.a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		c := orig(spec, bearer)
		c.OpenCODEBin = fakeBin
		c.ResultURL = forEnv.ts.URL + "/v1/inboxes/foreman:alpha/messages"
		c.MCPURL = forEnv.ts.URL + "/v1/mcp/worker"
		c.Timeout = 15 * time.Second
		c.PostRetryBackoff = 10 * time.Millisecond
		c.ReadUsage = func(dir string, sinceMs int64) ([]ocusage.Session, error) {
			return []ocusage.Session{{
				ID: "ses_worker", Directory: dir,
				Model:       "openrouter/z-ai/glm-5.3-flash",
				InputTokens: 578107, OutputTokens: 29891,
				ReasoningTokens: 37653, CostUSD: 0.3937,
			}}, nil
		}
		return c
	}

	if err := forEnv.a.SpawnWorker(forEnv.ctx, spec); err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	// The ledger row lands (the usage hook posts asynchronously, the
	// factory's consumer records it).
	waitFor(t, "session ledger row", 8*time.Second, func() bool {
		rows, err := fac.a.Jobs().ListSessions(fac.ctx, "", wire.SessionRoleWorker, 0)
		return err == nil && len(rows) == 1
	})
	rows, err := fac.a.Jobs().ListSessions(fac.ctx, "", wire.SessionRoleWorker, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := rows[0]
	if got.ID != "ses_worker" || got.JobID != spec.ID || got.ForemanID != "alpha" {
		t.Errorf("ledger row = %+v, want ses_worker for %s on alpha", got, spec.ID)
	}
	if got.CostUSD != 0.3937 || got.InputTokens != 578107 || got.ReasoningTokens != 37653 {
		t.Errorf("ledger accounting = %+v, want the recorded session's numbers", got)
	}

	// The job's usage backfilled from the ledger row.
	waitFor(t, "job usage backfill", 8*time.Second, func() bool {
		job, ok, err := fac.a.Jobs().Get(fac.ctx, spec.ID)
		if err != nil || !ok {
			return false
		}
		var u wire.Usage
		return json.Unmarshal(job.Usage, &u) == nil && u.CostUSD == 0.3937
	})
	job, ok, err := fac.a.Jobs().Get(fac.ctx, spec.ID)
	if err != nil || !ok {
		t.Fatalf("Get %s: %v (ok %v)", spec.ID, err, ok)
	}
	var usage wire.Usage
	if err := json.Unmarshal(job.Usage, &usage); err != nil {
		t.Fatalf("job usage %s: %v", job.Usage, err)
	}
	if usage.CostUSD != 0.3937 || usage.InputTokens != 578107 {
		t.Errorf("job usage = %+v, want the backfilled session numbers", usage)
	}
}
