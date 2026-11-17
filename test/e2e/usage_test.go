package e2e_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/ocusage"
	"github.com/pricec/factory/internal/wire"
)

// TestOpencodeUsageE2E: an opencode worker run (which prints no usage)
// has its sessions read back after the child exits and recorded through
// the same job.usage -> usage_events path as claude runs: the per-run log
// (GET /v1/usage/events), the job's usage rollup, and the per-kind
// breakdown of /v1/usage all see it.
func TestOpencodeUsageE2E(t *testing.T) {
	s := boot(t)
	reads := make(chan string, 4)
	s.readOpencodeUsage = func(dir string, sinceMs int64) ([]ocusage.Session, error) {
		reads <- filepath.Base(dir)
		if sinceMs <= 0 {
			t.Errorf("sinceMs = %d, want the child start time", sinceMs)
		}
		return []ocusage.Session{
			{ID: "ses_a", Model: "openrouter/z-ai/glm-5.3-flash", InputTokens: 100, OutputTokens: 10, ReasoningTokens: 5, CacheReadTokens: 7, CacheWriteTokens: 3, CostUSD: 0.25},
			{ID: "ses_b", InputTokens: 20, OutputTokens: 2, CostUSD: 0.5},
		}, nil
	}

	sc := s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "create_job", map[string]any{"task": "e2e opencode usage"})
	jobID, _ := sc["job_id"].(string)
	var job jobs.Job
	waitFor(t, "usage job done with rollup", 10*time.Second, func() bool {
		_ = s.do("GET", "/v1/jobs/"+jobID, "", nil, &job)
		return job.State == jobs.StateDone && len(job.Usage) > 0
	})
	if dir := <-reads; dir != jobID {
		t.Errorf("usage read for dir %q, want the job dir %q", dir, jobID)
	}

	var u wire.Usage
	if err := json.Unmarshal(job.Usage, &u); err != nil {
		t.Fatal(err)
	}
	// Rollup: input + cache_create + cache_read; output includes reasoning.
	if u.InputTokens != 130 || u.OutputTokens != 17 || u.CostUSD != 0.75 {
		t.Errorf("job usage = %+v, want in 130 out 17 cost 0.75", u)
	}

	var evs []jobs.UsageRecord
	if st := s.do("GET", "/v1/usage/events?kind=worker", "", nil, &evs); st != 200 {
		t.Fatalf("GET /v1/usage/events = %d", st)
	}
	if len(evs) != 2 {
		t.Fatalf("usage events = %+v, want 2", evs)
	}
	byID := map[string]jobs.UsageRecord{}
	for _, e := range evs {
		byID[e.SessionID] = e
		if e.JobID != jobID || e.Kind != wire.UsageKindWorker || e.ForemanID == "" {
			t.Errorf("event %+v: want worker event for %s with a foreman id", e, jobID)
		}
	}
	if a := byID["ses_a"]; a.Model != "openrouter/z-ai/glm-5.3-flash" || a.Output != 15 || a.CacheCreate != 3 || a.CacheRead != 7 {
		t.Errorf("ses_a = %+v", a)
	}
	if b := byID["ses_b"]; b.Model == "" {
		t.Errorf("ses_b model empty, want the tier model fallback")
	}

	var usage struct {
		Spend map[string]jobs.UsageSums `json:"factory_spend"`
	}
	if st := s.do("GET", "/v1/usage", "", nil, &usage); st != 200 {
		t.Fatalf("GET /v1/usage = %d", st)
	}
	if w := usage.Spend["today"].ByKind[wire.UsageKindWorker]; w.Runs != 2 || w.CostUSD != 0.75 {
		t.Errorf("today worker breakdown = %+v, want 2 runs $0.75", w)
	}
}
