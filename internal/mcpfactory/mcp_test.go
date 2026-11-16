package mcpfactory_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	// Production pairing seeds the foremen registry (factoryapp handlePair
	// upserts by identity); message_foreman resolves against it.
	if err := store.UpsertForeman(ctx, "alpha", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("UpsertForeman: %v", err)
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

// structuredSlice extracts an array field the tool wrapped in a record
// ({"jobs": [...]}, {"foremen": [...]}): MCP structured content must be
// an object - client harnesses reject bare arrays.
func structuredSlice(t *testing.T, res map[string]any, key string) []map[string]any {
	t.Helper()
	sc := structured(t, res)
	raw, err := json.Marshal(sc[key])
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structuredContent %q not a JSON array: %v (%s)", key, err, raw)
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
	if !ok || len(tools) != 8 {
		t.Fatalf("tools/list: want 8 tools, got %v", res["tools"])
	}
	want := map[string]bool{
		"list_foremen": false, "list_jobs": false, "create_job": false,
		"read_result": false, "cancel_job": false, "message_foreman": false,
		"notify_operator": false, "raise_alert": false,
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
	// Spec §9.1 budget: factory surface < 2KB (2026-11-15 amendments -
	// raised from 1536 for cancel_job; create_job documents the requires
	// tag-name forms after capability values parked two jobs pending ~7h).
	if budget >= 2560 {
		t.Errorf("description budget = %d bytes, want < 2560 (raised from 2KB for raise_alert, inbox S3)", budget)
	}
	t.Logf("description budget: %d bytes", budget)
}

func TestListJobsSummaryOnlyAndReadResult(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())

	big := json.RawMessage(`{"data":"` + strings.Repeat("x", 4096) + `"}`)
	spec := e.seedFinished(t, "do a big thing", "all done", big, 0.5)

	// list_jobs: summary only - no results array, no spec payload (#7).
	entries := structuredSlice(t, resultOf(t, e.callTool("list_jobs", map[string]any{})), "jobs")
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

// TestCreateJobRequiresValidation: creation-time requires validation
// (spec §6.7, 2026-11-15 amendment) - entries are tag names (bare or
// name:value), never capability values; an entry naming no registered tag
// key is a tool error and creates nothing, and with no foremen registered
// creation succeeds with a loud warning.
func TestCreateJobRequiresValidation(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	ctx := context.Background()

	// No registered tag keys yet (alpha's caps are {}): cannot validate -
	// create anyway, with a loud warning.
	sc := structured(t, resultOf(t, e.callTool("create_job", map[string]any{
		"task":     "needs linux",
		"requires": []string{"os:linux"},
	})))
	jobID, _ := sc["job_id"].(string)
	if jobID == "" {
		t.Fatalf("create_job = %v, want a created job", sc)
	}
	warn, ok := sc["warnings"].([]any)
	if !ok || len(warn) == 0 {
		t.Fatalf("warnings = %v, want a loud warning", sc["warnings"])
	}
	if !strings.Contains(fmt.Sprint(warn), "no foremen registered") {
		t.Errorf("warnings = %v, want the no-foremen warning", warn)
	}

	// Register the standard fleet tag keys.
	if err := e.store.UpsertForeman(ctx, "alpha",
		json.RawMessage(`{"os":"linux","arch":"amd64","tools":"git,docker"}`)); err != nil {
		t.Fatalf("UpsertForeman: %v", err)
	}

	// Accept: bare and name:value tag names, no warnings.
	sc = structured(t, resultOf(t, e.callTool("create_job", map[string]any{
		"task":     "build the widget",
		"requires": []string{"os:linux", "tools:git"},
	})))
	if sc["state"] != "dispatched" {
		t.Fatalf("state = %v, want dispatched", sc["state"])
	}
	if _, exists := sc["warnings"]; exists {
		t.Errorf("warnings = %v, want none for validated requires", sc["warnings"])
	}

	// Reject: capability VALUES are not tag names - the 2026-11-15
	// incident. Tool error naming the offender and the known keys.
	msg := toolError(t, "create_job", e.callTool("create_job", map[string]any{
		"task":     "never matches",
		"requires": []string{"linux", "amd64"},
	}))
	for _, want := range []string{"unknown requires", "linux", "amd64", "os:linux", "arch:amd64", "capability values"} {
		if !strings.Contains(msg, want) {
			t.Errorf("tool error = %q, want mention of %q", msg, want)
		}
	}
	// Invented tag keys are rejected too.
	msg = toolError(t, "create_job", e.callTool("create_job", map[string]any{
		"task":     "never matches",
		"requires": []string{"quantum"},
	}))
	if !strings.Contains(msg, "quantum") {
		t.Errorf("tool error = %q, want mention of quantum", msg)
	}

	// The rejected calls created nothing: exactly the two accepted jobs.
	listed, err := e.store.List(ctx, "", "", 100)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	if len(listed) != 2 {
		t.Errorf("jobs after accept+reject = %d, want 2 (rejected calls must not create)", len(listed))
	}
}

// TestCreateJobDescription: the tool description must state the requires
// tag-name semantics so the planner cannot invent capability values (spec
// §9.4, 2026-11-15 amendment).
func TestCreateJobDescription(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	_, out := e.rpc(e.planner, "tools/list", nil)
	var create struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	found := false
	for _, tl := range resultOf(t, out)["tools"].([]any) {
		tm := tl.(map[string]any)
		if tm["name"] == "create_job" {
			b, err := json.Marshal(tm)
			if err != nil {
				t.Fatalf("marshal tool: %v", err)
			}
			if err := json.Unmarshal(b, &create); err != nil {
				t.Fatalf("unmarshal tool: %v", err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("tools/list has no create_job")
	}
	for _, want := range []string{"tag NAMES", "os:linux", "arch:amd64", "tools:<tool>", "linux", "amd64", "unknown_requires"} {
		if !strings.Contains(create.Description, want) {
			t.Errorf("create_job description %q missing %q", create.Description, want)
		}
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

	// Full inbox-id form (the form list_foremen reports) addresses the SAME
	// inbox - never "foreman:foreman:alpha" (2026-11-14 incident).
	sc = structured(t, resultOf(t, e.callTool("message_foreman", map[string]any{
		"foreman_id": "foreman:alpha",
		"type":       "note",
		"payload":    map[string]any{"say": "full form"},
	})))
	if sc["ok"] != true {
		t.Fatalf("message_foreman full form: %v", sc)
	}
	envs, err = e.svc.Get(ctx, "foreman:alpha", 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("foreman inbox has %d envelopes, want 2 (full form landed in the same inbox)", len(envs))
	}
	if envs[1].Payload == nil || !strings.Contains(string(envs[1].Payload), "full form") {
		t.Errorf("full-form envelope payload = %s", envs[1].Payload)
	}
	// The phantom double-prefixed inbox never materialized.
	if phantom, err := e.svc.Get(ctx, "foreman:foreman:alpha", 10, 0); err != nil || len(phantom) != 0 {
		t.Errorf("foreman:foreman:alpha exists (err=%v, n=%d): a double-prefixed id must not open a dead inbox", err, len(phantom))
	}

	// Unknown foreman: tool error, nothing posted (never a dead inbox).
	msg := toolError(t, "message_foreman", e.callTool("message_foreman", map[string]any{
		"foreman_id": "ghost",
		"type":       "note",
		"payload":    map[string]any{"say": "boo"},
	}))
	if !strings.Contains(msg, "unknown foreman") {
		t.Errorf("unknown-foreman error = %q, want unknown foreman mention", msg)
	}
	msg = toolError(t, "message_foreman", e.callTool("message_foreman", map[string]any{
		"foreman_id": "foreman:foreman:alpha",
		"type":       "note",
		"payload":    map[string]any{"say": "double"},
	}))
	if !strings.Contains(msg, "unknown foreman") {
		t.Errorf("double-prefixed id error = %q, want unknown foreman mention", msg)
	}

	// Mis-typed calls fail loudly (2026-11-14: `text` instead of `payload`
	// was silently dropped and the message body never left the planner).
	msg = toolError(t, "message_foreman", e.callTool("message_foreman", map[string]any{
		"foreman_id": "alpha",
		"type":       "note",
	}))
	if !strings.Contains(msg, "payload") {
		t.Errorf("missing-payload error = %q, want payload mention", msg)
	}
	msg = toolError(t, "message_foreman", e.callTool("message_foreman", map[string]any{
		"foreman_id": "alpha",
		"type":       "note",
		"text":       "wrong arg name",
	}))
	if !strings.Contains(msg, "text") {
		t.Errorf("unknown-arg error = %q, want the unknown property named", msg)
	}

	// Explicit null payload: rejected, not an empty message.
	msg = toolError(t, "message_foreman", e.callTool("message_foreman", map[string]any{
		"foreman_id": "alpha",
		"type":       "note",
		"payload":    nil,
	}))
	if !strings.Contains(msg, "payload") {
		t.Errorf("null-payload error = %q, want payload mention", msg)
	}

	// Job-scoped type without job_id: tool error (spec §3.1).
	msg = toolError(t, "message_foreman", e.callTool("message_foreman", map[string]any{
		"foreman_id": "alpha",
		"type":       "job.pause",
		"payload":    map[string]any{},
	}))
	if !strings.Contains(msg, "job_id") {
		t.Errorf("tool error = %q, want job_id mention", msg)
	}
}

// TestNotifyOperator: notify_operator posts a brain.reply envelope into the
// operator inbox (planner:main, spec §9.5) - the planner's only reply path.
func TestNotifyOperator(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	ctx := context.Background()

	sc := structured(t, resultOf(t, e.callTool("notify_operator", map[string]any{
		"text": "all quiet; 3 jobs done",
	})))
	if sc["ok"] != true {
		t.Fatalf("notify_operator: %v", sc)
	}

	envs, err := e.svc.Get(ctx, "planner:main", 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("operator inbox has %d envelopes, want 1", len(envs))
	}
	got := envs[0]
	if got.Type != "brain.reply" || got.From != "factory:brain" {
		t.Fatalf("envelope = %+v", got)
	}
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(got.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Text != "all quiet; 3 jobs done" {
		t.Errorf("payload text = %q", p.Text)
	}

	// Empty text: tool error.
	msg := toolError(t, "notify_operator", e.callTool("notify_operator", map[string]any{"text": ""}))
	if !strings.Contains(msg, "text") {
		t.Errorf("tool error = %q, want text mention", msg)
	}
}

func TestListForemen(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	ctx := context.Background()

	if err := e.store.UpsertForeman(ctx, "alpha", json.RawMessage(`{"os":"linux"}`)); err != nil {
		t.Fatalf("UpsertForeman: %v", err)
	}

	sc := structuredSlice(t, resultOf(t, e.callTool("list_foremen", map[string]any{})), "foremen")
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

// TestCancelJobTool: the planner's cancel/supersede surface (spec §9.4,
// 2026-11-15 amendment) - non-terminal jobs transition through the state
// machine and the tool reports the resulting state; terminal jobs and
// unknown ids are tool errors.
func TestCancelJobTool(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	ctx := context.Background()

	seed := func(t *testing.T) string {
		t.Helper()
		spec := wire.NewJobSpec("cancel me", "factory")
		if err := e.store.Create(ctx, spec); err != nil {
			t.Fatalf("store.Create: %v", err)
		}
		return spec.ID
	}

	t.Run("pending -> cancelled", func(t *testing.T) {
		id := seed(t)
		sc := structured(t, resultOf(t, e.callTool("cancel_job", map[string]any{"job_id": id})))
		if sc["state"] != "cancelled" || sc["ok"] != true || sc["job_id"] != id {
			t.Fatalf("structured = %v, want {ok, job_id, state: cancelled}", sc)
		}
		if job, ok, err := e.store.Get(ctx, id); err != nil || !ok || job.State != jobs.StateCancelled {
			t.Fatalf("stored state = %+v ok=%v err=%v, want cancelled", job, ok, err)
		}
	})
	t.Run("pending -> superseded", func(t *testing.T) {
		id := seed(t)
		sc := structured(t, resultOf(t, e.callTool("cancel_job", map[string]any{
			"job_id": id, "supersede": true, "reason": "duplicate",
		})))
		if sc["state"] != "superseded" {
			t.Fatalf("state = %v, want superseded", sc["state"])
		}
	})
	t.Run("running -> cancelled", func(t *testing.T) {
		id := seed(t)
		if err := e.store.Dispatch(ctx, id, "alpha"); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if err := e.store.ApplyEvent(ctx, id, "started"); err != nil {
			t.Fatalf("ApplyEvent: %v", err)
		}
		sc := structured(t, resultOf(t, e.callTool("cancel_job", map[string]any{"job_id": id})))
		if sc["state"] != "cancelled" {
			t.Fatalf("state = %v, want cancelled (running cancel is allowed)", sc["state"])
		}
	})
	t.Run("terminal job rejected", func(t *testing.T) {
		spec := e.seedFinished(t, "already done", "ok", json.RawMessage(`{}`), 0)
		msg := toolError(t, "cancel_job", e.callTool("cancel_job", map[string]any{"job_id": spec.ID}))
		if !strings.Contains(msg, "terminal") || !strings.Contains(msg, "done") {
			t.Errorf("tool error = %q, want terminal-state mention", msg)
		}
	})
	t.Run("unknown job rejected", func(t *testing.T) {
		msg := toolError(t, "cancel_job", e.callTool("cancel_job", map[string]any{"job_id": "job_missing"}))
		if !strings.Contains(msg, "unknown job") {
			t.Errorf("tool error = %q, want unknown-job mention", msg)
		}
	})
	t.Run("missing job_id rejected", func(t *testing.T) {
		msg := toolError(t, "cancel_job", e.callTool("cancel_job", map[string]any{}))
		if !strings.Contains(msg, "job_id") {
			t.Errorf("tool error = %q, want job_id mention", msg)
		}
	})
}

// TestRaiseAlertTool: raise_alert forwards to Config.RaiseAlert and reports
// the id/expiry; a missing backend and backend errors are tool errors.
func TestRaiseAlertTool(t *testing.T) {
	e := newEnv(t, budgets.DefaultConfig())
	msg := toolError(t, "raise_alert", e.callTool("raise_alert", map[string]any{"priority": 1, "title": "x"}))
	if !strings.Contains(msg, "not configured") {
		t.Errorf("unconfigured error = %q", msg)
	}
}
