// Package e2e_test holds the black-box end-to-end suite (issue #22): the
// REAL factoryapp and foremanapp booted over httptest servers and driven
// entirely through their public HTTP and MCP surfaces. Nothing is
// reimplemented, and no real opencode ever runs - the worker executes a
// fake shell-script opencode binary written to a temp dir.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/budgets"
	"github.com/pricec/factory/internal/factoryapp"
	"github.com/pricec/factory/internal/foremanapp"
	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

// Pinned e2e identities and scripted outputs.
const (
	// foremanID is the bare foreman id; foremanInbox is its full inbox id.
	foremanID    = "alpha"
	foremanInbox = "foreman:" + foremanID
	// fakeSummary is the fake opencode's scripted stdout; the harness
	// fallback turns it into the job result summary (spec §8.5).
	fakeSummary = "e2e: all systems nominal"
)

// stack is the booted stack: real factory + real foreman + fake opencode,
// each on a 127.0.0.1 ephemeral port.
type stack struct {
	t              *testing.T
	factory        *factoryapp.App
	factorySrv     *httptest.Server
	foreman        *foremanapp.App
	foremanSrv     *httptest.Server
	fakeOpencode   string
	foremanDataDir string
	ctx            context.Context
	cancel         func()

	// workerBearers records the per-job worker token the spawner minted
	// (captured via the BuildHarnessConfig override seam), for the
	// terminal-revocation assert in the happy path.
	mu            sync.Mutex
	workerBearers map[string]string
	// harnessModels records the per-job model the spawner resolved
	// (captured via the same override seam), for the model-override
	// e2e scenario.
	harnessModels map[string]string

	// happyJobID/happyTask carry scenario 1's finished job into the
	// scenario 4 summary-view subtest.
	happyJobID string
	happyTask  string
}

