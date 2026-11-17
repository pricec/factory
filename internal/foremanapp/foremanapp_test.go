package foremanapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/factoryapp"
	"github.com/pricec/factory/internal/foremanapp"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

// fakeClock: frozen clock so the factory's liveness/sweeper logic stays
// deterministic (the foreman's own loops are wall-clock-driven).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// factoryEnv boots a REAL factoryapp over an httptest server.
type factoryEnv struct {
	t      *testing.T
	a      *factoryapp.App
	ts     *httptest.Server
	ctx    context.Context
	cancel context.CancelFunc
}

func newFactory(t *testing.T) *factoryEnv {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	a, err := factoryapp.New(factoryapp.Config{
		DataDir:      t.TempDir(),
		Addr:         "127.0.0.1:0",
		AdvertiseURL: "http://factory.test",
		Now:          clock.Now,
	})
	if err != nil {
		t.Fatalf("factoryapp.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ts := httptest.NewServer(a.Handler())
	go a.Run(ctx)
	t.Cleanup(func() {
		cancel()
		ts.Close()
		_ = a.Close()
	})
	return &factoryEnv{t: t, a: a, ts: ts, ctx: ctx, cancel: cancel}
}

func (e *factoryEnv) do(method, path, bearer string, in, out any) int {
	e.t.Helper()
	var body io.Reader
	if in != nil {
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

// foremanEnv boots a REAL foremanapp over an httptest server, wired at the
// real factory's test server URL. opts mutate the Config before New.
type foremanEnv struct {
	t      *testing.T
	a      *foremanapp.App
	ts     *httptest.Server
	ctx    context.Context
	cancel context.CancelFunc
}

func newForeman(t *testing.T, factory *factoryEnv, pairingToken string, opts ...func(*foremanapp.Config)) *foremanEnv {
	t.Helper()
	cfg := foremanapp.Config{
		FactoryURL:   factory.ts.URL,
		ID:           "alpha",
		DataDir:      t.TempDir(),
		Addr:         "127.0.0.1:0",
		PairingToken: pairingToken,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	a, err := foremanapp.New(cfg)
	if err != nil {
		t.Fatalf("foremanapp.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ts := httptest.NewServer(a.Handler())
	go a.Run(ctx)
	t.Cleanup(func() {
		cancel()
		ts.Close()
		_ = a.Close()
	})
	return &foremanEnv{t: t, a: a, ts: ts, ctx: ctx, cancel: cancel}
}

func (e *foremanEnv) do(method, path, bearer string, in, out any) int {
	e.t.Helper()
	var body io.Reader
	if in != nil {
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

// fakeOpencode writes a fake `opencode` shell script named "opencode" into a
// fresh temp dir and returns the binary path (the workerharness test
// pattern: injected via the Config OpenCODEBin seam; no real opencode ever
// runs).
func fakeOpencode(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	path := filepath.Join(dir, "opencode")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	return path
}

func TestPairFlow(t *testing.T) {
	fac := newFactory(t)

	// No token and no stored pairing -> New fails.
	if _, err := foremanapp.New(foremanapp.Config{
		FactoryURL: fac.ts.URL,
		ID:         "alpha",
		DataDir:    t.TempDir(),
		Addr:       "127.0.0.1:0",
	}); err == nil || !strings.Contains(err.Error(), "not paired") {
		t.Fatalf("New without pairing = %v, want not paired error", err)
	}

	// Real pair flow: factory issues the one-time token, the foreman burns
	// it over the real HTTP surface.
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	forEnv := newForeman(t, fac, token)
	if !forEnv.a.Paired() {
		t.Fatal("foreman not paired after New")
	}

	// The factory registry shows the foreman online with the detected caps.
	var foremen []jobs.Foreman
	waitFor(t, "foreman registered", 5*time.Second, func() bool {
		_ = fac.do("GET", "/v1/foremen", "", nil, &foremen)
		return len(foremen) == 1
	})
	f := foremen[0]
	if f.ID != "foreman:alpha" || !f.Online {
		t.Fatalf("foreman row = %+v, want foreman:alpha online", f)
	}
	var caps map[string]string
	if err := json.Unmarshal([]byte(f.Capabilities), &caps); err != nil {
		t.Fatalf("capabilities passthrough: %v", err)
	}
	if caps["os"] == "" || caps["arch"] == "" {
		t.Errorf("capabilities = %v, want os/arch present", caps)
	}

	// paired.json persists: a restart New without a token loads it.
	token2, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	dataDir := t.TempDir()
	a1, err := foremanapp.New(foremanapp.Config{
		FactoryURL:   fac.ts.URL,
		ID:           "alpha",
		DataDir:      dataDir,
		Addr:         "127.0.0.1:0",
		PairingToken: token2,
	})
	if err != nil {
		t.Fatalf("restart New: %v", err)
	}
	defer func() { _ = a1.Close() }()
	a2, err := foremanapp.New(foremanapp.Config{
		FactoryURL: fac.ts.URL,
		ID:         "alpha",
		DataDir:    dataDir,
		Addr:       "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New after restart without token: %v", err)
	}
	defer func() { _ = a2.Close() }()
	if !a2.Paired() {
		t.Fatal("foreman not paired after restart from paired.json")
	}
}

func TestWorkerFlowEndToEnd(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}

	// Fake opencode: fixed stdout, exit 0, never calls report_result, so
	// the harness posts the fallback result over real HTTP.
	fakeBin := fakeOpencode(t, `printf 'fake worker summary'`)

	var workerBearer string
	var harnessMut sync.Mutex
	forEnv := newForeman(t, fac, token)
	orig := forEnv.a.BuildHarnessConfig
	forEnv.a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		harnessMut.Lock()
		workerBearer = bearer
		harnessMut.Unlock()
		c := orig(spec, bearer)
		c.OpenCODEBin = fakeBin
		c.ResultURL = forEnv.ts.URL + "/v1/inboxes/foreman:alpha/messages"
		c.MCPURL = forEnv.ts.URL + "/v1/mcp/worker"
		c.Timeout = 10 * time.Second
		c.PostRetryBackoff = 10 * time.Millisecond
		return c
	}

	// Create + dispatch a real job at the factory, to foreman:alpha.
	spec := wire.NewJobSpec("count the widgets", "factory")
	ctx := context.Background()
	if err := fac.a.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := fac.a.Jobs().Dispatch(ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// The factory notifier posts job.request; the foreman client loop
	// routes it; the spawner mints a token and runs the harness with the
	// fake binary; the fallback result posts to the foreman inbox; the
	// local consumer forwards it byte-identical to factory:main; the
	// factory consumer applies it. Poll the job record until terminal.
	var job jobs.Job
	waitFor(t, "job done", 8*time.Second, func() bool {
		_ = fac.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
		return job.State == jobs.StateDone
	})
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s", job.State)
	}
	var results []wire.JobResult
	if err := json.Unmarshal(job.Results, &results); err != nil || len(results) != 1 {
		t.Fatalf("results = %s (err %v), want exactly one", job.Results, err)
	}
	if results[0].Summary != "fake worker summary" {
		t.Errorf("summary = %q, want fake stdout", results[0].Summary)
	}
	if results[0].Status != wire.StatusDone {
		t.Errorf("status = %s, want done", results[0].Status)
	}

	// The per-job worker credential was revoked once the terminal result
	// passed through the foreman inbox loop (spec §7.4).
	waitFor(t, "worker token revoked", 5*time.Second, func() bool {
		harnessMut.Lock()
		bearer := workerBearer
		harnessMut.Unlock()
		if bearer == "" {
			return false
		}
		_, _, err := forEnv.a.Auth().Identify(ctx, bearer)
		var ue *auth.UnauthorizedError
		return err != nil && errors.As(err, &ue)
	})

	// The factory consumer acked the forwarded result.
	waitFor(t, "factory:main drained", 5*time.Second, func() bool {
		envs, err := fac.a.Inbox().Get(ctx, "factory:main", 50, 0)
		if err != nil {
			t.Fatalf("factory inbox get: %v", err)
		}
		return len(envs) == 0
	})
}

func TestBrainWake(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	forEnv := newForeman(t, fac, token)

	// Brain seam: replace the Exec with a recorder; no real opencode.
	var mut sync.Mutex
	var prompts []string
	forEnv.a.Brain().Exec = func(ctx context.Context, prompt string) (string, error) {
		mut.Lock()
		prompts = append(prompts, prompt)
		mut.Unlock()
		return "brain says hi", nil
	}

	// Post a brain.wake envelope to the foreman's LOCAL inbox over its
	// real HTTP mux, authed with a local foreman credential minted at New
	// (a local PairForeman on the foreman's own auth store: read+post on
	// its own inbox).
	env := wire.MessageEnvelope{
		Type:    "brain.wake",
		From:    "factory:main",
		Payload: json.RawMessage(`{"reason":"test"}`),
	}
	if status := forEnv.do("POST", "/v1/inboxes/foreman:alpha/messages",
		forEnv.a.BrainToken(), env, nil); status != http.StatusCreated {
		t.Fatalf("post brain.wake status = %d, want 201", status)
	}

	// The local consumer routed it to the brain.
	waitFor(t, "brain exec recorded", 5*time.Second, func() bool {
		mut.Lock()
		defer mut.Unlock()
		return len(prompts) > 0
	})
	mut.Lock()
	prompt := prompts[0]
	mut.Unlock()
	if !strings.Contains(prompt, "brain.wake") || !strings.Contains(prompt, "reason") {
		t.Errorf("brain prompt = %q, want the envelope rendered", prompt)
	}

	// The loop acked it: the local inbox has no unacked messages left.
	waitFor(t, "local inbox drained", 5*time.Second, func() bool {
		envs, err := forEnv.a.Inbox().Get(context.Background(), "foreman:alpha", 50, 0)
		if err != nil {
			t.Fatalf("foreman inbox get: %v", err)
		}
		return len(envs) == 0
	})

	// And the remote batch path acks via the factory REST: post a
	// job.event through the factory's foreman inbox and watch the
	// factory-side snapshot drain (the routing table drops job.event;
	// the remote loop still acks it).
	if _, err := fac.a.Inbox().Post(context.Background(), "foreman:alpha", wire.MessageEnvelope{
		Type: "job.event", JobID: "job_test", From: "factory:main",
		Payload: json.RawMessage(`{"event":"noop"}`),
	}); err != nil {
		t.Fatalf("post job.event to factory foreman inbox: %v", err)
	}
	waitFor(t, "factory-side foreman inbox drained", 5*time.Second, func() bool {
		envs, err := fac.a.Inbox().Get(context.Background(), "foreman:alpha", 50, 0)
		if err != nil {
			t.Fatalf("factory inbox get: %v", err)
		}
		return len(envs) == 0
	})
}
