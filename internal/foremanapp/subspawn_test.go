package foremanapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

// errPostDown stands in for a down report endpoint (the §8.5 loud-failure
// trigger in spawner_test.go).
var errPostDown = errors.New("post down")

// mcpCall posts one worker-MCP tools/call as the given bearer and returns
// status + decoded body (the mcpworker test pattern, over the real foreman
// mux).
func mcpCall(t *testing.T, base, bearer, tool string, args map[string]any) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal rpc: %v", err)
	}
	req, err := http.NewRequest("POST", base+"/v1/mcp/worker", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST tools/call %s: %v", tool, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s response: %v", tool, err)
	}
	return resp.StatusCode, out
}

// mcpStructured extracts the tool's structuredContent, failing on tool
// errors.
func mcpStructured(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result object in %v", out)
	}
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("unexpected tool error: %v", res["content"])
	}
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("no structuredContent in %v", res)
	}
	return sc
}

// mcpToolError extracts a tool error's text, failing when the call is not
// an error.
func mcpToolError(t *testing.T, out map[string]any) string {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result object in %v", out)
	}
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("want tool error, got %v", res)
	}
	var text string
	for _, c := range res["content"].([]any) {
		cm, _ := c.(map[string]any)
		if cm["type"] == "text" {
			text = cm["text"].(string)
		}
	}
	return text
}

// subspawnCall calls one worker-MCP tool with the given bearer, failing on
// a non-200 transport status.
func subspawnCall(t *testing.T, base, bearer, tool string, args map[string]any) map[string]any {
	t.Helper()
	status, out := mcpCall(t, base, bearer, tool, args)
	if status != 200 {
		t.Fatalf("%s status = %d: %v", tool, status, out)
	}
	return out
}

// subspawnEnv boots a factory + foreman pair with the fake worker binaries
// sub-spawning tests need: the parent worker stays alive for the whole
// test (a long sleep), children behave by their own fake binary. It also
// wires a recording brain so the tests can assert the foreman brain never
// relayed anything.
type subspawnEnv struct {
	fac    *factoryEnv
	forEnv *foremanEnv
	parent wire.JobSpec
	brain  *sessionRecorder // must record zero sessions in these tests
	// worker is the parent worker bearer the tests act with (the same
	// per-job shape SpawnWorker mints).
	worker string
}

func newSubspawnEnv(t *testing.T, childBin string) *subspawnEnv {
	t.Helper()
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	forEnv := newForeman(t, fac, token)

	// No brain session may ever run in these tests: sub-spawning must not
	// relay through the foreman brain.
	rec := &sessionRecorder{behavior: func(_ context.Context, _ int) error {
		return nil
	}}
	forEnv.a.Brain().Exec = rec.exec

	parentBin := fakeOpencode(t, `sleep 120`)
	orig := forEnv.a.BuildHarnessConfig
	forEnv.a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		c := orig(spec, bearer)
		if spec.ParentJobID != "" {
			c.OpenCODEBin = childBin
		} else {
			c.OpenCODEBin = parentBin
		}
		c.ResultURL = forEnv.ts.URL + "/v1/inboxes/foreman:alpha/messages"
		c.MCPURL = forEnv.ts.URL + "/v1/mcp/worker"
		c.Timeout = 60 * time.Second
		c.PostRetryBackoff = 10 * time.Millisecond
		return c
	}

	// The parent: a real spawned worker job whose agent (simulated below
	// with its own per-job bearer over the real worker MCP) sub-spawns.
	parent := wire.NewJobSpec("parent task", "factory")
	if err := forEnv.a.SpawnWorker(fac.ctx, parent); err != nil {
		t.Fatalf("SpawnWorker parent: %v", err)
	}
	worker, err := forEnv.a.Auth().MintWorkerToken(fac.ctx, "foreman:alpha", parent.ID)
	if err != nil {
		t.Fatalf("MintWorkerToken parent: %v", err)
	}
	return &subspawnEnv{fac: fac, forEnv: forEnv, parent: parent, brain: rec, worker: worker}
}

