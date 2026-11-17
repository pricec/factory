package workerharness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pricec/factory/internal/ocusage"
	"github.com/pricec/factory/internal/wire"
)

// usageRecorder captures OnUsage invocations (the hook is invoked on a
// background goroutine; the test synchronizes on the channel).
type usageRecorder struct {
	ch chan []ocusage.Session
}

func newUsageRecorder() *usageRecorder { return &usageRecorder{ch: make(chan []ocusage.Session, 1)} }

func (u *usageRecorder) onUsage(_ wire.JobSpec, sessions []ocusage.Session, _ int64) {
	u.ch <- sessions
}

// testSessions is a fake session-accounting read: an older session then
// the agent's own (newest) run.
func testSessions() []ocusage.Session {
	return []ocusage.Session{
		{
			ID: "ses_sub", Directory: "/jobs/j1", Model: "openrouter/z-ai/glm-5.3-flash",
			InputTokens: 1000, OutputTokens: 100, CostUSD: 0.01, CreatedMs: 100,
		},
		{
			ID: "ses_main", Directory: "/jobs/j1", Model: "openrouter/z-ai/glm-5.3-flash#max",
			InputTokens: 5000, OutputTokens: 250, ReasoningTokens: 42,
			CacheReadTokens: 500, CostUSD: 0.17, CreatedMs: 200,
		},
	}
}

// TestRunUsageHookFiresOnBothPaths: the session-accounting read (spec
// §6.5 reporting amendment) feeds the OnUsage hook whenever sessions
// were recorded - after an agent-reported run AND after a fallback
// result - because a session that produced no result still spent
// tokens. The hook is asynchronous: accounting never adds latency to
// the run path.
func TestRunUsageHookFiresOnBothPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// body is the fake opencode behavior: report the result via the
		// flag file, or fall back (exit 4 posts a crash result).
		body     string
		readFunc func(dir string, sinceMs int64) ([]ocusage.Session, error)
		wantHook bool
	}{
		{
			name:     "agent-reported run",
			body:     `touch .result_posted; printf 'reported'`,
			readFunc: func(string, int64) ([]ocusage.Session, error) { return testSessions(), nil },
			wantHook: true,
		},
		{
			name:     "fallback (crash) run",
			body:     `printf 'halfway there'; exit 4`,
			readFunc: func(string, int64) ([]ocusage.Session, error) { return testSessions(), nil },
			wantHook: true,
		},
		{
			name:     "read error degrades to no hook",
			body:     `printf 'boom'; exit 4`,
			readFunc: func(string, int64) ([]ocusage.Session, error) { return nil, errors.New("no db") },
			wantHook: false,
		},
		{
			name:     "no sessions recorded",
			body:     `printf 'nothing'; exit 4`,
			readFunc: func(string, int64) ([]ocusage.Session, error) { return nil, nil },
			wantHook: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			_, specPath := writeSpec(t, dir, nil)
			binDir := fakeOpencode(t, tc.body)
			rec := &recordingPost{}
			usage := newUsageRecorder()
			cfg := testConfig(t, specPath, binDir, rec.post)
			cfg.ReadUsage = tc.readFunc
			cfg.OnUsage = usage.onUsage

			if err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}

			select {
			case sessions := <-usage.ch:
				if !tc.wantHook {
					t.Fatal("OnUsage fired, want it silent")
				}
				if len(sessions) != 2 {
					t.Errorf("OnUsage sessions = %d, want 2", len(sessions))
				}
			case <-time.After(2 * time.Second):
				if tc.wantHook {
					t.Fatal("OnUsage did not fire within 2s")
				}
			}
		})
	}
}

// TestRunFallbackResultCarriesNoUsage: the fallback result never
// carries harness-measured usage (spec §6.5 reporting amendment): the
// measured accounting reaches the factory as a session.usage envelope,
// whose ledger row backfills the job there - one accounting source,
// never two.
func TestRunFallbackResultCarriesNoUsage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, specPath := writeSpec(t, dir, nil)
	binDir := fakeOpencode(t, `printf 'halfway there'; exit 4`)
	rec := &recordingPost{}
	cfg := testConfig(t, specPath, binDir, rec.post)
	cfg.ReadUsage = func(string, int64) ([]ocusage.Session, error) { return testSessions(), nil }
	cfg.OnUsage = newUsageRecorder().onUsage

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if rec.calls() != 1 {
		t.Fatalf("fallback posts = %d, want 1", rec.calls())
	}
	if got := rec.last(); got.Usage != nil {
		t.Errorf("fallback result usage = %+v, want nil (the ledger backfills, not the harness)", got.Usage)
	}
}
