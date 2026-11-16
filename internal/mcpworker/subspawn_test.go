package mcpworker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// parentSpec returns a normalized parent spec for the depth/inheritance
// tests: tier default, 90s timeout, a non-default budget. It carries the
// env's fixed job id (the worker bearer's job scope).
func parentSpec(t *testing.T) wire.JobSpec {
	t.Helper()
	spec := wire.NewJobSpec("parent work", "foreman:alpha")
	spec.ID = jobID
	spec.Tier = wire.TierHardest
	spec.TimeoutS = 90
	spec.Budget = wire.Budget{MaxPromptBytes: 1000, MaxOutputTokens: 2000, MaxCostUSD: 3.5}
	if err := spec.Validate(); err != nil {
		t.Fatalf("parent spec invalid: %v", err)
	}
	return spec
}

func TestSpawnChild(t *testing.T) {
	e := newEnv(t)
	parent := parentSpec(t)
	e.setJob(parent)

	sc := structured(t, resultOf(t, e.call("spawn_child", map[string]any{
		"task": "child work",
		"result_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"answer": map[string]any{"type": "integer"}},
			"required":   []string{"answer"},
		},
	})))
	gotID, ok := sc["job_id"].(string)
	if !ok || !strings.HasPrefix(gotID, wire.JobIDPrefix) {
		t.Fatalf("spawn_child: job_id = %v, want a fresh %s id", sc["job_id"], wire.JobIDPrefix)
	}

	specs := e.spawner.recorded()
	if len(specs) != 1 {
		t.Fatalf("spawn recorded %d specs, want 1", len(specs))
	}
	spec := specs[0]
	if spec.ID != gotID || spec.Task != "child work" {
		t.Fatalf("spec = %+v", spec)
	}
	// The child links to its parent and is created by the worker identity.
	if spec.ParentJobID != jobID {
		t.Errorf("parent_job_id = %q, want %q", spec.ParentJobID, jobID)
	}
	if spec.CreatedBy != "worker:"+jobID {
		t.Errorf("created_by = %q, want worker:%s", spec.CreatedBy, jobID)
	}
	// Inheritance: tier comes from the parent.
	if spec.Tier != wire.TierHardest {
		t.Errorf("tier = %q, want the parent's %q", spec.Tier, wire.TierHardest)
	}
	// Caps: unset timeout/budget inherit the parent's.
	if spec.TimeoutS != 90 {
		t.Errorf("timeout_s = %d, want the parent's 90", spec.TimeoutS)
	}
	if spec.Budget != parent.Budget {
		t.Errorf("budget = %+v, want the parent's %+v", spec.Budget, parent.Budget)
	}
	var schema map[string]any
	if err := json.Unmarshal(spec.ResultSchema, &schema); err != nil || schema["type"] != "object" {
		t.Fatalf("result_schema = %s: %v", spec.ResultSchema, err)
	}
}

