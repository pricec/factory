package mcpfactory_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/budgets"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/mcpfactory"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/wire"
)

// stubMatcher is the dispatch Matcher stub (spec §6.7): returns a fixed
// foreman and records the requires list it was called with.
type stubMatcher struct {
	foreman  registry.Foreman
	ok       bool
	requires []string
}

func (m *stubMatcher) Match(_ context.Context, requires []string) (registry.Foreman, bool, error) {
	m.requires = requires
	return m.foreman, m.ok, nil
}

type env struct {
	t         *testing.T
	authStore *auth.Store
	store     *jobs.Store
	svc       *inbox.Service
	matcher   *stubMatcher
	base      string
	planner   string
	foreman   string
	worker    string
}

func newEnv(t *testing.T, budget budgets.Config) *env {
	t.Helper()
	dir := t.TempDir()
	authStore, err := auth.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	store, err := jobs.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatalf("jobs.Open: %v", err)
	}
	svc, err := inbox.Open(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatalf("inbox.Open: %v", err)
	}
	matcher := &stubMatcher{foreman: registry.Foreman{ID: "alpha", Online: true}, ok: true}
	h, err := mcpfactory.Handler(mcpfactory.Config{
		Auth:    authStore,
		Store:   store,
		Matcher: matcher,
		Inbox:   svc,
		Budget:  budget,
	})
	if err != nil {
		t.Fatalf("mcpfactory.Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { svc.Close(); store.Close(); authStore.Close() })

	ctx := context.Background()
	planner, err := authStore.MintPlannerToken(ctx, "factory:main")
	if err != nil {
		t.Fatalf("MintPlannerToken: %v", err)
	}
	pair, _, err := authStore.IssuePairingToken(ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	foreman, _, err := authStore.PairForeman(ctx, pair, "alpha")
	if err != nil {
		t.Fatalf("PairForeman: %v", err)
	}
	worker, err := authStore.MintWorkerToken(ctx, "foreman:alpha", "job_tst")
	if err != nil {
		t.Fatalf("MintWorkerToken: %v", err)
	}
	return &env{
		t: t, authStore: authStore, store: store, svc: svc, matcher: matcher,
		base: srv.URL, planner: planner, foreman: foreman, worker: worker,
	}
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

func (e *env) callTool(name string, args map[string]any) map[string]any {
	e.t.Helper()
	status, out := e.rpc(e.planner, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if status != 200 {
		e.t.Fatalf("tools/call %s status = %d, want 200: %v", name, status, out)
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
		t.Fatalf("no structuredContent object in %v", res)
	}
	return sc
}

// structuredArray extracts an array-valued structuredContent.
func structuredArray(t *testing.T, res map[string]any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(res["structuredContent"])
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structuredContent not a JSON array: %v (%s)", err, raw)
	}
	return out
}

func toolError(t *testing.T, name string, out map[string]any) string {
	t.Helper()
	res := resultOf(t, out)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("tools/call %s: want tool error, got %v", name, res)
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

// seedFinished creates a job that ran to a done result with the given
// summary, result payload, and usage cost.
func (e *env) seedFinished(t *testing.T, task, summary string, result json.RawMessage, cost float64) wire.JobSpec {
	t.Helper()
	ctx := context.Background()
	spec := wire.NewJobSpec(task, "factory")
	if err := e.store.Create(ctx, spec); err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	if err := e.store.Dispatch(ctx, spec.ID, "alpha"); err != nil {
		t.Fatalf("store.Dispatch: %v", err)
	}
	if err := e.store.ApplyEvent(ctx, spec.ID, "started"); err != nil {
		t.Fatalf("store.ApplyEvent: %v", err)
	}
	if err := e.store.ApplyResult(ctx, wire.JobResult{
		JobID: spec.ID, Status: wire.StatusDone, Summary: summary,
		Result: result, Usage: &wire.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: cost},
	}); err != nil {
		t.Fatalf("store.ApplyResult: %v", err)
	}
	return spec
}

func TestToolsListAndBudget(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())

	status, out := e.rpc(e.planner, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	if status != 200 {
		t.Fatalf("initialize status = %d: %v", status, out)
	}

	_, out = e.rpc(e.planner, "tools/list", nil)
	res := resultOf(t, out)
	tools, ok := res["tools"].([]any)
	if !ok || len(tools) != 5 {
		t.Fatalf("tools/list: want 5 tools, got %v", res["tools"])
	}
	want := map[string]bool{
		"list_foremen": false, "list_jobs": false, "create_job": false,
		"read_result": false, "message_foreman": false,
	}
	var budget int
	for _, tl := range tools {
		tm := tl.(map[string]any)
		name := tm["name"].(string)
		if _, known := want[name]; !known {
			t.Errorf("unexpected tool %q", name)
			continue
		}
		want[name] = true
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
	for name, seen := range want {
		if !seen {
			t.Errorf("tools/list missing %q", name)
		}
	}
	if budget >= 1536 {
		t.Errorf("description budget = %d bytes, want < 1536", budget)
	}
	t.Logf("description budget: %d bytes", budget)
}

func TestListJobsSummaryOnlyAndReadResult(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())

	big := json.RawMessage(`{"data":"` + strings.Repeat("x", 4096) + `"}`)
	spec := e.seedFinished(t, "do a big thing", "all done", big, 0.5)

	// list_jobs: summary only - no results array, no spec payload (#7).
	entries := structuredArray(t, resultOf(t, e.callTool("list_jobs", map[string]any{})))
	found := false
	for _, entry := range entries {
		if entry["id"] != spec.ID {
			continue
		}
		found = true
		if entry["state"] != "done" {
			t.Errorf("state = %v, want done", entry["state"])
		}
		if entry["summary"] != "all done" {
			t.Errorf("summary = %v, want %q", entry["summary"], "all done")
		}
		for _, key := range []string{"results", "spec"} {
			if _, exists := entry[key]; exists {
				t.Errorf("list entry leaks %q (read_result-only per #7): %v", key, entry)
			}
		}
		if entry["over_budget"] != false {
			t.Errorf("over_budget = %v, want false", entry["over_budget"])
		}
		var u map[string]any
		usage, _ := json.Marshal(entry["usage"])
		_ = json.Unmarshal(usage, &u)
		if u["cost_usd"] != 0.5 {
			t.Errorf("usage = %v, want cost_usd 0.5", entry["usage"])
		}
	}
	if !found {
		t.Fatalf("seeded job %s not in list_jobs output: %v", spec.ID, entries)
	}
	// read_result: the full payload enters LLM context here, and only here.
	sc := structured(t, resultOf(t, e.callTool("read_result", map[string]any{"job_id": spec.ID})))
	resultsJSON, err := json.Marshal(sc["results"])
	if err != nil {
		t.Fatalf("marshal results: %v", err)
	}
	if !strings.Contains(string(resultsJSON), strings.Repeat("x", 64)) {
		t.Errorf("read_result results missing the big payload: %s", resultsJSON)
	}
	var specOut wire.JobSpec
	specJSON, err := json.Marshal(sc["spec"])
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if err := json.Unmarshal(specJSON, &specOut); err != nil {
		t.Fatalf("read_result spec not a JobSpec: %v", err)
	}
	if specOut.Task != "do a big thing" {
		t.Errorf("read_result spec.Task = %q", specOut.Task)
	}
}

func TestCreateJobDispatch(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	ctx := context.Background()

	// Happy path: matcher returns a foreman -> dispatched.
	sc := structured(t, resultOf(t, e.callTool("create_job", map[string]any{
		"task":     "build the widget",
		"requires": []string{"go", "docker"},
	})))
	jobID, _ := sc["job_id"].(string)
	if !strings.HasPrefix(jobID, "job_") {
		t.Fatalf("job_id = %v", sc["job_id"])
	}
	if sc["state"] != "dispatched" {
		t.Fatalf("state = %v, want dispatched", sc["state"])
	}
	if sc["foreman"] != "alpha" {
		t.Fatalf("foreman = %v, want alpha", sc["foreman"])
	}
	if len(e.matcher.requires) != 2 || e.matcher.requires[0] != "go" {
		t.Errorf("matcher called with %v, want [go docker]", e.matcher.requires)
	}
	job, ok, err := e.store.Get(ctx, jobID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !ok || job.State != "dispatched" || job.ForemanID != "alpha" {
		t.Fatalf("stored job = %+v ok=%v", job, ok)
	}
	if job.Spec.CreatedBy != "factory" {
		t.Errorf("spec.CreatedBy = %q, want factory", job.Spec.CreatedBy)
	}

	// No match: the job stays pending, foreman null (spec §6.7).
	e.matcher.ok = false
	sc = structured(t, resultOf(t, e.callTool("create_job", map[string]any{"task": "no one can do this"})))
	if sc["state"] != "pending" {
		t.Fatalf("state = %v, want pending", sc["state"])
	}
	if f, exists := sc["foreman"]; exists && f != nil {
		t.Fatalf("foreman = %v, want null", sc["foreman"])
	}
	job, ok, err = e.store.Get(ctx, sc["job_id"].(string))
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !ok || job.State != "pending" {
		t.Fatalf("stored job = %+v ok=%v, want pending", job, ok)
	}
}

func TestCreateJobSchedule(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	ctx := context.Background()

	sc := structured(t, resultOf(t, e.callTool("create_job", map[string]any{
		"task":          "nightly sweep",
		"schedule_cron": "0 3 * * *",
		"timeout_s":     120,
		"priority":      7,
	})))
	jobID := sc["job_id"].(string)

	// The one-off job exists (AND the schedule, per the §9.4 pin).
	job, ok, err := e.store.Get(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("one-off job: ok=%v err=%v", ok, err)
	}
	if job.Spec.TimeoutS != 120 || job.Spec.Priority != 7 {
		t.Errorf("spec overrides not applied: %+v", job.Spec)
	}
	schedules, err := e.store.Schedules(ctx)
	if err != nil {
		t.Fatalf("store.Schedules: %v", err)
	}
	if len(schedules) != 1 {
		t.Fatalf("schedules = %d, want 1", len(schedules))
	}
	sch := schedules[0]
	if sch.CronSpec != "0 3 * * *" {
		t.Errorf("cron_spec = %q", sch.CronSpec)
	}
	if sch.JobTemplate.Task != "nightly sweep" {
		t.Errorf("template task = %q, want nightly sweep", sch.JobTemplate.Task)
	}
	if !sch.Enabled {
		t.Errorf("schedule not enabled")
	}

	// Invalid cron: tool error.
	msg := toolError(t, "create_job", e.callTool("create_job", map[string]any{
		"task": "bad cron", "schedule_cron": "not a cron",
	}))
	if !strings.Contains(msg, "cron") {
		t.Errorf("tool error = %q, want cron mention", msg)
	}
}

func TestCreateJobValidation(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty task", map[string]any{"task": ""}, "task"},
		{"bad priority", map[string]any{"task": "x", "priority": 42}, "priority"},
		{"bad timeout", map[string]any{"task": "x", "timeout_s": -1}, "timeout_s"},
		{"bad result_schema", map[string]any{"task": "x", "result_schema": "not json"}, "result_schema"},
		{"empty require", map[string]any{"task": "x", "requires": []string{""}}, "requires"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := toolError(t, "create_job", e.callTool("create_job", tc.args))
			if !strings.Contains(msg, tc.want) {
				t.Errorf("tool error = %q, want mention of %q", msg, tc.want)
			}
		})
	}
}

func TestCreateJobSpendBreaker(t *testing.T) {
	budget := budgets.Config{DailySpendCapUSD: 1.0, SpendOverride: false}
	e := newEnv(t, budget)
	ctx := context.Background()

	// Seed today's spend above the cap with a completed expensive job.
	e.seedFinished(t, "expensive", "costly", json.RawMessage(`{}`), 5.0)

	msg := toolError(t, "create_job", e.callTool("create_job", map[string]any{"task": "one more"}))
	if !strings.Contains(msg, "spend_cap_exceeded") {
		t.Fatalf("tool error = %q, want spend_cap_exceeded", msg)
	}

	// FACTORY_SPEND_OVERRIDE=1 equivalent: the same store accepts again.
	h2, err := mcpfactory.Handler(mcpfactory.Config{
		Auth: e.authStore, Store: e.store, Matcher: e.matcher, Inbox: e.svc,
		Budget: budgets.Config{DailySpendCapUSD: 1.0, SpendOverride: true},
	})
	if err != nil {
		t.Fatalf("override Handler: %v", err)
	}
	srv2 := httptest.NewServer(h2)
	t.Cleanup(srv2.Close)

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "create_job", "arguments": map[string]any{"task": "one more"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest("POST", srv2.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+e.planner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := resultOf(t, out)
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("override create_job errored: %v", res)
	}
	sc := structured(t, res)
	if sc["state"] != "dispatched" {
		t.Errorf("override state = %v, want dispatched", sc["state"])
	}
	// Sanity: the refused call created no job; the override call created one.
	listed, err := e.store.List(ctx, "", "", 200)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	if len(listed) != 2 { // the seeded expensive job + the override job
		t.Errorf("jobs after breaker test = %d, want 2 (refusal created none)", len(listed))
	}
}

func TestMessageForeman(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	ctx := context.Background()

	sc := structured(t, resultOf(t, e.callTool("message_foreman", map[string]any{
		"foreman_id": "alpha",
		"type":       "note",
		"payload":    map[string]any{"say": "hello"},
	})))
	if sc["ok"] != true {
		t.Fatalf("message_foreman: %v", sc)
	}
	if _, ok := sc["id"].(string); !ok {
		t.Fatalf("message_foreman: no envelope id: %v", sc)
	}

	envs, err := e.svc.Get(ctx, "foreman:alpha", 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("foreman inbox has %d envelopes, want 1", len(envs))
	}
	got := envs[0]
	if got.Type != "note" || got.From != "factory:planner" {
		t.Fatalf("envelope = %+v", got)
	}
	var p map[string]any
	if err := json.Unmarshal(got.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p["say"] != "hello" {
		t.Errorf("payload = %v", p)
	}

	// Job-scoped type without job_id: tool error (spec §3.1).
	msg := toolError(t, "message_foreman", e.callTool("message_foreman", map[string]any{
		"foreman_id": "alpha",
		"type":       "job.pause",
		"payload":    map[string]any{},
	}))
	if !strings.Contains(msg, "job_id") {
		t.Errorf("tool error = %q, want job_id mention", msg)
	}
}

func TestListForemen(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	ctx := context.Background()

	if err := e.store.UpsertForeman(ctx, "alpha", json.RawMessage(`{"os":"linux"}`)); err != nil {
		t.Fatalf("UpsertForeman: %v", err)
	}

	sc := structuredArray(t, resultOf(t, e.callTool("list_foremen", map[string]any{})))
	var foremen []struct {
		ID           string          `json:"id"`
		Online       bool            `json:"online"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	for _, m := range sc {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		var f struct {
			ID           string          `json:"id"`
			Online       bool            `json:"online"`
			Capabilities json.RawMessage `json:"capabilities"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("list_foremen entry: %v", err)
		}
		foremen = append(foremen, f)
	}
	if len(foremen) != 1 || foremen[0].ID != "alpha" || !foremen[0].Online {
		t.Fatalf("foremen = %v", foremen)
	}
	var caps map[string]string
	if err := json.Unmarshal(foremen[0].Capabilities, &caps); err != nil {
		t.Fatalf("capabilities passthrough: %v", err)
	}
	if caps["os"] != "linux" {
		t.Errorf("capabilities = %v", caps)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	tests := []struct {
		name   string
		bearer string
		status int
	}{
		{"no bearer", "", http.StatusUnauthorized},
		{"unknown bearer", "badtoken", http.StatusUnauthorized},
		{"foreman token", e.foreman, http.StatusForbidden},
		{"worker token", e.worker, http.StatusForbidden},
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
	// Sanity: the planner bearer passes.
	status, _ := e.rpc(e.planner, "tools/list", nil)
	if status != 200 {
		t.Errorf("planner bearer status = %d, want 200", status)
	}
}
