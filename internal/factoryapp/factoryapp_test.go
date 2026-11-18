package factoryapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/factoryapp"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// fakeClock: the App clock is fully controllable so the sweeper/liveness
// tests can time-travel without sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type testEnv struct {
	t      *testing.T
	a      *factoryapp.App
	ts     *httptest.Server
	clock  *fakeClock
	ctx    context.Context
	cancel context.CancelFunc
}

const testAdvertiseURL = "http://factory.test"

func newEnv(t *testing.T, opts ...func(*factoryapp.Config)) *testEnv {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	cfg := factoryapp.Config{
		DataDir:      t.TempDir(),
		Addr:         "127.0.0.1:0",
		AdvertiseURL: testAdvertiseURL,
		Now:          clock.Now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	a, err := factoryapp.New(cfg)
	if err != nil {
		t.Fatalf("factoryapp.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &testEnv{t: t, a: a, ts: httptest.NewServer(a.Handler()), clock: clock, ctx: ctx, cancel: cancel}
	go a.Run(ctx)
	t.Cleanup(func() {
		cancel()
		e.ts.Close()
		_ = a.Close()
	})
	return e
}

// do issues one HTTP request against the app's server. A string body is
// sent verbatim (for malformed-input cases); anything else is marshaled.
func (e *testEnv) do(method, path, bearer string, in any, out any) int {
	e.t.Helper()
	var body io.Reader
	if s, ok := in.(string); ok {
		body = strings.NewReader(s)
	} else if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			e.t.Fatalf("marshal request: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, body)
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("decode %s %s response: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

// rpc issues one MCP JSON-RPC request against /v1/mcp/factory with the
// planner bearer.
func (e *testEnv) rpc(method string, params any) (int, map[string]any) {
	e.t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatalf("marshal rpc: %v", err)
	}
	req, err := http.NewRequest("POST", e.ts.URL+"/v1/mcp/factory", bytes.NewReader(b))
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+e.a.PlannerToken())
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

func (e *testEnv) pair(id string, caps map[string]string) (bearer string) {
	e.t.Helper()
	token, _, err := e.a.Auth().IssuePairingToken(e.ctx)
	if err != nil {
		e.t.Fatalf("IssuePairingToken: %v", err)
	}
	var resp struct {
		Bearer    string `json:"bearer"`
		InboxID   string `json:"inbox_id"`
		ParentURL string `json:"parent_url"`
	}
	body := map[string]any{
		"pairing_token": token,
		"self":          map[string]any{"id": id, "capabilities": caps},
	}
	if status := e.do("POST", "/v1/pair", "", body, &resp); status != http.StatusOK {
		e.t.Fatalf("POST /v1/pair status = %d, want 200", status)
	}
	if resp.Bearer == "" || resp.InboxID != "foreman:"+id || resp.ParentURL != testAdvertiseURL {
		e.t.Fatalf("pair response = %+v", resp)
	}
	return resp.Bearer
}

// postToFactoryMain posts an envelope to the factory's main inbox with the
// given foreman bearer (post-only scope on factory:main, spec §7.3).
func (e *testEnv) postToFactoryMain(bearer string, env wire.MessageEnvelope) int {
	e.t.Helper()
	return e.do("POST", "/v1/inboxes/factory:main/messages", bearer, env, nil)
}

func waitFor(t *testing.T, what string, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: condition not met within %v", what, deadline)
}

func TestBootPlannerMCP(t *testing.T) {
	e := newEnv(t)
	token := e.a.PlannerToken()
	if token == "" {
		t.Fatal("PlannerToken() is empty")
	}

	status, out := e.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	if status != http.StatusOK {
		t.Fatalf("initialize status = %d: %v", status, out)
	}

	status, out = e.rpc("tools/list", nil)
	if status != http.StatusOK {
		t.Fatalf("tools/list status = %d: %v", status, out)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list: no result object: %v", out)
	}
	tools, _ := res["tools"].([]any)
	if len(tools) != 14 {
		t.Fatalf("tools/list: got %d tools, want 14", len(tools))
	}
	want := map[string]bool{
		"list_foremen": false, "list_jobs": false, "create_job": false,
		"read_result": false, "cancel_job": false, "message_foreman": false,
		"notify_operator": false, "raise_alert": false,
		"list_tasks": false, "get_task": false, "update_task": false, "set_priority_criteria": false,
		"request_merge": false, "get_message": false,
	}
	for _, tl := range tools {
		name := tl.(map[string]any)["name"].(string)
		if _, known := want[name]; !known {
			t.Errorf("unexpected tool %q", name)
			continue
		}
		want[name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tools/list missing %q", name)
		}
	}
}

func TestPair(t *testing.T) {
	e := newEnv(t)

	table := []struct {
		name   string
		body   any
		status int
	}{
		{"missing id", map[string]any{"pairing_token": "deadbeef", "self": map[string]any{"id": "", "capabilities": map[string]string{}}}, http.StatusBadRequest},
		{"bad token", map[string]any{"pairing_token": "deadbeef", "self": map[string]any{"id": "x", "capabilities": map[string]string{}}}, http.StatusUnauthorized},
		{"malformed json", `{not json`, http.StatusBadRequest},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			var errBody map[string]any
			if status := e.do("POST", "/v1/pair", "", tc.body, &errBody); status != tc.status {
				t.Fatalf("status = %d, want %d: %v", status, tc.status, errBody)
			}
		})
	}

	// Happy path: pairing registers the foreman with its capabilities.
	foremanBearer := e.pair("alpha", map[string]string{"gpu": "true"})
	if foremanBearer == "" {
		t.Fatal("pair returned empty bearer")
	}
	var foremen []jobs.Foreman
	if status := e.do("GET", "/v1/foremen", "", nil, &foremen); status != http.StatusOK {
		t.Fatalf("GET /v1/foremen status = %d", status)
	}
	if len(foremen) != 1 {
		t.Fatalf("foremen = %v, want 1 entry", foremen)
	}
	f := foremen[0]
	if f.ID != "foreman:alpha" || !f.Online {
		t.Fatalf("foreman row = %+v, want foreman:alpha online", f)
	}
	var caps map[string]string
	if err := json.Unmarshal([]byte(f.Capabilities), &caps); err != nil {
		t.Fatalf("capabilities passthrough: %v", err)
	}
	if caps["gpu"] != "true" {
		t.Errorf("capabilities = %v, want gpu=true", caps)
	}
}

func TestPairRefreshesCapabilities(t *testing.T) {
	e := newEnv(t)
	bearer := e.pair("alpha", nil)

	if status := e.do("POST", "/v1/foreman/capabilities", bearer, map[string]any{
		"capabilities": map[string]string{"os": "linux"},
	}, nil); status != http.StatusNoContent {
		t.Fatalf("capabilities refresh status = %d, want 204", status)
	}
	var foremen []jobs.Foreman
	_ = e.do("GET", "/v1/foremen", "", nil, &foremen)
	var caps map[string]string
	if err := json.Unmarshal([]byte(foremen[0].Capabilities), &caps); err != nil {
		t.Fatal(err)
	}
	if caps["os"] != "linux" {
		t.Errorf("capabilities after refresh = %v, want os=linux", caps)
	}
}

func TestPairingTokens(t *testing.T) {
	e := newEnv(t)

	// A foreman paired via the existing flow (#11) supplies the
	// non-planner bearer for the rejection table.
	foremanBearer := e.pair("alpha", nil)

	// Planner-credential auth only (spec §7.1): 401 for a
	// missing/unknown bearer, 403 for any non-planner identity.
	for _, tc := range []struct {
		name   string
		bearer string
		status int
		code   string
	}{
		{"foreman bearer", foremanBearer, http.StatusForbidden, "forbidden"},
		{"unknown bearer", "bearer-that-does-not-exist", http.StatusUnauthorized, "unauthorized"},
		{"missing authorization", "", http.StatusUnauthorized, "unauthorized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errBody struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if status := e.do("POST", "/v1/pairing-tokens", tc.bearer, nil, &errBody); status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if errBody.Error.Code != tc.code {
				t.Fatalf("error code = %q, want %q", errBody.Error.Code, tc.code)
			}
		})
	}

	// Happy path: the planner bearer (the v1 operator credential) mints a
	// one-time pairing token with an empty request body.
	var resp struct {
		PairingToken string `json:"pairing_token"`
		ExpiresAt    string `json:"expires_at"`
	}
	if status := e.do("POST", "/v1/pairing-tokens", e.a.PlannerToken(), nil, &resp); status != http.StatusOK {
		t.Fatalf("POST /v1/pairing-tokens status = %d, want 200", status)
	}
	if !isHex64(resp.PairingToken) {
		t.Fatalf("pairing_token = %q, want 64 hex chars (32B)", resp.PairingToken)
	}
	if _, err := time.Parse(time.RFC3339, resp.ExpiresAt); err != nil {
		t.Fatalf("expires_at = %q, want RFC3339: %v", resp.ExpiresAt, err)
	}

	// The issued token pairs a fresh foreman end to end.
	var pairResp struct {
		Bearer    string `json:"bearer"`
		InboxID   string `json:"inbox_id"`
		ParentURL string `json:"parent_url"`
	}
	if status := e.do("POST", "/v1/pair", "", map[string]any{
		"pairing_token": resp.PairingToken,
		"self":          map[string]any{"id": "bravo", "capabilities": map[string]string{}},
	}, &pairResp); status != http.StatusOK {
		t.Fatalf("POST /v1/pair status = %d, want 200", status)
	}
	if pairResp.Bearer == "" || pairResp.InboxID != "foreman:bravo" || pairResp.ParentURL != testAdvertiseURL {
		t.Fatalf("pair response = %+v", pairResp)
	}
	var foremen []jobs.Foreman
	if status := e.do("GET", "/v1/foremen", "", nil, &foremen); status != http.StatusOK {
		t.Fatalf("GET /v1/foremen status = %d", status)
	}
	got := map[string]bool{}
	for _, f := range foremen {
		got[f.ID] = true
	}
	if !got["foreman:alpha"] || !got["foreman:bravo"] {
		t.Fatalf("foremen = %+v, want foreman:alpha and foreman:bravo", foremen)
	}
}

func TestJobFlow(t *testing.T) {
	e := newEnv(t)
	foremanBearer := e.pair("alpha", map[string]string{"gpu": "true"})

	spec := wire.NewJobSpec("train the model", "factory")
	ctx := context.Background()
	if err := e.a.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := e.a.Jobs().Dispatch(ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// The dispatch notifier posts a job.request envelope to the foreman's
	// inbox (at-least-once).
	waitFor(t, "job.request in foreman inbox", 5*time.Second, func() bool {
		envs, err := e.a.Inbox().Get(ctx, "foreman:alpha", 10, 0)
		if err != nil {
			t.Fatalf("inbox.Get: %v", err)
		}
		for _, env := range envs {
			if env.Type == "job.request" && env.JobID == spec.ID && env.From == "factory:main" {
				return true
			}
		}
		return false
	})

	// started event, then done result - the consumer drives the job to
	// running then terminal.
	if status := e.postToFactoryMain(foremanBearer, wire.MessageEnvelope{
		Type: "job.event", JobID: spec.ID, From: "foreman:alpha",
		Payload: json.RawMessage(`{"event":"started"}`),
	}); status != http.StatusCreated {
		t.Fatalf("post job.event status = %d, want 201", status)
	}
	result := wire.JobResult{
		JobID: spec.ID, Status: wire.StatusDone, Summary: "all done",
		Result: json.RawMessage(`{"accuracy":0.9}`),
	}
	if status := e.postToFactoryMain(foremanBearer, wire.MessageEnvelope{
		Type: "job.result", JobID: spec.ID, From: "foreman:alpha",
		Payload: mustJSON(t, result),
	}); status != http.StatusCreated {
		t.Fatalf("post job.result status = %d, want 201", status)
	}

	var job jobs.Job
	waitFor(t, "job done with one result", 5*time.Second, func() bool {
		if status := e.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job); status != http.StatusOK {
			t.Fatalf("GET /v1/jobs/{id} status = %d", status)
		}
		return job.State == jobs.StateDone && len(parseResults(t, job.Results)) == 1
	})

	// Duplicate result post: the inbox dedupes (409) and the job keeps a
	// single result.
	dupEnv := wire.MessageEnvelope{
		Type: "job.result", JobID: spec.ID, From: "foreman:alpha",
		Payload: mustJSON(t, result),
	}
	if status := e.postToFactoryMain(foremanBearer, dupEnv); status != http.StatusConflict {
		t.Fatalf("duplicate result status = %d, want 409", status)
	}
	_ = e.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
	if n := len(parseResults(t, job.Results)); n != 1 {
		t.Fatalf("results after duplicate = %d, want 1", n)
	}

	// Consumer acked everything: factory:main has no unacked messages.
	waitFor(t, "factory:main drained", 5*time.Second, func() bool {
		envs, err := e.a.Inbox().Get(ctx, "factory:main", 50, 0)
		if err != nil {
			t.Fatalf("inbox.Get: %v", err)
		}
		return len(envs) == 0
	})
}

func TestOverBudget(t *testing.T) {
	e := newEnv(t)
	foremanBearer := e.pair("alpha", nil)
	ctx := context.Background()

	spec := wire.NewJobSpec("expensive task", "factory")
	spec.Budget.MaxCostUSD = 0.01
	if err := e.a.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := e.a.Jobs().Dispatch(ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if status := e.postToFactoryMain(foremanBearer, wire.MessageEnvelope{
		Type: "job.event", JobID: spec.ID, From: "foreman:alpha",
		Payload: json.RawMessage(`{"event":"started"}`),
	}); status != http.StatusCreated {
		t.Fatalf("post job.event status = %d", status)
	}
	result := wire.JobResult{
		JobID: spec.ID, Status: wire.StatusDone, Summary: "cost a lot",
		Usage: &wire.Usage{CostUSD: 1.0},
	}
	if status := e.postToFactoryMain(foremanBearer, wire.MessageEnvelope{
		Type: "job.result", JobID: spec.ID, From: "foreman:alpha",
		Payload: mustJSON(t, result),
	}); status != http.StatusCreated {
		t.Fatalf("post job.result status = %d", status)
	}

	var job jobs.Job
	waitFor(t, "job done and flagged over budget", 5*time.Second, func() bool {
		_ = e.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
		return job.State == jobs.StateDone && job.OverBudget
	})
}

// TestLoopKillDeadLetters: a harness loop kill (reason=loop) must not be
// retried - it dead-letters immediately, like a budget kill.
func TestLoopKillDeadLetters(t *testing.T) {
	e := newEnv(t)
	foremanBearer := e.pair("alpha", nil)
	ctx := context.Background()

	spec := wire.NewJobSpec("loops forever", "factory")
	if err := e.a.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := e.a.Jobs().Dispatch(ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if status := e.postToFactoryMain(foremanBearer, wire.MessageEnvelope{
		Type: "job.event", JobID: spec.ID, From: "foreman:alpha",
		Payload: json.RawMessage(`{"event":"started"}`),
	}); status != http.StatusCreated {
		t.Fatalf("post job.event status = %d", status)
	}
	result := wire.JobResult{
		JobID: spec.ID, Status: wire.StatusFailed, Summary: "loop guard",
		Error: &wire.JobError{Kind: wire.KindOther, Reason: wire.ReasonLoop, Message: "tool repeated"},
	}
	if status := e.postToFactoryMain(foremanBearer, wire.MessageEnvelope{
		Type: "job.result", JobID: spec.ID, From: "foreman:alpha",
		Payload: mustJSON(t, result),
	}); status != http.StatusCreated {
		t.Fatalf("post job.result status = %d", status)
	}
	var job jobs.Job
	waitFor(t, "loop-killed job dead-lettered (reason=loop, no retry)", 5*time.Second, func() bool {
		_ = e.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
		return job.State == jobs.StateDead
	})
	if !strings.Contains(string(job.Results), `"reason":"loop"`) {
		t.Errorf("results = %s, want reason=loop recorded", job.Results)
	}
}

func TestWorkerTokenInvalidation(t *testing.T) {
	e := newEnv(t)
	foremanBearer := e.pair("alpha", nil)
	ctx := context.Background()

	spec := wire.NewJobSpec("worker token test", "factory")
	if err := e.a.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := e.a.Jobs().Dispatch(ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	workerTok, err := e.a.Auth().MintWorkerToken(ctx, "foreman:alpha", spec.ID)
	if err != nil {
		t.Fatalf("MintWorkerToken: %v", err)
	}

	// Pre-terminal: the worker token reads its job inbox (200), then it is
	// invalidated on the terminal transition.
	jobInbox := "/v1/inboxes/job:" + spec.ID + "/messages"
	if status := e.do("GET", jobInbox, workerTok, nil, nil); status != http.StatusOK {
		t.Fatalf("job inbox read status = %d, want 200", status)
	}
	for _, env := range []wire.MessageEnvelope{
		{Type: "job.event", JobID: spec.ID, From: "foreman:alpha", Payload: json.RawMessage(`{"event":"started"}`)},
		{Type: "job.result", JobID: spec.ID, From: "foreman:alpha", Payload: mustJSON(t, wire.JobResult{
			JobID: spec.ID, Status: wire.StatusDone, Summary: "done",
		})},
	} {
		if status := e.postToFactoryMain(foremanBearer, env); status != http.StatusCreated {
			t.Fatalf("post %s status = %d", env.Type, status)
		}
	}
	var job jobs.Job
	waitFor(t, "job done and worker token revoked", 5*time.Second, func() bool {
		_ = e.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
		if job.State != jobs.StateDone {
			return false
		}
		_, _, err := e.a.Auth().Identify(ctx, workerTok)
		var ue *auth.UnauthorizedError
		return errors.As(err, &ue)
	})

	_, _, err = e.a.Auth().Identify(ctx, workerTok)
	var ue *auth.UnauthorizedError
	if !errors.As(err, &ue) {
		t.Fatalf("Identify(worker token) after done = %v, want *auth.UnauthorizedError", err)
	}
	if status := e.do("GET", jobInbox, workerTok, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("job inbox read status = %d, want 401", status)
	}
}

func TestForemanOffline(t *testing.T) {
	e := newEnv(t)
	_ = e.pair("alpha", nil)

	var foremen []jobs.Foreman
	_ = e.do("GET", "/v1/foremen", "", nil, &foremen)
	if len(foremen) != 1 || !foremen[0].Online {
		t.Fatalf("foremen = %v, want foreman:alpha online", foremen)
	}

	// Advance past the 90s offline threshold (spec §11) and let the
	// sweeper's next pump mark it offline.
	e.clock.Advance(91 * time.Second)
	waitFor(t, "foreman offline", 12*time.Second, func() bool {
		var foremen []jobs.Foreman
		_ = e.do("GET", "/v1/foremen", "", nil, &foremen)
		return len(foremen) == 1 && !foremen[0].Online
	})
}

// TestCapsRefreshKeepsForemanOnline: a connected-but-silent foreman goes
// offline after 90s (spec §6.3); its 60s capabilities push (spec §6.7)
// must revive the registry row - last_seen bumped, online again - so a
// healthy foreman never shows offline to the operator.
func TestCapsRefreshKeepsForemanOnline(t *testing.T) {
	e := newEnv(t)
	bearer := e.pair("alpha", map[string]string{"gpu": "true"})

	var before []jobs.Foreman
	_ = e.do("GET", "/v1/foremen", "", nil, &before)

	// 91s silent: the sweep marks it offline...
	e.clock.Advance(91 * time.Second)
	waitFor(t, "foreman offline after 91s of silence", 12*time.Second, func() bool {
		var foremen []jobs.Foreman
		_ = e.do("GET", "/v1/foremen", "", nil, &foremen)
		return len(foremen) == 1 && !foremen[0].Online
	})

	// ...and the capabilities push brings it back (204 NoContent).
	if status := e.do("POST", "/v1/foreman/capabilities", bearer,
		map[string]any{"capabilities": map[string]string{"gpu": "true"}}, nil); status != http.StatusNoContent {
		t.Fatalf("caps refresh status = %d, want 204", status)
	}
	var after []jobs.Foreman
	_ = e.do("GET", "/v1/foremen", "", nil, &after)
	if len(after) != 1 || !after[0].Online {
		t.Fatalf("foremen after refresh = %v, want foreman:alpha online", after)
	}
	if after[0].LastSeen == before[0].LastSeen {
		t.Errorf("last_seen = %s, want bumped from %s", after[0].LastSeen, before[0].LastSeen)
	}
}

// TestPendingJobDispatchesWhenForemanArrives: a job created while no
// foreman is registered stays pending (spec §6.7); the pending picker loop
// re-matches it once a foreman pairs, and the dispatch notifier delivers
// the job.request to the foreman inbox (spec §6.9).
func TestPendingJobDispatchesWhenForemanArrives(t *testing.T) {
	e := newEnv(t)

	// No foreman yet: create_job matches nothing and leaves the job
	// pending.
	var created struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if status := e.do("POST", "/v1/jobs", "", map[string]any{"task": "smoke test"}, &created); status != http.StatusCreated {
		t.Fatalf("POST /v1/jobs status = %d, want 201", status)
	}
	if created.State != jobs.StatePending {
		t.Fatalf("created state = %s, want pending (no foreman yet)", created.State)
	}

	// The foreman pairs (and pushes capabilities): the pending picker
	// dispatches the backlog within its 10s tick.
	e.pair("alpha", nil)

	waitFor(t, "pending job dispatched to foreman:alpha", 15*time.Second, func() bool {
		var job jobs.Job
		_ = e.do("GET", "/v1/jobs/"+created.JobID, "", nil, &job)
		return job.State == jobs.StateDispatched && job.ForemanID == "foreman:alpha"
	})

	// The notifier delivered the job.request envelope at-least-once.
	waitFor(t, "job.request in foreman inbox", 5*time.Second, func() bool {
		envs, err := e.a.Inbox().Get(e.ctx, "foreman:alpha", 10, 0)
		if err != nil {
			e.t.Fatalf("inbox.Get: %v", err)
		}
		for _, env := range envs {
			if env.Type == "job.request" && env.JobID == created.JobID {
				return true
			}
		}
		return false
	})
}

func TestEnvPlannerToken(t *testing.T) {
	const envTok = "env-supplied-planner-secret-0123456789"
	t.Setenv("FACTORY_PLANNER_TOKEN", envTok)
	e := newEnv(t)
	if got := e.a.PlannerToken(); got != envTok {
		t.Fatalf("PlannerToken() = %q, want the env value", got)
	}
	status, out := e.rpc("tools/list", nil)
	if status != http.StatusOK {
		t.Fatalf("tools/list with env planner token: status %d: %v", status, out)
	}
}

func TestStreamSmoke(t *testing.T) {
	e := newEnv(t)
	foremanBearer := e.pair("alpha", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/v1/inboxes/foreman:alpha/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+foremanBearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil && ctx.Err() == nil {
		t.Fatalf("stream request: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
}

// --- helpers ---

// isHex64 reports whether s is 64 lowercase hex chars (32B, spec §11).
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return json.RawMessage(b)
}

func parseResults(t *testing.T, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse results %s: %v", raw, err)
	}
	return out
}