func TestSpawnChildOverridesCapped(t *testing.T) {
	e := newEnv(t)
	parent := parentSpec(t)
	e.setJob(parent)

	sc := structured(t, resultOf(t, e.call("spawn_child", map[string]any{
		"task":      "child work",
		"tier":      wire.TierFlash,
		"timeout_s": 30,
		"budget": map[string]any{
			"max_prompt_bytes":  99999, // capped at the parent's 1000
			"max_output_tokens": 1500,  // under the parent's 2000: wins
			"max_cost_usd":      99.0,  // capped at the parent's 3.5
		},
	})))
	if sc["job_id"] == nil {
		t.Fatalf("spawn_child: %v, want job_id", sc)
	}
	specs := e.spawner.recorded()
	if len(specs) != 1 {
		t.Fatalf("spawn recorded %d specs, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Tier != wire.TierFlash {
		t.Errorf("tier = %q, want the caller's flash", spec.Tier)
	}
	if spec.TimeoutS != 30 {
		t.Errorf("timeout_s = %d, want the caller's 30 (under the parent's cap)", spec.TimeoutS)
	}
	if spec.Budget.MaxPromptBytes != 1000 {
		t.Errorf("max_prompt_bytes = %d, want capped at the parent's 1000", spec.Budget.MaxPromptBytes)
	}
	if spec.Budget.MaxOutputTokens != 1500 {
		t.Errorf("max_output_tokens = %d, want the caller's 1500", spec.Budget.MaxOutputTokens)
	}
	if spec.Budget.MaxCostUSD != 3.5 {
		t.Errorf("max_cost_usd = %v, want capped at the parent's 3.5", spec.Budget.MaxCostUSD)
	}
}

func TestSpawnChildTimeoutAboveParentCap(t *testing.T) {
	e := newEnv(t)
	e.setJob(parentSpec(t))

	sc := structured(t, resultOf(t, e.call("spawn_child", map[string]any{
		"task":      "child work",
		"timeout_s": 3600,
	})))
	if sc["job_id"] == nil {
		t.Fatalf("spawn_child: %v, want job_id", sc)
	}
	spec := e.spawner.recorded()[0]
	if spec.TimeoutS != 90 {
		t.Errorf("timeout_s = %d, want capped at the parent's 90", spec.TimeoutS)
	}
}

// TestSpawnChildDepthCap is the hard depth limit: a job that is itself a
// spawned child (its spec carries parent_job_id) cannot spawn - max 1
// level of children, and nothing is enqueued.
func TestSpawnChildDepthCap(t *testing.T) {
	e := newEnv(t)
	childSpec := parentSpec(t)
	childSpec.ParentJobID = "job_grandparent"
	if err := childSpec.Validate(); err != nil {
		t.Fatalf("child spec invalid: %v", err)
	}
	e.setJob(childSpec)

	msg := toolError(t, e.call("spawn_child", map[string]any{"task": "grandchild"}))
	if !strings.Contains(msg, "depth") {
		t.Fatalf("tool error = %q, want a depth refusal", msg)
	}
	if got := len(e.spawner.recorded()); got != 0 {
		t.Errorf("spawn recorded %d specs, want 0", got)
	}
}

func TestSpawnChildSpawnError(t *testing.T) {
	e := newEnv(t)
	e.setJob(parentSpec(t))
	e.spawner.err = error(spawnErr{"queue full"})
	msg := toolError(t, e.call("spawn_child", map[string]any{"task": "x"}))
	if !strings.Contains(msg, "queue full") {
		t.Fatalf("tool error = %q, want the spawn error", msg)
	}
}

type spawnErr struct{ s string }

func (e spawnErr) Error() string { return e.s }

func TestSpawnChildValidation(t *testing.T) {
	e := newEnv(t)
	e.setJob(parentSpec(t))
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty task", map[string]any{"task": ""}, "task"},
		{"schema outside subset", map[string]any{
			"task": "x", "result_schema": map[string]any{"pattern": "^a$"},
		}, "result_schema"},
		{"negative timeout", map[string]any{"task": "x", "timeout_s": -1}, "timeout_s"},
		{"non-object budget", map[string]any{"task": "x", "budget": "cheap"}, "budget"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := toolError(t, e.call("spawn_child", tc.args))
			if !strings.Contains(msg, tc.want) {
				t.Errorf("tool error = %q, want mention of %q", msg, tc.want)
			}
			if got := len(e.spawner.recorded()); got != 0 {
				t.Errorf("spawn recorded %d specs, want 0 (nothing enqueued)", got)
			}
		})
	}
}

// TestSpawnChildUnknownParent: no persisted spec for the calling job - the
// tool refuses loudly instead of spawning with invented caps.
func TestSpawnChildUnknownParent(t *testing.T) {
	e := newEnv(t)
	msg := toolError(t, e.call("spawn_child", map[string]any{"task": "x"}))
	if !strings.Contains(msg, "not found") {
		t.Fatalf("tool error = %q, want the missing-spec refusal", msg)
	}
	if got := len(e.spawner.recorded()); got != 0 {
		t.Errorf("spawn recorded %d specs, want 0", got)
	}
}

// childResultEnvelope builds the copy the foreman consumer posts into the
// parent's per-job inbox (payload byte-identical to the child's report).
func childResultEnvelope(t *testing.T, childID string, res wire.JobResult) (string, wire.MessageEnvelope) {
	t.Helper()
	payload, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	env := wire.MessageEnvelope{
		Type:    "job.result",
		JobID:   childID,
		From:    "worker:" + childID,
		Payload: payload,
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("envelope invalid: %v", err)
	}
	return env.ID, env
}

