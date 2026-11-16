package workerharness

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

func noUsage(context.Context, string, string, wire.UsageEvent) error { return nil }

// runLimits runs one fake child under the given limits and returns the
// posted fallback result and wall time.
func runLimits(t *testing.T, claude bool, body string, mutate func(*Config, *wire.JobSpec)) (wire.JobResult, time.Duration) {
	t.Helper()
	dir := t.TempDir()
	var spec wire.JobSpec
	_, specPath := writeSpec(t, dir, func(s *wire.JobSpec) { spec = *s })
	rec := &recordingPost{}
	var cfg Config
	if claude {
		binDir := fakeClaude(t, body)
		cfg = testClaudeConfig(t, specPath, binDir, rec.post)
		cfg.Model = "claude-sonnet-5-5"
	} else {
		cfg = testConfig(t, specPath, fakeOpencode(t, body), rec.post)
	}
	cfg.PostUsage = noUsage
	cfg.TermGrace = 2 * time.Second
	if mutate != nil {
		mutate(&cfg, &spec)
	}
	start := time.Now()
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	elapsed := time.Since(start)
	if rec.calls() != 1 {
		t.Fatalf("fallback posts = %d, want 1", rec.calls())
	}
	res := rec.last()
	if err := res.Validate(nil); err != nil {
		t.Errorf("result invalid: %v", err)
	}
	return res, elapsed
}

func TestIdleKill(t *testing.T) {
	t.Parallel()
	res, elapsed := runLimits(t, false, "printf 'started'\nsleep 30", func(c *Config, _ *wire.JobSpec) {
		c.IdleTimeout = 400 * time.Millisecond
		c.Timeout = time.Minute
	})
	if res.Error == nil || res.Error.Reason != wire.ReasonIdle || res.Error.Kind != wire.KindTimeout {
		t.Fatalf("error = %+v, want timeout/idle", res.Error)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v, want idle kill near 400ms", elapsed)
	}
	if res.Summary != "started" {
		t.Errorf("summary = %q, want partial output kept", res.Summary)
	}
}

func TestCeilingKillDespiteHeartbeats(t *testing.T) {
	t.Parallel()
	// Chatty child: output every 50ms forever, so idle never fires.
	res, elapsed := runLimits(t, false, "while true; do printf x; sleep 0.05; done", func(c *Config, _ *wire.JobSpec) {
		c.IdleTimeout = 500 * time.Millisecond
		c.Timeout = time.Second
	})
	if res.Error == nil || res.Error.Reason != wire.ReasonCeiling || res.Error.Kind != wire.KindTimeout {
		t.Fatalf("error = %+v, want timeout/ceiling", res.Error)
	}
	if elapsed < time.Second || elapsed > 5*time.Second {
		t.Errorf("took %v, want ~1s ceiling", elapsed)
	}
}

func TestHeartbeatResetsIdleTimer(t *testing.T) {
	t.Parallel()
	// 6 x 100ms of output gaps = 600ms total, idle limit 300ms: only
	// survives if each write resets the timer. Stderr counts too.
	res, _ := runLimits(t, false, "for i in 1 2 3; do printf out; sleep 0.1; printf err >&2; sleep 0.1; done\nprintf done", func(c *Config, _ *wire.JobSpec) {
		c.IdleTimeout = 300 * time.Millisecond
		c.Timeout = time.Minute
	})
	if res.Status != wire.StatusDone {
		t.Fatalf("result = %+v, want done (heartbeats kept the job alive)", res)
	}
}

func TestBudgetKillKeepsPartialOutput(t *testing.T) {
	t.Parallel()
	// Sonnet: 1M output tokens = $15. Each event is $3 (200k out); the
	// cap is $5 so the second event (cumulative $6) trips it.
	ev := func(id string) string {
		return `{"type":"assistant","message":{"id":"` + id + `","model":"claude-sonnet-5-5","usage":{"input_tokens":0,"output_tokens":200000},"content":[{"type":"text","text":"hi"}]}}`
	}
	body := "echo '" + ev("m1") + "'\necho '" + ev("m1") + "'\nsleep 0.3\necho '" + ev("m2") + "'\nsleep 30"
	res, elapsed := runLimits(t, true, body, func(c *Config, s *wire.JobSpec) {
		c.IdleTimeout = time.Minute
		c.Timeout = time.Minute
	})
	if res.Error == nil || res.Error.Reason != wire.ReasonBudget {
		t.Fatalf("error = %+v, want budget", res.Error)
	}
	if elapsed > 8*time.Second {
		t.Errorf("took %v", elapsed)
	}
	if !strings.Contains(res.Summary, "spend cap") || !strings.Contains(res.Summary, "$6.00") {
		t.Errorf("summary = %q, want clear spend-cap summary with cost (m1 duplicate priced once)", res.Summary)
	}
	if res.Usage == nil || res.Usage.CostUSD < 5 {
		t.Errorf("usage = %+v, want accumulated cost", res.Usage)
	}
}

func TestBudgetNoUsageDataFallsBackToIdleCeiling(t *testing.T) {
	t.Parallel()
	// opencode path emits no usage events: a tiny max_cost never fires,
	// only the idle limit does.
	res, _ := runLimits(t, false, "printf 'x'\nsleep 30", func(c *Config, _ *wire.JobSpec) {
		c.IdleTimeout = 300 * time.Millisecond
		c.Timeout = time.Minute
	})
	if res.Error == nil || res.Error.Reason != wire.ReasonIdle {
		t.Fatalf("error = %+v, want idle", res.Error)
	}
}

func TestLoopGuard(t *testing.T) {
	t.Parallel()
	same := claudeLine("a", `{"command":"ls"}`)
	body := ""
	for i := 0; i < 5; i++ {
		body += "echo '" + strings.Replace(same, "ID", string(rune('a'+i)), 1) + "'\n"
	}
	body += "sleep 30"
	res, _ := runLimits(t, true, body, func(c *Config, _ *wire.JobSpec) {
		c.LoopLimit = 5
		c.IdleTimeout = time.Minute
		c.Timeout = time.Minute
	})
	if res.Error == nil || res.Error.Reason != wire.ReasonLoop {
		t.Fatalf("error = %+v, want loop", res.Error)
	}
}

func claudeLine(_ string, input string) string {
	return `{"type":"assistant","message":{"id":"mID","model":"claude-sonnet-5-5","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"tool_use","id":"tuID","name":"Bash","input":` + input + `}]}}`
}