// boot wires the whole stack: the factory on a temp dir + httptest server,
// the foreman paired to it over the real /v1/pair surface, the worker
// harness pointed at the fake opencode binary and the foreman's own test
// server, and both apps' Run loops started. Cleanup runs in reverse order:
// cancel the loops, close the servers, close the stores.
func boot(t *testing.T) *stack {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &stack{t: t, ctx: ctx, cancel: cancel, workerBearers: map[string]string{}, harnessModels: map[string]string{}}

	fac, err := factoryapp.New(factoryapp.Config{
		DataDir:      t.TempDir(),
		Addr:         "127.0.0.1:0",
		AdvertiseURL: "http://factory.test",
		Budget:       budgets.DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("factoryapp.New: %v", err)
	}
	s.factory = fac
	s.factorySrv = httptest.NewServer(fac.Handler())

	// Fake opencode: prints the scripted summary and exits 0. It never
	// calls report_result, so the harness posts the fallback result
	// (spec §8.5); stdout becomes the summary verbatim.
	s.fakeOpencode = filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/sh\nprintf '" + fakeSummary + "'\n"
	if err := os.WriteFile(s.fakeOpencode, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}

	// Real pairing (spec §7.1): the factory issues the one-time token,
	// the foreman burns it over HTTP.
	pairTok, _, err := fac.Auth().IssuePairingToken(ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	s.foremanDataDir = t.TempDir()
	fore, err := foremanapp.New(foremanapp.Config{
		FactoryURL:   s.factorySrv.URL,
		ID:           foremanID,
		DataDir:      s.foremanDataDir,
		Addr:         "127.0.0.1:0",
		PairingToken: pairTok,
		// Fast settings pulls so the SettingsPanel scenario does not
		// wait a full 60s refresh (spec §6.5: production default 60s).
		SettingsRefresh: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("foremanapp.New: %v", err)
	}
	s.foreman = fore
	s.foremanSrv = httptest.NewServer(fore.Handler())

	// Brain stub: scenario 2's ask_parent routes job.question to the
	// brain; never let a real opencode session spawn for it.
	fore.Brain().Exec = func(context.Context, string) (string, error) {
		return "e2e brain stub: no action", nil
	}

	// Harness override (the foremanapp_test pattern): the fake opencode
	// binary, loopback URLs at the foreman's real test server, and the
	// spawner-minted worker bearer captured for the revocation assert.
	orig := fore.BuildHarnessConfig
	fore.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		s.mu.Lock()
		s.workerBearers[spec.ID] = bearer
		s.mu.Unlock()
		c := orig(spec, bearer)
		s.mu.Lock()
		s.harnessModels[spec.ID] = c.Model
		s.mu.Unlock()
		c.OpenCODEBin = s.fakeOpencode
		c.ResultURL = s.foremanSrv.URL + "/v1/inboxes/" + foremanInbox + "/messages"
		c.MCPURL = s.foremanSrv.URL + "/v1/mcp/worker"
		c.Timeout = 10 * time.Second
		c.PostRetryBackoff = 10 * time.Millisecond
		return c
	}

	go func() { _ = fac.Run(ctx) }()
	go func() { _ = fore.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		s.foremanSrv.Close()
		s.factorySrv.Close()
		_ = fore.Close()
		_ = fac.Close()
	})
	return s
}

// MCP endpoint bases for the three surfaces.
func (s *stack) factoryMCP() string { return s.factorySrv.URL + "/v1/mcp/factory" }
func (s *stack) workerMCP() string  { return s.foremanSrv.URL + "/v1/mcp/worker" }
func (s *stack) brainMCP() string   { return s.foremanSrv.URL + "/v1/mcp/foreman" }

// req issues one JSON HTTP request against base+path. A string body is
// sent verbatim; anything else is marshaled.
func (s *stack) req(base, method, path, bearer string, in, out any) int {
	s.t.Helper()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			s.t.Fatalf("marshal request: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		s.t.Fatalf("NewRequest %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			s.t.Fatalf("decode %s %s response: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

// do issues one JSON HTTP request against the factory's server.
func (s *stack) do(method, path, bearer string, in, out any) int {
	s.t.Helper()
	return s.req(s.factorySrv.URL, method, path, bearer, in, out)
}

// rpc posts one MCP JSON-RPC request (the raw-JSON client pattern every
// internal MCP test uses) and returns status + decoded body.
func (s *stack) rpc(base, bearer, method string, params any) (int, map[string]any) {
	s.t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	b, err := json.Marshal(body)
	if err != nil {
		s.t.Fatalf("marshal rpc: %v", err)
	}
	req, err := http.NewRequest("POST", base, bytes.NewReader(b))
	if err != nil {
		s.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("POST %s: %v", method, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		s.t.Fatalf("decode %s response: %v", method, err)
	}
	return resp.StatusCode, out
}

// callTool calls one MCP tool and returns its structured content.
func (s *stack) callTool(base, bearer, name string, args map[string]any) map[string]any {
	s.t.Helper()
	sc := s.callToolRaw(base, bearer, name, args)
	m, ok := sc.(map[string]any)
	if !ok {
		s.t.Fatalf("tools/call %s: structuredContent not an object: %v", name, sc)
	}
	return m
}

// callToolSlice calls one MCP tool whose structured content is a record
// wrapping an array field ({"jobs": [...]}, {"foremen": [...]}): MCP
// structured content must be an object - client harnesses reject bare
// arrays.
func (s *stack) callToolSlice(base, bearer, name, key string, args map[string]any) []map[string]any {
	s.t.Helper()
	sc := s.callToolRaw(base, bearer, name, args)
	rec, ok := sc.(map[string]any)
	if !ok {
		s.t.Fatalf("tools/call %s: structuredContent not a record: %v", name, sc)
	}
	raw, err := json.Marshal(rec[key])
	if err != nil {
		s.t.Fatalf("marshal %s: %v", key, err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		s.t.Fatalf("tools/call %s: structuredContent[%q] not a JSON array: %v", name, key, err)
	}
	return out
}

// callToolRaw calls one MCP tool and returns its structured content
// untyped (object or array).
func (s *stack) callToolRaw(base, bearer, name string, args map[string]any) any {
	s.t.Helper()
	status, out := s.rpc(base, bearer, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if status != http.StatusOK {
		s.t.Fatalf("tools/call %s status = %d, want 200: %v", name, status, out)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("tools/call %s: no result object: %v", name, out)
	}
	if isErr, _ := res["isError"].(bool); isErr {
		s.t.Fatalf("tools/call %s: tool error: %v", name, res)
	}
	sc, ok := res["structuredContent"]
	if !ok {
		s.t.Fatalf("tools/call %s: no structuredContent: %v", name, res)
	}
	return sc
}

// workerBearer returns the spawner-minted worker bearer for jobID.
func (s *stack) workerBearer(jobID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workerBearers[jobID]
}

// harnessModel returns the model the spawner resolved for jobID (empty
// until the foreman has consumed the job.request).
func (s *stack) harnessModel(jobID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.harnessModels[jobID]
}

// waitFor polls cond every 50ms until it holds or the deadline lapses.
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

// mustJSON marshals v or fails the test.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return json.RawMessage(b)
}