func TestCollectChildrenReturnsPendingResults(t *testing.T) {
	e := newEnv(t)
	e.addChild(jobID, "job_child1")
	e.addChild(jobID, "job_child2")

	res1 := wire.JobResult{JobID: "job_child1", Status: wire.StatusDone, Summary: "first child done"}
	_, env1 := childResultEnvelope(t, "job_child1", res1)
	if _, err := e.svc.Post(context.Background(), "job:"+jobID, env1); err != nil {
		t.Fatalf("post child result: %v", err)
	}
	// A job.answer is in flight for ask_parent: collect must neither
	// return nor consume it.
	if _, err := e.svc.Post(context.Background(), "job:"+jobID, wire.MessageEnvelope{
		Type: "job.answer", JobID: jobID, From: foremanInbox,
		Payload: json.RawMessage(`{"answer":"hold on"}`),
	}); err != nil {
		t.Fatalf("post answer: %v", err)
	}

	sc := structured(t, resultOf(t, e.call("collect_children", map[string]any{"wait_seconds": 2})))
	results, ok := sc["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("collect_children: %v, want exactly 1 result", sc["results"])
	}
	got, _ := results[0].(map[string]any)
	if got["job_id"] != "job_child1" || got["status"] != "done" || got["summary"] != "first child done" {
		t.Fatalf("result = %v, want job_child1 done", got)
	}

	// The returned copy was acked; a second collect (nothing new pending)
	// returns empty after its wait.
	start := time.Now()
	sc = structured(t, resultOf(t, e.call("collect_children", map[string]any{"wait_seconds": 1})))
	if results, _ = sc["results"].([]any); len(results) != 0 {
		t.Fatalf("second collect: %v, want empty", sc["results"])
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("second collect returned after %v, want it to have waited", elapsed)
	}

	// The answer survived for ask_parent.
	envs, err := e.svc.Get(context.Background(), "job:"+jobID, 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 1 || envs[0].Type != "job.answer" {
		t.Fatalf("job inbox = %+v, want only the untouched job.answer", envs)
	}
}

func TestCollectChildrenFiltersForeignResults(t *testing.T) {
	e := newEnv(t)
	e.addChild(jobID, "job_mine")

	// A copy for a child this attempt did not spawn (a previous attempt's
	// stale copy): left in place, never returned.
	_, stale := childResultEnvelope(t, "job_stale", wire.JobResult{
		JobID: "job_stale", Status: wire.StatusDone, Summary: "stale",
	})
	if _, err := e.svc.Post(context.Background(), "job:"+jobID, stale); err != nil {
		t.Fatalf("post stale copy: %v", err)
	}

	sc := structured(t, resultOf(t, e.call("collect_children", map[string]any{"wait_seconds": 1})))
	if results, _ := sc["results"].([]any); len(results) != 0 {
		t.Fatalf("collect returned %v, want the stale copy filtered out", sc["results"])
	}

	// The stale copy is still there (skipped, not consumed).
	envs, err := e.svc.Get(context.Background(), "job:"+jobID, 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 1 || envs[0].JobID != "job_stale" {
		t.Fatalf("job inbox = %+v, want the stale copy untouched", envs)
	}

	// Now the real child's copy arrives and IS returned.
	_, mine := childResultEnvelope(t, "job_mine", wire.JobResult{
		JobID: "job_mine", Status: wire.StatusFailed, Summary: "boom",
		Error: &wire.JobError{Kind: wire.KindCrash, Message: "child crashed"},
	})
	if _, err := e.svc.Post(context.Background(), "job:"+jobID, mine); err != nil {
		t.Fatalf("post child copy: %v", err)
	}
	sc = structured(t, resultOf(t, e.call("collect_children", map[string]any{"wait_seconds": 2})))
	results, _ := sc["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("collect returned %v, want the child's failed result", sc["results"])
	}
	got, _ := results[0].(map[string]any)
	if got["job_id"] != "job_mine" || got["status"] != "failed" {
		t.Fatalf("result = %v, want job_mine failed", got)
	}
}

// TestCollectChildrenWaitsForLateResult: the long-poll returns as soon as
// a copy lands, not only at the deadline.
func TestCollectChildrenWaitsForLateResult(t *testing.T) {
	e := newEnv(t)
	e.addChild(jobID, "job_late")

	go func() {
		time.Sleep(300 * time.Millisecond)
		_, env := childResultEnvelope(t, "job_late", wire.JobResult{
			JobID: "job_late", Status: wire.StatusDone, Summary: "late but done",
		})
		if _, err := e.svc.Post(context.Background(), "job:"+jobID, env); err != nil {
			t.Errorf("post: %v", err)
		}
	}()

	start := time.Now()
	sc := structured(t, resultOf(t, e.call("collect_children", map[string]any{"wait_seconds": 5})))
	results, _ := sc["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("collect returned %v, want the late result", sc["results"])
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("collect took %v, want it to return when the copy landed", elapsed)
	}
}

func TestCollectChildrenEmpty(t *testing.T) {
	e := newEnv(t)
	sc := structured(t, resultOf(t, e.call("collect_children", map[string]any{"wait_seconds": 1})))
	if results, _ := sc["results"].([]any); len(results) != 0 {
		t.Fatalf("collect returned %v, want empty on timeout", sc["results"])
	}
}