// TestWorkerSubspawnCollectAndDepth is the sub-spawning e2e: a worker
// spawns a child via spawn_child; the child is an ordinary registry-visible
// foreman job (same accepted/started events, no second visibility path);
// the parent collects the child's terminal result itself via
// collect_children - no foreman-brain session ever runs; and a child
// cannot spawn again (depth hard-capped at one level).
func TestWorkerSubspawnCollectAndDepth(t *testing.T) {
	childBin := fakeOpencode(t, `sleep 1; printf child-done`)
	e := newSubspawnEnv(t, childBin)

	// The parent spawns one child.
	sc := mcpStructured(t, subspawnCall(t, e.forEnv.ts.URL, e.worker,
		"spawn_child", map[string]any{"task": "child task"}))
	childID, ok := sc["job_id"].(string)
	if !ok || !strings.HasPrefix(childID, wire.JobIDPrefix) {
		t.Fatalf("spawn_child: job_id = %v, want a job_ id", sc["job_id"])
	}

	// A child cannot spawn again (depth): checked with the child's own
	// per-job bearer, before its terminal result invalidates it.
	childBearer, err := e.forEnv.a.Auth().MintWorkerToken(e.fac.ctx, "foreman:alpha", childID)
	if err != nil {
		t.Fatalf("MintWorkerToken child: %v", err)
	}
	msg := mcpToolError(t, subspawnCall(t, e.forEnv.ts.URL, childBearer,
		"spawn_child", map[string]any{"task": "grandchild"}))
	if !strings.Contains(msg, "depth") {
		t.Fatalf("child spawn_child error = %q, want a depth refusal", msg)
	}

	// The child is registry-visible through the ordinary spawn events, and
	// its registry spec carries the parent linkage.
	var job jobs.Job
	waitFor(t, "child done at the factory", 20*time.Second, func() bool {
		_ = e.fac.do("GET", "/v1/jobs/"+childID, "", nil, &job)
		return job.State == jobs.StateDone
	})
	if job.State != jobs.StateDone {
		t.Fatalf("child state = %s, want done", job.State)
	}
	if job.Spec.ParentJobID != e.parent.ID {
		t.Errorf("registry parent_job_id = %q, want %q", job.Spec.ParentJobID, e.parent.ID)
	}
	if job.ForemanID != "foreman:alpha" {
		t.Errorf("foreman_id = %q, want foreman:alpha", job.ForemanID)
	}
	if job.Spec.CreatedBy != "worker:"+e.parent.ID {
		t.Errorf("created_by = %q, want worker:%s", job.Spec.CreatedBy, e.parent.ID)
	}

	// The parent collects the child's result itself.
	sc = mcpStructured(t, subspawnCall(t, e.forEnv.ts.URL, e.worker,
		"collect_children", map[string]any{"wait_seconds": 5}))
	results, ok := sc["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("collect_children: %v, want the child's result", sc["results"])
	}
	got, _ := results[0].(map[string]any)
	if got["job_id"] != childID || got["status"] != "done" || got["summary"] != "child-done" {
		t.Fatalf("collected result = %v, want child done/child-done", got)
	}

	// Zero brain sessions: collection never touched the foreman brain.
	if calls := e.brain.calls(); calls != 0 {
		t.Errorf("brain sessions = %d, want 0 (no brain relay)", calls)
	}
}

// TestChildSpawnFailureVisibleToParent is the loud-failure leg: a child
// whose harness cannot deliver its report surfaces as a failed job at the
// factory (the §8.5 spawn-failure path) AND the parent sees the failed
// result via collect_children - never silent.
func TestChildSpawnFailureVisibleToParent(t *testing.T) {
	childBin := fakeOpencode(t, `echo boom >&2; exit 1`)
	e := newSubspawnEnv(t, childBin)

	// Report delivery is down for children: the harness cannot land its
	// own crash result, so the spawner synthesizes the loud failed result.
	orig := e.forEnv.a.BuildHarnessConfig
	e.forEnv.a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		c := orig(spec, bearer)
		if spec.ParentJobID != "" {
			c.Post = func(_ context.Context, _, _ string, _ wire.JobResult) error {
				return errPostDown
			}
		}
		return c
	}

	sc := mcpStructured(t, subspawnCall(t, e.forEnv.ts.URL, e.worker,
		"spawn_child", map[string]any{"task": "failing child"}))
	childID, _ := sc["job_id"].(string)
	if childID == "" {
		t.Fatalf("spawn_child: %v, want job_id", sc)
	}

	var job jobs.Job
	waitFor(t, "child failed at the factory", 20*time.Second, func() bool {
		_ = e.fac.do("GET", "/v1/jobs/"+childID, "", nil, &job)
		return job.State == jobs.StateFailed
	})
	if job.State != jobs.StateFailed {
		t.Fatalf("child state = %s, want failed", job.State)
	}
	if !strings.Contains(job.LastError, "spawn failure") {
		t.Errorf("last_error = %q, want the synthesized spawn failure", job.LastError)
	}

	// The parent sees the failed child result via collect_children.
	sc = mcpStructured(t, subspawnCall(t, e.forEnv.ts.URL, e.worker,
		"collect_children", map[string]any{"wait_seconds": 10}))
	results, ok := sc["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("collect_children: %v, want the child's failed result", sc["results"])
	}
	got, _ := results[0].(map[string]any)
	if got["job_id"] != childID || got["status"] != "failed" {
		t.Fatalf("collected result = %v, want child failed", got)
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil || !strings.Contains(errObj["message"].(string), "spawn failure") {
		t.Fatalf("error = %v, want the spawn failure surfaced to the parent", got["error"])
	}
}
