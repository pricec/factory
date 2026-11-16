package foremanapp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pricec/factory/internal/buildinfo"
	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

func TestIntakeGate(t *testing.T) {
	g := &intakeGate{}
	if busy, d := g.busy(); busy || d != "idle" {
		t.Fatalf("fresh gate busy=%v %q", busy, d)
	}
	if !g.beginDispatch() || !g.beginBrain() {
		t.Fatal("open gate refused work")
	}
	g.workerStarted()
	g.workerStarted()
	busy, d := g.busy()
	if !busy || d != "2 live worker(s), live brain session, 1 dispatch(es) in flight" {
		t.Fatalf("busy=%v %q", busy, d)
	}
	if g.resume() {
		t.Fatal("resume of an open gate reported deferrals")
	}
	g.pause()
	g.pause() // idempotent
	if !g.isPaused() || g.beginDispatch() || g.beginBrain() {
		t.Fatal("paused gate admitted work")
	}
	// Live work is never interrupted by pause and finishes normally.
	g.endDispatch()
	g.endBrain()
	g.workerDone()
	g.workerDone()
	if busy, _ := g.busy(); busy {
		t.Fatal("still busy after all work ended")
	}
	if !g.resume() {
		t.Fatal("resume must report the refused envelopes")
	}
	if g.isPaused() || g.resume() {
		t.Fatal("resume state")
	}
	g.pause()
	if g.resume() {
		t.Fatal("pause/resume with no refusals reported deferrals")
	}
}

// fakeFactory serves the factory endpoints the foreman calls in these
// tests and records them.
type fakeFactory struct {
	srv       *httptest.Server
	mu        sync.Mutex
	capsCode  int
	capsBody  string
	capsSeen  []map[string]string
	acked     []string
	posted    []wire.MessageEnvelope
	capsCalls atomic.Int32
}

