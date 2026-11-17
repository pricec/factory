package mcpworker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/mcpworker"
	"github.com/pricec/factory/internal/wire"
)

const (
	foremanInbox = "foreman:alpha"
	jobID        = "job_abc"
)

type env struct {
	t       *testing.T
	svc     *inbox.Service
	auth    *auth.Store
	handler http.Handler
	base    string
	worker  string // worker bearer
	foreman string // foreman bearer (no job scope)
	workDir string
	spawner *fakeSpawner               // spawn_child enqueues (spec §9.2)
	parents map[string]wire.JobSpec    // ParentSpec stub backing store
	childs  map[string]map[string]bool // ChildJobs stub: parent -> child ids
	merges  *fakeMergeQueue            // request_merge relay (spec §9.2)
}

// fakeMergeQueue records request_merge relays (the foreman -> factory
// seam); an injected err simulates a factory refusal.
type fakeMergeQueue struct {
	mu    sync.Mutex
	jobs  []string
	calls []mcpworker.MergeRequestArgs
	err   error
}

func (f *fakeMergeQueue) request(_ context.Context, jobID string, in mcpworker.MergeRequestArgs) (mcpworker.MergeRequestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return mcpworker.MergeRequestResult{}, f.err
	}
	f.jobs = append(f.jobs, jobID)
	f.calls = append(f.calls, in)
	return mcpworker.MergeRequestResult{ID: "mr_TEST", State: "queued", WaitingOn: in.After}, nil
}

// fakeSpawner records spawn_child's enqueues (the harness seam, spec
// §9.2); an injected err simulates a spawn failure.
type fakeSpawner struct {
	mu    sync.Mutex
	specs []wire.JobSpec
	err   error
}

func (f *fakeSpawner) Spawn(_ context.Context, spec wire.JobSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.specs = append(f.specs, spec)
	return nil
}

func (f *fakeSpawner) recorded() []wire.JobSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wire.JobSpec(nil), f.specs...)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	authStore, err := auth.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	svc, err := inbox.Open(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatalf("inbox.Open: %v", err)
	}
	workDir := filepath.Join(dir, "work")
	spawner := &fakeSpawner{}
	parents := map[string]wire.JobSpec{}
	childs := map[string]map[string]bool{}
	merges := &fakeMergeQueue{}
	h, err := mcpworker.Handler(mcpworker.Config{
		Auth:           authStore,
		Inbox:          svc,
		ForemanInboxID: foremanInbox,
		WorkDir:        workDir,
		Spawn:          spawner.Spawn,
		ParentSpec: func(jobID string) (wire.JobSpec, bool, error) {
			spec, ok := parents[jobID]
			return spec, ok, nil
		},
		ChildJobs: func(parent string) []string {
			var out []string
			for id := range childs[parent] {
				out = append(out, id)
			}
			return out
		},
		RequestMerge: merges.request,
	})
	if err != nil {
		t.Fatalf("mcpworker.Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { svc.Close(); authStore.Close() })

	ctx := context.Background()
	worker, err := authStore.MintWorkerToken(ctx, foremanInbox, jobID)
	if err != nil {
		t.Fatalf("MintWorkerToken: %v", err)
	}
	pair, _, err := authStore.IssuePairingToken(ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	foreman, _, err := authStore.PairForeman(ctx, pair, "alpha")
	if err != nil {
		t.Fatalf("PairForeman: %v", err)
	}
	return &env{
		t: t, svc: svc, auth: authStore, handler: h, base: srv.URL,
		worker: worker, foreman: foreman, workDir: workDir,
		spawner: spawner, parents: parents, childs: childs, merges: merges,
	}
}

// setJob registers a persisted spec for one job (the ParentSpec stub
// backing store - the real one reads the spawner's job.json).
func (e *env) setJob(spec wire.JobSpec) {
	e.parents[spec.ID] = spec
}

// addChild records a child id under its parent (the ChildJobs stub).
func (e *env) addChild(parent, child string) {
	set := e.childs[parent]
	if set == nil {
		set = map[string]bool{}
		e.childs[parent] = set
	}
	set[child] = true
}

// rpc posts one JSON-RPC request and returns status + decoded body.
func (e *env) rpc(bearer, method string, params any) (int, map[string]any) {
	e.t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatalf("marshal rpc: %v", err)
	}
	req, err := http.NewRequest("POST", e.base, bytes.NewReader(b))
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("POST %s: %v", method, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		e.t.Fatalf("decode %s response: %v", method, err)
	}
	return resp.StatusCode, out
}

func (e *env) callTool(args map[string]any) map[string]any {
	return e.call("report_result", args)
}

func (e *env) call(name string, args map[string]any) map[string]any {
	e.t.Helper()
	status, out := e.rpc(e.worker, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if status != 200 {
		e.t.Fatalf("tools/call status = %d, want 200: %v", status, out)
	}
	return out
}

func resultOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result object in %v", out)
	}
	return res
}

