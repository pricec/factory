package workerharness

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

func noUsage(context.Context, string, string, wire.UsageEvent) error { return nil }

// runLimits runs one fake child under the given limits and returns the
// posted fallback result and wall time.
func runLimits(t *testing.T, claude bool, body string, mutate func(*Config, *wire.JobSpec)) (wire.JobResult, time.Duration) {
	return runLimitsSpec(t, claude, body, nil, mutate)
}

// runLimitsSpec is runLimits with a mutator applied to the persisted spec
// (job.json) before the run, e.g. to set budget or loop_limit.
func runLimitsSpec(t *testing.T, claude bool, body string, specMut func(*wire.JobSpec), mutate func(*Config, *wire.JobSpec)) (wire.JobResult, time.Duration) {
	t.Helper()
	dir := t.TempDir()
	var spec wire.JobSpec
	_, specPath := writeSpec(t, dir, func(s *wire.JobSpec) {
		if specMut != nil {
			specMut(s)
		}
		spec = *s
	})
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
	// Sonnet 5.5: 1M output tokens = $10. Each event is $2 (200k out); the
	// cap is $3 so the second event (cumulative $4) trips it.
	ev := func(id string) string {
		return `{"type":"assistant","message":{"id":"` + id + `","model":"claude-sonnet-5-5","usage":{"input_tokens":0,"output_tokens":200000},"content":[{"type":"text","text":"hi"}]}}`
	}
	body := "echo '" + ev("m1") + "'\necho '" + ev("m1") + "'\nsleep 0.3\necho '" + ev("m2") + "'\nsleep 30"
	res, elapsed := runLimitsSpec(t, true, body, func(s *wire.JobSpec) { s.Budget.MaxCostUSD = 3 }, func(c *Config, s *wire.JobSpec) {
		c.IdleTimeout = time.Minute
		c.Timeout = time.Minute
	})
	if res.Error == nil || res.Error.Reason != wire.ReasonBudget {
		t.Fatalf("error = %+v, want budget", res.Error)
	}
	if elapsed > 8*time.Second {
		t.Errorf("took %v", elapsed)
	}
	if !strings.Contains(res.Summary, "spend cap") || !strings.Contains(res.Summary, "$4.00") {
		t.Errorf("summary = %q, want clear spend-cap summary with cost (m1 duplicate priced once)", res.Summary)
	}
	if res.Usage == nil || res.Usage.CostUSD < 3 {
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

// toolCall returns the shell lines for one claude stream-json tool call:
// the assistant tool_use and the user tool_result for it.
func toolCall(id, input, result string) string {
	return "echo '" + `{"type":"assistant","message":{"id":"m` + id + `","model":"claude-sonnet-5-5","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"tool_use","id":"tu` + id + `","name":"Bash","input":` + input + `}]}}` + "'\n" +
		"echo '" + `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu` + id + `","content":"` + result + `"}]}}` + "'\n"
}

func loopBody(n int, input string, result func(i int) string, tail string) string {
	body := ""
	for i := 0; i < n; i++ {
		body += toolCall(string(rune('a'+i)), input, result(i))
	}
	return body + tail
}

func TestLoopGuardKillsIdenticalLoop(t *testing.T) {
	t.Parallel()
	body := loopBody(6, `{"command":"ls"}`, func(int) string { return "same" }, "sleep 30")
	res, _ := runLimits(t, true, body, func(c *Config, _ *wire.JobSpec) {
		c.LoopLimit = 5
		c.IdleTimeout = time.Minute
		c.Timeout = time.Minute
	})
	if res.Error == nil || res.Error.Reason != wire.ReasonLoop {
		t.Fatalf("error = %+v, want loop", res.Error)
	}
}

func TestLoopGuardPollingWithChangingResultsNotKilled(t *testing.T) {
	t.Parallel()
	// Same tool+input 10 times (polling `make ci`) but the output keeps
	// changing: not a loop even with limit 3.
	body := loopBody(10, `{"command":"make ci"}`, func(i int) string { return "progress " + string(rune('a'+i)) }, "sleep 0.5\nprintf finished")
	res, _ := runLimits(t, true, body, func(c *Config, _ *wire.JobSpec) {
		c.LoopLimit = 3
		c.IdleTimeout = time.Minute
		c.Timeout = time.Minute
	})
	if res.Error != nil || res.Status != wire.StatusDone {
		t.Fatalf("result = %+v, want done (changing results are not a loop)", res)
	}
}

func TestLoopGuardJobSpecOverride(t *testing.T) {
	t.Parallel()
	same := func(int) string { return "same" }
	// loop_limit 3 kills an identical loop well below the default 8.
	res, _ := runLimitsSpec(t, true, loopBody(4, `{"command":"ls"}`, same, "sleep 30"),
		func(s *wire.JobSpec) { s.LoopLimit = 3 }, func(c *Config, _ *wire.JobSpec) {
			c.IdleTimeout = time.Minute
			c.Timeout = time.Minute
		})
	if res.Error == nil || res.Error.Reason != wire.ReasonLoop {
		t.Fatalf("loop_limit=3: error = %+v, want loop", res.Error)
	}
	// Without the override, 4 identical calls are under the default 8.
	res, _ = runLimits(t, true, loopBody(4, `{"command":"ls"}`, same, "sleep 0.5\nprintf finished"),
		func(c *Config, _ *wire.JobSpec) { c.IdleTimeout = time.Minute; c.Timeout = time.Minute })
	if res.Error != nil {
		t.Fatalf("default limit: error = %+v, want none", res.Error)
	}
	// A negative loop_limit disables the guard entirely.
	res, _ = runLimitsSpec(t, true, loopBody(12, `{"command":"ls"}`, same, "sleep 0.5\nprintf finished"),
		func(s *wire.JobSpec) { s.LoopLimit = -1 }, func(c *Config, _ *wire.JobSpec) {
			c.IdleTimeout = time.Minute
			c.Timeout = time.Minute
		})
	if res.Error != nil {
		t.Fatalf("loop_limit=-1: error = %+v, want guard disabled", res.Error)
	}
}

// TestBudgetKillReapsSIGTERMIgnoringGrandchild: the leader dies on SIGTERM
// but a grandchild traps it; the harness must still SIGKILL the whole
// process group after the grace period.
func TestBudgetKillReapsSIGTERMIgnoringGrandchild(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "gc.pid")
	ev := `{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-5-5","usage":{"input_tokens":0,"output_tokens":500000},"content":[{"type":"text","text":"hi"}]}}`
	body := "sh -c 'trap \"\" TERM; echo $$ > " + pidFile + "; while :; do sleep 0.2; done' >/dev/null 2>&1 &\n" +
		"sleep 0.3\necho '" + ev + "'\nsleep 30"
	res, _ := runLimitsSpec(t, true, body, func(s *wire.JobSpec) { s.Budget.MaxCostUSD = 1 }, func(c *Config, _ *wire.JobSpec) {
		c.IdleTimeout = time.Minute
		c.Timeout = time.Minute
		c.TermGrace = 500 * time.Millisecond
	})
	if res.Error == nil || res.Error.Reason != wire.ReasonBudget {
		t.Fatalf("error = %+v, want budget", res.Error)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("grandchild never started: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid file %q: %v", raw, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // reaped
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("grandchild %d ignoring SIGTERM survived the budget kill", pid)
}