func newFakeFactory(t *testing.T) *fakeFactory {
	t.Helper()
	f := &fakeFactory{capsCode: http.StatusNoContent}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/foreman/capabilities":
			f.capsCalls.Add(1)
			var in struct {
				Capabilities map[string]string `json:"capabilities"`
			}
			_ = json.Unmarshal(body, &in)
			f.capsSeen = append(f.capsSeen, in.Capabilities)
			if f.capsBody != "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(f.capsCode)
			io.WriteString(w, f.capsBody)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs":
			// Startup orphan recovery (spec §8.7) lists running jobs
			// before the first caps push: nothing is running here.
			io.WriteString(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/ack"):
			var in struct {
				IDs []string `json:"ids"`
			}
			_ = json.Unmarshal(body, &in)
			f.acked = append(f.acked, in.IDs...)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/messages") && r.Method == http.MethodPost:
			var env wire.MessageEnvelope
			_ = json.Unmarshal(body, &env)
			f.posted = append(f.posted, env)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFactory) setCaps(code int, body string) {
	f.mu.Lock()
	f.capsCode, f.capsBody = code, body
	f.mu.Unlock()
}

// newDrainApp builds a real App against the fake factory, pre-paired via
// paired.json (no pairing round trip).
func newDrainApp(t *testing.T, f *fakeFactory, build string) *App {
	t.Helper()
	dir := t.TempDir()
	rec, _ := json.Marshal(pairingRecord{Bearer: "paired-bearer", InboxID: "foreman:alpha", ParentURL: f.srv.URL})
	if err := os.WriteFile(filepath.Join(dir, pairedFileName), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{FactoryURL: f.srv.URL, ID: "alpha", DataDir: dir, Addr: "127.0.0.1:0", Build: build})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestAppBuildDefaultsToBuildinfo(t *testing.T) {
	f := newFakeFactory(t)
	a := newDrainApp(t, f, "")
	if a.Build() != buildinfo.Current() || a.Updater().Running != a.Build() {
		t.Fatalf("build %q, updater %q, want %q", a.Build(), a.Updater().Running, buildinfo.Current())
	}
	b := newDrainApp(t, f, buildNew)
	if b.Build() != buildNew || b.Updater().Running != buildNew || b.Updater().DataDir == "" ||
		b.Updater().Platform == "" || b.Updater().Bearer != "paired-bearer" {
		t.Fatalf("updater wiring %+v", b.Updater())
	}
}

func TestPushCapabilitiesBuildAndAdvert(t *testing.T) {
	f := newFakeFactory(t)
	a := newDrainApp(t, f, buildOld)
	ctx := context.Background()

	advert := `{"foreman_update":{"build":"` + buildNew + `","platforms":{"linux-amd64":{"sha256":"ab","path":"/download/foreman/foreman-linux-amd64"}}}}`
	f.setCaps(http.StatusOK, advert)
	if err := a.pushCapabilities(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}
	if adv := a.updater.currentAdvert(); adv == nil || adv.Build != buildNew {
		t.Fatalf("advert = %+v", adv)
	}
	f.mu.Lock()
	caps := f.capsSeen[0]
	f.mu.Unlock()
	if caps["build"] != buildOld || caps["os"] == "" {
		t.Fatalf("caps = %v, want build %s", caps, buildOld)
	}

	// 204: nothing advertised any more.
	f.setCaps(http.StatusNoContent, "")
	if err := a.pushCapabilities(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}
	if adv := a.updater.currentAdvert(); adv != nil {
		t.Fatalf("204 kept advert %+v", adv)
	}

	// Malformed 200: logged, treated as nothing advertised, push succeeds.
	f.setCaps(http.StatusOK, advert)
	_ = a.pushCapabilities(ctx)
	f.setCaps(http.StatusOK, `{"foreman_update":`)
	if err := a.pushCapabilities(ctx); err != nil {
		t.Fatalf("malformed body failed the push: %v", err)
	}
	if adv := a.updater.currentAdvert(); adv != nil {
		t.Fatal("malformed body kept an advert")
	}

	// Failed push: error, last advert kept.
	f.setCaps(http.StatusOK, advert)
	_ = a.pushCapabilities(ctx)
	f.setCaps(http.StatusInternalServerError, "boom")
	if err := a.pushCapabilities(ctx); err == nil {
		t.Fatal("500 push succeeded")
	}
	if adv := a.updater.currentAdvert(); adv == nil {
		t.Fatal("failed push dropped the advert")
	}
}

type countingSpawner struct {
	mu    sync.Mutex
	specs []wire.JobSpec
	busy  []bool
	gate  *intakeGate
}

func (s *countingSpawner) Spawn(_ context.Context, spec wire.JobSpec) error {
	busy, _ := s.gate.busy()
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	s.busy = append(s.busy, busy)
	s.mu.Unlock()
	return nil
}

func jobRequest(t *testing.T, id string) wire.MessageEnvelope {
	t.Helper()
	spec := wire.NewJobSpec("do it", "factory:main")
	payload, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return wire.MessageEnvelope{ID: id, Type: "job.request", JobID: spec.ID, From: "factory:main", Payload: payload}
}

func TestHandleBatchDefersDispatchWhileDraining(t *testing.T) {
	f := newFakeFactory(t)
	a := newDrainApp(t, f, buildOld)
	sp := &countingSpawner{gate: a.gate}
	a.router.Spawner = sp
	ctx := context.Background()
	var acked []string
	ack := func(_ context.Context, ids []string) error { acked = append(acked, ids...); return nil }

	evt := wire.MessageEnvelope{ID: "msg_evt", Type: "job.event", JobID: "job_x", From: "foreman:alpha", Payload: []byte(`{"event":"started"}`)}
	a.gate.pause()
	if err := a.handleBatch(ctx, []wire.MessageEnvelope{jobRequest(t, "msg_req"), evt}, ack); err != nil {
		t.Fatal(err)
	}
	if len(sp.specs) != 0 {
		t.Fatal("dispatched while draining")
	}
	if len(acked) != 1 || acked[0] != "msg_evt" {
		t.Fatalf("acked %v: the deferred job.request must stay unacked, other traffic flows", acked)
	}

	a.resumeIntake() // deferred -> remote resync requested
	acked = nil
	if err := a.handleBatch(ctx, []wire.MessageEnvelope{jobRequest(t, "msg_req")}, ack); err != nil {
		t.Fatal(err)
	}
	if len(sp.specs) != 1 || len(acked) != 1 || acked[0] != "msg_req" {
		t.Fatalf("after resume: spawned %d, acked %v", len(sp.specs), acked)
	}
	if !sp.busy[0] {
		t.Fatal("an admitted dispatch must count as live work until its spawn returns")
	}
	if busy, _ := a.gate.busy(); busy {
		t.Fatal("dispatch count leaked")
	}

	// A failed spawn (logged and acked by the router, unchanged) still
	// releases its dispatch count.
	a.router.Spawner = spawnerFunc(func(context.Context, wire.JobSpec) error { return io.ErrUnexpectedEOF })
	_ = a.handleBatch(ctx, []wire.MessageEnvelope{jobRequest(t, "msg_bad")}, ack)
	if busy, _ := a.gate.busy(); busy {
		t.Fatal("failed dispatch count leaked")
	}
}

func TestBrainQueueGatedWhileDraining(t *testing.T) {
	f := newFakeFactory(t)
	a := newDrainApp(t, f, buildOld)
	var sessions atomic.Int32
	var busyInSession atomic.Bool
	a.brain.Exec = func(ctx context.Context, prompt string) (string, error) {
		busy, _ := a.gate.busy()
		busyInSession.Store(busy)
		sessions.Add(1)
		return "ok", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := a.inbox.Post(ctx, a.brainInboxID, wire.MessageEnvelope{Type: "brain.wake", From: "factory:main", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	a.gate.pause()
	done := make(chan struct{})
	go func() { a.brainQueueLoop(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	if sessions.Load() != 0 {
		t.Fatal("brain session started while draining")
	}
	a.resumeIntake()
	waitUntil(t, 5*time.Second, func() bool { return sessions.Load() == 1 })
	if !busyInSession.Load() {
		t.Fatal("a live brain session must count as busy")
	}
	waitUntil(t, 5*time.Second, func() bool { busy, _ := a.gate.busy(); return !busy })
	cancel()
	<-done
}

func TestLocalLoopBacksOffWhileDraining(t *testing.T) {
	f := newFakeFactory(t)
	a := newDrainApp(t, f, buildOld)
	sp := &countingSpawner{gate: a.gate}
	a.router.Spawner = sp
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := jobRequest(t, "")
	req.ID = ""
	if _, err := a.inbox.Post(ctx, a.inboxID, req); err != nil {
		t.Fatal(err)
	}
	a.gate.pause()
	done := make(chan struct{})
	go func() { a.localLoop(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	if len(sp.specs) != 0 {
		t.Fatal("local job.request dispatched while draining")
	}
	a.gate.mu.Lock()
	refusals := a.gate.deferred
	a.gate.mu.Unlock()
	if refusals > 3 {
		t.Fatalf("local loop spun: %d refusals in 300ms", refusals)
	}
	a.resumeIntake()
	waitUntil(t, 5*time.Second, func() bool { sp.mu.Lock(); defer sp.mu.Unlock(); return len(sp.specs) == 1 })
	cancel()
	<-done
}

func TestSpawnWorkerCountsLiveWorker(t *testing.T) {
	f := newFakeFactory(t)
	a := newDrainApp(t, f, buildOld)
	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 0.4\nprintf done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := a.BuildHarnessConfig
	a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
		c := orig(spec, bearer)
		c.OpenCODEBin = bin
		c.Timeout = 10 * time.Second
		c.Post = func(context.Context, string, string, wire.JobResult) error { return nil }
		return c
	}
	if err := a.SpawnWorker(context.Background(), wire.NewJobSpec("x", "factory:main")); err != nil {
		t.Fatal(err)
	}
	if busy, d := a.gate.busy(); !busy || !strings.Contains(d, "1 live worker") {
		t.Fatalf("after spawn busy=%v %q", busy, d)
	}
	waitUntil(t, 8*time.Second, func() bool { busy, _ := a.gate.busy(); return !busy })
}

func TestSelfCheck(t *testing.T) {
	f := newFakeFactory(t)
	a := newDrainApp(t, f, buildOld)
	ctx := context.Background()
	if err := a.selfCheck(ctx); err == nil || !strings.Contains(err.Error(), "listener not up") {
		t.Fatalf("no listener: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	a.MarkListening(ln.Addr().String())
	if err := a.selfCheck(ctx); err != nil {
		t.Fatalf("listening: %v", err)
	}
	a.MarkListening("[::]:" + port) // unspecified host -> loopback
	if err := a.selfCheck(ctx); err != nil {
		t.Fatalf("unspecified host: %v", err)
	}
	a.MarkListening(":" + port)
	if err := a.selfCheck(ctx); err != nil {
		t.Fatalf("empty host: %v", err)
	}
	a.MarkListening("nonsense")
	if err := a.selfCheck(ctx); err == nil {
		t.Fatal("bad address passed")
	}
	ln.Close()
	a.MarkListening("127.0.0.1:" + port)
	if err := a.selfCheck(ctx); err == nil {
		t.Fatal("closed listener passed")
	}
	_ = a.Close()
	if err := a.selfCheck(ctx); err == nil {
		t.Fatal("closed stores passed")
	}
}

func TestRequestExitFirstWins(t *testing.T) {
	f := newFakeFactory(t)
	a := newDrainApp(t, f, buildOld)
	a.requestExit(0)
	a.requestExit(1)
	select {
	case code := <-a.ExitRequested():
		if code != 0 {
			t.Fatalf("code %d", code)
		}
	default:
		t.Fatal("no exit requested")
	}
	select {
	case code := <-a.ExitRequested():
		t.Fatalf("second exit delivered: %d", code)
	default:
	}
}

// TestRunConfirmsPendingUpdate: a swapped build (marker.to == own build)
// is confirmed by Run's first successful caps push once the listener is up.
func TestRunConfirmsPendingUpdate(t *testing.T) {
	f := newFakeFactory(t)
	a := newDrainApp(t, f, buildNew)
	bin := filepath.Join(a.dataDir, updateBinDir)
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(updateMarker{From: buildOld, To: buildNew, Attempts: 1})
	os.WriteFile(filepath.Join(bin, updateMarkerName), marker, 0o644)
	os.WriteFile(filepath.Join(bin, updateBootsName), []byte("1"), 0o644)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a.MarkListening(ln.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	waitUntil(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(bin, updateMarkerName))
		return os.IsNotExist(err)
	})
	if a.Updater().Pending() {
		t.Fatal("still pending after confirm")
	}
	if _, err := os.Stat(filepath.Join(bin, updateBootsName)); !os.IsNotExist(err) {
		t.Fatalf("update-boots not deleted: %v", err)
	}
	cancel()
	<-done
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