func structured(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("no structuredContent in %v", res)
	}
	return sc
}

func toolError(t *testing.T, out map[string]any) string {
	t.Helper()
	res := resultOf(t, out)
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

func TestInitializeToolsListAndBudget(t *testing.T) {
	e := newEnv(t)

	status, out := e.rpc(e.worker, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	if status != 200 {
		t.Fatalf("initialize status = %d: %v", status, out)
	}
	if _, ok := out["result"]; !ok {
		t.Fatalf("initialize: no result: %v", out)
	}

	_, out = e.rpc(e.worker, "tools/list", nil)
	res := resultOf(t, out)
	tools, ok := res["tools"].([]any)
	if !ok || len(tools) != 5 {
		t.Fatalf("tools/list: want 5 tools, got %v", res["tools"])
	}
	want := map[string]bool{
		"report_result": true, "ask_parent": true,
		"spawn_child": true, "collect_children": true,
		"request_merge": true, // job-owned merges (spec §6.11)
	}
	names := map[string]bool{}
	var budget int
	for _, tl := range tools {
		tm := tl.(map[string]any)
		name := tm["name"].(string)
		names[name] = true
		if !want[name] {
			t.Errorf("unexpected tool %q", name)
		}
		delete(want, name)
		desc, _ := tm["description"].(string)
		budget += len(name) + len(desc)
		for _, key := range []string{"inputSchema", "outputSchema"} {
			if s, ok := tm[key]; ok && s != nil {
				b, err := json.Marshal(s)
				if err != nil {
					t.Fatalf("marshal %s: %v", key, err)
				}
				budget += len(b)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
	// Spec §9.1: worker surface < 2KB (raised from 1.5KB for request_merge,
	// the job-owned-merge tool, 2026-11-17).
	if budget >= 2048 {
		t.Errorf("description budget = %d bytes, want < 2048", budget)
	}
	t.Logf("description budget: %d bytes", budget)
}

func TestReportResult(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	args := map[string]any{
		"status":  "done",
		"summary": "all good",
		"result":  map[string]any{"answer": 42},
		"usage":   map[string]any{"input_tokens": 10, "output_tokens": 5, "cost_usd": 0.1},
	}
	sc := structured(t, resultOf(t, e.callTool(args)))
	if sc["ok"] != true {
		t.Fatalf("first report: structuredContent = %v, want ok:true", sc)
	}

	// Result envelope landed in the foreman inbox with the JobResult payload.
	envs, err := e.svc.Get(ctx, foremanInbox, 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("foreman inbox has %d envelopes, want 1", len(envs))
	}
	got := envs[0]
	if got.Type != "job.result" || got.JobID != jobID || got.From != "worker:"+jobID {
		t.Fatalf("envelope = %+v", got)
	}
	var jr wire.JobResult
	if err := json.Unmarshal(got.Payload, &jr); err != nil {
		t.Fatalf("payload not a JobResult: %v", err)
	}
	if jr.Status != "done" || jr.Summary != "all good" {
		t.Fatalf("JobResult = %+v", jr)
	}

	// Flag file exists (spec §8.5).
	flag := filepath.Join(e.workDir, "jobs", jobID, ".result_posted")
	if _, err := os.Stat(flag); err != nil {
		t.Fatalf("flag file %s: %v", flag, err)
	}

	// Second identical report: idempotent success, still one envelope.
	sc = structured(t, resultOf(t, e.callTool(args)))
	if sc["ok"] != true || sc["duplicate"] != true {
		t.Fatalf("second report: structuredContent = %v, want ok+duplicate", sc)
	}
	envs, err = e.svc.Get(ctx, foremanInbox, 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("foreman inbox has %d envelopes after double report, want 1", len(envs))
	}
}

func TestReportResultValidation(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty summary", map[string]any{"status": "done", "summary": ""}, "summary"},
		{"bogus status", map[string]any{"status": "bogus", "summary": "x"}, "status"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := toolError(t, e.callTool(tc.args))
			if !strings.Contains(msg, tc.want) {
				t.Errorf("tool error = %q, want mention of %q", msg, tc.want)
			}
		})
	}
}

func TestAskParentAnswer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// The foreman brain answers into the per-job inbox (spec §3.2).
	if _, err := e.svc.Post(ctx, "job:"+jobID, wire.MessageEnvelope{
		Type: "job.answer", JobID: jobID, From: foremanInbox,
		Payload: json.RawMessage(`{"answer":"use the blue one"}`),
	}); err != nil {
		t.Fatalf("post answer: %v", err)
	}

	status, out := e.rpc(e.worker, "tools/call", map[string]any{
		"name": "ask_parent",
		"arguments": map[string]any{
			"question":     "which config?",
			"wait_seconds": 5,
		},
	})
	if status != 200 {
		t.Fatalf("status = %d: %v", status, out)
	}
	sc := structured(t, resultOf(t, out))
	if sc["answer"] != "use the blue one" {
		t.Fatalf("answer = %v, want the pre-posted one", sc)
	}

	// The question landed in the foreman inbox.
	envs, err := e.svc.Get(ctx, foremanInbox, 10, 0)
	if err != nil || len(envs) != 1 {
		t.Fatalf("foreman inbox after ask_parent: %d envelopes, err %v", len(envs), err)
	}
	if envs[0].Type != "job.question" || envs[0].JobID != jobID {
		t.Fatalf("question envelope = %+v", envs[0])
	}

	// The answer was consumed (acked), so a fresh ask does not see it again.
	envs, err = e.svc.Get(ctx, "job:"+jobID, 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 0 {
		t.Fatalf("job inbox still holds %d envelopes, want 0 (acked)", len(envs))
	}
}

func TestAskParentTimeout(t *testing.T) {
	e := newEnv(t)
	start := time.Now()
	status, out := e.rpc(e.worker, "tools/call", map[string]any{
		"name":      "ask_parent",
		"arguments": map[string]any{"question": "anyone?", "wait_seconds": 1},
	})
	elapsed := time.Since(start)
	if status != 200 {
		t.Fatalf("status = %d: %v", status, out)
	}
	sc := structured(t, resultOf(t, out))
	if sc["answer"] != "proceed with your best judgment" {
		t.Fatalf("answer = %v, want the default", sc)
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout took %v, want ~1s", elapsed)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name   string
		bearer string
		status int
	}{
		{"no bearer", "", http.StatusUnauthorized},
		{"unknown bearer", "badtoken", http.StatusUnauthorized},
		{"foreman token, no job scope", e.foreman, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, out := e.rpc(tc.bearer, "tools/list", nil)
			if status != tc.status {
				t.Fatalf("status = %d, want %d: %v", status, tc.status, out)
			}
			errObj, ok := out["error"].(map[string]any)
			if !ok {
				t.Fatalf("want error model {\"error\":{...}}, got %v", out)
			}
			for _, key := range []string{"code", "message", "retryable"} {
				if _, ok := errObj[key]; !ok {
					t.Errorf("error model missing %q: %v", key, errObj)
				}
			}
		})
	}
	// Sanity: the worker bearer passes.
	status, _ := e.rpc(e.worker, "tools/list", nil)
	if status != 200 {
		t.Errorf("worker bearer status = %d, want 200", status)
	}
}
