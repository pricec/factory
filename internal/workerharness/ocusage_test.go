package workerharness

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pricec/factory/internal/ocusage"
	"github.com/pricec/factory/internal/wire"
)

// An opencode run prints no usage: the harness reads the sessions opencode
// recorded for the job dir since the child started and posts each as a
// job.usage event, on both the fallback and the agent-reported paths,
// before Run returns (the per-job bearer is revoked on return).
func TestRunOpencodeRecordsSessionUsage(t *testing.T) {
	t.Parallel()
	for _, agentReported := range []bool{false, true} {
		dir := t.TempDir()
		spec, specPath := writeSpec(t, dir, func(s *wire.JobSpec) { s.Tier = wire.TierHardest })
		body := "printf 'done'"
		if agentReported {
			body = "touch .result_posted\n" + body
		}
		rec := &recordingPost{}
		cfg := testConfig(t, specPath, fakeOpencode(t, body), rec.post)
		cfg.Model = "openrouter/z-ai/glm-5.3"
		before := time.Now().UnixMilli()
		var readDir string
		var readSince int64
		cfg.ReadUsage = func(dir string, sinceMs int64) ([]ocusage.Session, error) {
			readDir, readSince = dir, sinceMs
			return []ocusage.Session{
				{ID: "ses_1", Model: "openrouter/z-ai/glm-5.3#max", InputTokens: 7, OutputTokens: 3, ReasoningTokens: 2, CacheReadTokens: 13, CacheWriteTokens: 11, CostUSD: 0.25},
				{ID: "ses_2", InputTokens: 1, CostUSD: 0.01},
			}, nil
		}
		var got []wire.UsageEvent
		cfg.PostUsage = func(_ context.Context, _, _ string, ev wire.UsageEvent) error {
			got = append(got, ev)
			return nil
		}
		if err := Run(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if readDir != filepath.Join(cfg.WorkDir, jobsDirName, spec.ID) || readSince < before {
			t.Fatalf("read dir %q since %d, want the job dir since >= %d", readDir, readSince, before)
		}
		if len(got) != 2 {
			t.Fatalf("agentReported=%v usage events = %+v, want 2", agentReported, got)
		}
		a := got[0]
		if a.Kind != wire.UsageKindWorker || a.JobID != spec.ID || a.Tier != wire.TierHardest || a.SessionID != "ses_1" ||
			a.Model != "openrouter/z-ai/glm-5.3#max" || a.Input != 7 || a.Output != 5 || a.CacheCreate != 11 ||
			a.CacheRead != 13 || a.CostUSD != 0.25 || a.DurationMs < 0 {
			t.Fatalf("event = %+v", a)
		}
		if got[1].Model != cfg.Model || got[1].SessionID != "ses_2" {
			t.Fatalf("model fallback: %+v", got[1])
		}
	}
}

// Read failures and an unwired reader record nothing and never fail the run.
func TestRunOpencodeUsageReadBestEffort(t *testing.T) {
	t.Parallel()
	for name, read := range map[string]ocusage.Reader{
		"nil reader": nil,
		"read error": func(string, int64) ([]ocusage.Session, error) { return nil, errors.New("no db") },
	} {
		_, specPath := writeSpec(t, t.TempDir(), nil)
		rec := &recordingPost{}
		cfg := testConfig(t, specPath, fakeOpencode(t, "printf 'done'"), rec.post)
		cfg.ReadUsage = read
		posted := 0
		cfg.PostUsage = func(context.Context, string, string, wire.UsageEvent) error { posted++; return nil }
		if err := Run(context.Background(), cfg); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if posted != 0 || rec.calls() != 1 || rec.last().Status != wire.StatusDone {
			t.Fatalf("%s: usage posts %d, results %+v", name, posted, rec.results)
		}
	}
}
