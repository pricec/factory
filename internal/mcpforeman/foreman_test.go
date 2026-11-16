package mcpforeman_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/mcpforeman"
	"github.com/pricec/factory/internal/wire"
)

const (
	foremanInbox = "foreman:alpha"
	parentInbox  = "factory:main"
	jobID        = "job_abc"
)

// fakeSpawner records spawn_worker's enqueues (the harness seam,
// spec §9.3); an injected err simulates a full queue.
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

type env struct {
	t       *testing.T
	svc     *inbox.Service
	auth    *auth.Store
	spawner *fakeSpawner
	base    string
	brain   string // foreman bearer: the brain token is foreman-scoped (#18)
	worker  string // worker bearer: job-scoped, must 403
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
	spawner := &fakeSpawner{}
	h, err := mcpforeman.Handler(mcpforeman.Config{
		Auth:           authStore,
		Inbox:          svc,
		ForemanInboxID: foremanInbox,
		ParentInboxID:  parentInbox,
		Spawn:          spawner.Spawn,
	})
	if err != nil {
		t.Fatalf("mcpforeman.Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { svc.Close(); authStore.Close() })

	ctx := context.Background()
	pair, _, err := authStore.IssuePairingToken(ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	brain, _, err := authStore.PairForeman(ctx, pair, "alpha")
	if err != nil {
		t.Fatalf("PairForeman: %v", err)
	}
	worker, err := authStore.MintWorkerToken(ctx, foremanInbox, jobID)
	if err != nil {
		t.Fatalf("MintWorkerToken: %v", err)
	}
	return &env{
		t: t, svc: svc, auth: authStore, spawner: spawner, base: srv.URL,
		brain: brain, worker: worker,
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
	status, out := e.rpc(e.brain, "tools/call", map[string]any{
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

func TestHandlerConfigValidation(t *testing.T) {
	dir := t.TempDir()
	authStore, err := auth.Open(filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	defer authStore.Close()
	svc, err := inbox.Open(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatalf("inbox.Open: %v", err)
	}
	defer svc.Close()
	spawn := func(context.Context, wire.JobSpec) error { return nil }

	tests := []struct {
		name string
		cfg  mcpforeman.Config
		want string
	}{
		{"no auth", mcpforeman.Config{Inbox: svc, ForemanInboxID: "f", ParentInboxID: "p", Spawn: spawn}, "Config.Auth"},
		{"no inbox", mcpforeman.Config{Auth: authStore, ForemanInboxID: "f", ParentInboxID: "p", Spawn: spawn}, "Config.Inbox"},
		{"no foreman inbox", mcpforeman.Config{Auth: authStore, Inbox: svc, ParentInboxID: "p", Spawn: spawn}, "Config.ForemanInboxID"},
		{"no parent inbox", mcpforeman.Config{Auth: authStore, Inbox: svc, ForemanInboxID: "f", Spawn: spawn}, "Config.ParentInboxID"},
		{"no spawn seam", mcpforeman.Config{Auth: authStore, Inbox: svc, ForemanInboxID: "f", ParentInboxID: "p"}, "Config.Spawn"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mcpforeman.Handler(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Handler err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestInitializeToolsListAndBudget(t *testing.T) {
	e := newEnv(t)

	status, out := e.rpc(e.brain, "initialize", map[string]any{
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

	_, out = e.rpc(e.brain, "tools/list", nil)
	res := resultOf(t, out)
	tools, ok := res["tools"].([]any)
	if !ok || len(tools) != 6 {
		t.Fatalf("tools/list: want exactly 6 tools, got %v", res["tools"])
	}
	want := map[string]bool{
		"inbox_next": true, "inbox_ack": true, "post_to_factory": true,
		"spawn_worker": true, "answer_worker": true, "raise_alert": true,
	}
	var budget int
	for _, tl := range tools {
		tm := tl.(map[string]any)
		name := tm["name"].(string)
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
	if budget >= 2048 {
		t.Errorf("description budget = %d bytes, want < 2048 (2KB; raised from 1.5KB for raise_alert, S3)", budget)
	}
	t.Logf("description budget: %d bytes (limit 2048)", budget)
}

func TestInboxNextAck(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Two envelopes in the foreman inbox; FIFO returns the oldest first.
	first, err := e.svc.Post(ctx, foremanInbox, wire.MessageEnvelope{
		Type:    "brain.wake",
		From:    parentInbox,
		Payload: json.RawMessage(`{"reason":"schedule"}`),
	})
	if err != nil {
		t.Fatalf("post first: %v", err)
	}
	second, err := e.svc.Post(ctx, foremanInbox, wire.MessageEnvelope{
		Type:    "job.question",
		JobID:   jobID,
		From:    "worker:" + jobID,
		Payload: json.RawMessage(`{"question":"which config?"}`),
	})
	if err != nil {
		t.Fatalf("post second: %v", err)
	}

	sc := structured(t, resultOf(t, e.callTool("inbox_next", nil)))
	msg, ok := sc["message"].(map[string]any)
	if !ok {
		t.Fatalf("inbox_next: message = %v, want an envelope", sc["message"])
	}
	if msg["id"] != first.ID || msg["type"] != "brain.wake" {
		t.Fatalf("inbox_next: oldest = %v, want %s", msg, first.ID)
	}

	// Ack the first; the second becomes the oldest unacked.
	sc = structured(t, resultOf(t, e.callTool("inbox_ack", map[string]any{
		"ids": []string{first.ID},
	})))
	if sc["ok"] != true {
		t.Fatalf("inbox_ack: %v, want ok:true", sc)
	}
	sc = structured(t, resultOf(t, e.callTool("inbox_next", nil)))
	msg, ok = sc["message"].(map[string]any)
	if !ok || msg["id"] != second.ID || msg["job_id"] != jobID {
		t.Fatalf("inbox_next after ack: %v, want %s", sc["message"], second.ID)
	}

	// Ack the second; the drained inbox yields {message: null}.
	sc = structured(t, resultOf(t, e.callTool("inbox_ack", map[string]any{
		"ids": []string{second.ID},
	})))
	if sc["ok"] != true {
		t.Fatalf("inbox_ack second: %v, want ok:true", sc)
	}
	sc = structured(t, resultOf(t, e.callTool("inbox_next", nil)))
	if sc["message"] != nil {
		t.Fatalf("inbox_next drained: %v, want message null", sc)
	}
}

func TestInboxNextWaits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	go func() {
		time.Sleep(200 * time.Millisecond)
		if _, err := e.svc.Post(ctx, foremanInbox, wire.MessageEnvelope{
			Type: "brain.wake", From: parentInbox,
		}); err != nil {
			e.t.Errorf("post: %v", err)
		}
	}()

	start := time.Now()
	sc := structured(t, resultOf(t, e.callTool("inbox_next", map[string]any{"wait_seconds": 5})))
	msg, ok := sc["message"].(map[string]any)
	if !ok || msg["type"] != "brain.wake" {
		t.Fatalf("inbox_next: %v, want the wake after waiting", sc["message"])
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("inbox_next returned after %v, want it to have waited", elapsed)
	}
}

func TestPostToFactory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	sc := structured(t, resultOf(t, e.callTool("post_to_factory", map[string]any{
		"type":    "job.event",
		"job_id":  jobID,
		"payload": map[string]any{"event": "accepted"},
	})))
	if sc["ok"] != true {
		t.Fatalf("post_to_factory: %v, want ok:true", sc)
	}
	id, ok := sc["id"].(string)
	if !ok || !strings.HasPrefix(id, wire.MessageIDPrefix) {
		t.Fatalf("post_to_factory id = %v, want a server-assigned %s id", sc["id"], wire.MessageIDPrefix)
	}

	envs, err := e.svc.Get(ctx, parentInbox, 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("parent inbox has %d envelopes, want 1", len(envs))
	}
	got := envs[0]
	if got.ID != id || got.Type != "job.event" || got.JobID != jobID || got.From != foremanInbox {
		t.Fatalf("envelope = %+v", got)
	}
	var p map[string]any
	if err := json.Unmarshal(got.Payload, &p); err != nil || p["event"] != "accepted" {
		t.Fatalf("payload = %s: %v", got.Payload, err)
	}
}

func TestPostToFactoryValidation(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"job type without job_id", map[string]any{
			"type": "job.result", "payload": map[string]any{"status": "done"},
		}, "job_id"},
		{"job type with unprefixed job_id", map[string]any{
			"type": "job.event", "job_id": "abc",
		}, "job_id"},
		{"empty type", map[string]any{"type": "", "payload": "x"}, "type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := toolError(t, e.callTool("post_to_factory", tc.args))
			if !strings.Contains(msg, tc.want) {
				t.Errorf("tool error = %q, want mention of %q", msg, tc.want)
			}
		})
	}
}

func TestSpawnWorker(t *testing.T) {
	e := newEnv(t)

	sc := structured(t, resultOf(t, e.callTool("spawn_worker", map[string]any{
		"task": "count to ten",
		"result_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"answer": map[string]any{"type": "integer"}},
			"required":   []string{"answer"},
		},
	})))
	gotID, ok := sc["job_id"].(string)
	if !ok || !strings.HasPrefix(gotID, wire.JobIDPrefix) {
		t.Fatalf("spawn_worker: job_id = %v, want a fresh %s id", sc["job_id"], wire.JobIDPrefix)
	}

	specs := e.spawner.recorded()
	if len(specs) != 1 {
		t.Fatalf("spawn recorded %d specs, want 1", len(specs))
	}
	spec := specs[0]
	if spec.ID != gotID || spec.Task != "count to ten" || spec.CreatedBy != foremanInbox {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.TimeoutS != wire.DefaultTimeoutS || spec.Priority != wire.DefaultPriority {
		t.Fatalf("defaults not applied: %+v", spec)
	}
	if spec.Budget != (wire.Budget{
		MaxPromptBytes:  wire.DefaultMaxPromptBytes,
		MaxOutputTokens: wire.DefaultMaxOutputTokens,
		MaxCostUSD:      wire.DefaultMaxCostUSD,
	}) {
		t.Fatalf("budget defaults not applied: %+v", spec.Budget)
	}
	var schema map[string]any
	if err := json.Unmarshal(spec.ResultSchema, &schema); err != nil || schema["type"] != "object" {
		t.Fatalf("result_schema = %s: %v", spec.ResultSchema, err)
	}

	// Optional overrides: timeout and a partial budget (Normalize refills
	// the fields left at zero, spec §5.1).
	sc = structured(t, resultOf(t, e.callTool("spawn_worker", map[string]any{
		"task":      "say hi",
		"timeout_s": 17,
		"budget":    map[string]any{"max_cost_usd": 9.5},
	})))
	nextID, _ := sc["job_id"].(string)
	if nextID == "" || nextID == gotID {
		t.Fatalf("second spawn job_id = %v, want a fresh id", sc["job_id"])
	}
	specs = e.spawner.recorded()
	if len(specs) != 2 {
		t.Fatalf("spawn recorded %d specs, want 2", len(specs))
	}
	spec = specs[1]
	if spec.ID != nextID || spec.Task != "say hi" || spec.TimeoutS != 17 {
		t.Fatalf("overrides not applied: %+v", spec)
	}
	if spec.Budget.MaxCostUSD != 9.5 || spec.Budget.MaxPromptBytes != wire.DefaultMaxPromptBytes {
		t.Fatalf("budget merge: %+v", spec.Budget)
	}
}

func TestSpawnWorkerValidation(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"schema outside subset", map[string]any{
			"task": "x", "result_schema": map[string]any{"pattern": "^a$"},
		}, "result_schema"},
		{"empty task", map[string]any{"task": ""}, "task"},
		{"negative timeout", map[string]any{"task": "x", "timeout_s": -1}, "timeout_s"},
		{"non-object budget", map[string]any{"task": "x", "budget": "cheap"}, "budget"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := toolError(t, e.callTool("spawn_worker", tc.args))
			if !strings.Contains(msg, tc.want) {
				t.Errorf("tool error = %q, want mention of %q", msg, tc.want)
			}
			if got := len(e.spawner.recorded()); got != 0 {
				t.Errorf("spawn recorded %d specs, want 0 (nothing enqueued)", got)
			}
		})
	}
}

func TestSpawnWorkerSpawnError(t *testing.T) {
	e := newEnv(t)
	e.spawner.err = errors.New("queue full")
	msg := toolError(t, e.callTool("spawn_worker", map[string]any{"task": "x"}))
	if !strings.Contains(msg, "queue full") {
		t.Fatalf("tool error = %q, want the spawn error", msg)
	}
}

func TestAnswerWorker(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	sc := structured(t, resultOf(t, e.callTool("answer_worker", map[string]any{
		"job_id": jobID,
		"answer": "use the blue one",
	})))
	if sc["ok"] != true {
		t.Fatalf("answer_worker: %v, want ok:true", sc)
	}

	envs, err := e.svc.Get(ctx, "job:"+jobID, 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("job inbox has %d envelopes, want 1", len(envs))
	}
	got := envs[0]
	if got.Type != "job.answer" || got.JobID != jobID || got.From != foremanInbox {
		t.Fatalf("envelope = %+v", got)
	}
	var p struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(got.Payload, &p); err != nil || p.Answer != "use the blue one" {
		t.Fatalf("payload = %s: %v", got.Payload, err)
	}

	// A malformed job_id never reaches the job inbox.
	msg := toolError(t, e.callTool("answer_worker", map[string]any{"job_id": "abc", "answer": "x"}))
	if !strings.Contains(msg, "job_id") {
		t.Fatalf("tool error = %q, want mention of job_id", msg)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Another foreman's credential is foreman-scoped too: the surface
	// enforces foreman identity + empty job scope, nothing tighter (#18).
	pair, _, err := e.auth.IssuePairingToken(ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	otherForeman, _, err := e.auth.PairForeman(ctx, pair, "beta")
	if err != nil {
		t.Fatalf("PairForeman: %v", err)
	}

	tests := []struct {
		name   string
		bearer string
		status int
	}{
		{"no bearer", "", http.StatusUnauthorized},
		{"unknown bearer", "badtoken", http.StatusUnauthorized},
		{"worker token", e.worker, http.StatusForbidden},
		{"other foreman token", otherForeman, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, out := e.rpc(tc.bearer, "tools/list", nil)
			if status != tc.status {
				t.Fatalf("status = %d, want %d: %v", status, tc.status, out)
			}
			if tc.status == http.StatusOK {
				return
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
}

// TestPostToFactoryStringifiedObjectPayload: LLM clients send an object
// payload as a JSON string; it must reach the parent inbox as a
// structured object, while genuine string payloads are unchanged.
func TestPostToFactoryStringifiedObjectPayload(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	post := func(typ string, payload any) wire.MessageEnvelope {
		t.Helper()
		sc := structured(t, resultOf(t, e.callTool("post_to_factory", map[string]any{
			"type": typ, "job_id": jobID, "payload": payload,
		})))
		if sc["ok"] != true {
			t.Fatalf("post_to_factory: %v", sc)
		}
		envs, err := e.svc.Get(ctx, parentInbox, 1, 0)
		if err != nil || len(envs) != 1 {
			t.Fatalf("inbox.Get: %v (%d envs)", err, len(envs))
		}
		if err := e.svc.Ack(ctx, parentInbox, []string{envs[0].ID}); err != nil {
			t.Fatalf("ack: %v", err)
		}
		return envs[0]
	}

	// Object payload as a JSON string round-trips as an object.
	got := post("foreman.report", `{"text": "DEPLOY-18 CLOSED"}`)
	var obj map[string]any
	if err := json.Unmarshal(got.Payload, &obj); err != nil || obj["text"] != "DEPLOY-18 CLOSED" {
		t.Fatalf("stringified object payload = %s (%v), want a JSON object", got.Payload, err)
	}

	// alert.raise via this path parses as the alert shape (not Malformed).
	got = post("alert.raise", `{"priority":2,"title":"disk full","body":"/ at 99%"}`)
	var al struct {
		Priority int    `json:"priority"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal(got.Payload, &al); err != nil || al.Title != "disk full" || al.Priority != 2 {
		t.Fatalf("alert.raise payload = %s (%v), want a parseable alert object", got.Payload, err)
	}

	// A native object is unchanged.
	got = post("job.event", map[string]any{"event": "accepted"})
	if string(got.Payload) != `{"event":"accepted"}` {
		t.Fatalf("object payload = %s", got.Payload)
	}

	// Plain string payloads keep working (back-compat).
	got = post("foreman.report", "all done")
	if string(got.Payload) != `"all done"` {
		t.Fatalf("string payload = %s, want a JSON string", got.Payload)
	}
}

// TestPostToFactoryTaskIDArg: the optional task_id argument sets
// payload.task_id for every payload shape (spec §9.5.2 threading).
func TestPostToFactoryTaskIDArg(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		payload any
		want    map[string]any
	}{
		"object":          {map[string]any{"text": "hi"}, map[string]any{"text": "hi", "task_id": "task_x"}},
		"stringified":     {`{"text": "hi"}`, map[string]any{"text": "hi", "task_id": "task_x"}},
		"plain string":    {"hi", map[string]any{"text": "hi", "task_id": "task_x"}},
		"overrides inner": {map[string]any{"text": "hi", "task_id": "task_old"}, map[string]any{"text": "hi", "task_id": "task_x"}},
	} {
		sc := structured(t, resultOf(t, e.callTool("post_to_factory", map[string]any{
			"type": "foreman.report", "payload": tc.payload, "task_id": "task_x",
		})))
		if sc["ok"] != true {
			t.Fatalf("%s: %v", name, sc)
		}
		envs, err := e.svc.Get(ctx, parentInbox, 1, 0)
		if err != nil || len(envs) != 1 {
			t.Fatalf("%s: Get: %v (%d)", name, err, len(envs))
		}
		_ = e.svc.Ack(ctx, parentInbox, []string{envs[0].ID})
		var got map[string]any
		if err := json.Unmarshal(envs[0].Payload, &got); err != nil || len(got) != len(tc.want) {
			t.Fatalf("%s: payload = %s (%v)", name, envs[0].Payload, err)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: payload[%s] = %v, want %v", name, k, got[k], v)
			}
		}
	}
}
