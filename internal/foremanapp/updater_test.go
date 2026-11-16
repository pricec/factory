package foremanapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	buildOld   = "20261116T120000Z-aaaaaaaaaaaa"
	buildNew   = "20261116T141100Z-38a6368abcde"
	buildNewer = "20261117T000000Z-bbbbbbbbbbbb"
	buildMid   = "20261116T130000Z-cccccccccccc"
	testPlat   = "linux-amd64"
)

var (
	oldBin = []byte("#!old foreman binary\n")
	newBin = []byte("#!new foreman binary, a bit longer\n")
)

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// updHarness is a fully faked Updater environment: a temp data dir laid
// out per C5, a download server, a fake clock, recorded exits/logs and a
// fake liveness/gate.
type updHarness struct {
	t       *testing.T
	u       *Updater
	data    string
	srv     *httptest.Server
	hits    atomic.Int32
	body    atomic.Value // []byte served
	handler atomic.Value // optional http.HandlerFunc override
	auth    atomic.Value // last Authorization header

	mu       sync.Mutex
	now      time.Time
	exits    []int
	logs     []string
	busy     bool
	paused   bool
	pauses   int
	resumes  int
	selfErr  error
	renameFn func(o, n string) error
	openFn   func(name string) error
}

func newUpdHarness(t *testing.T) *updHarness {
	t.Helper()
	h := &updHarness{t: t, data: t.TempDir(), now: time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC)}
	h.body.Store(newBin)
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		h.auth.Store(r.Header.Get("Authorization"))
		if f, ok := h.handler.Load().(http.HandlerFunc); ok && f != nil {
			f(w, r)
			return
		}
		if r.URL.Path != "/download/foreman/foreman-"+testPlat {
			http.NotFound(w, r)
			return
		}
		w.Write(h.body.Load().([]byte))
	}))
	t.Cleanup(h.srv.Close)
	if err := os.MkdirAll(filepath.Join(h.data, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.write("foreman", oldBin)
	live := filepath.Join(h.data, "bin", "foreman")
	h.u = &Updater{
		DataDir:    h.data,
		Running:    buildOld,
		Platform:   testPlat,
		FactoryURL: h.srv.URL,
		Bearer:     "tok",
		Now:        h.clock,
		Executable: func() (string, error) { return live, nil },
		Exit: func(code int) {
			h.mu.Lock()
			h.exits = append(h.exits, code)
			h.mu.Unlock()
		},
		Busy: func() (bool, string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.busy {
				return true, "1 live worker(s)"
			}
			return false, "idle"
		},
		Pause: func() {
			h.mu.Lock()
			h.paused = true
			h.pauses++
			h.mu.Unlock()
		},
		Resume: func() {
			h.mu.Lock()
			h.paused = false
			h.resumes++
			h.mu.Unlock()
		},
		SelfCheck: func(context.Context) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.selfErr
		},
		Rename: func(o, n string) error {
			h.mu.Lock()
			fn := h.renameFn
			h.mu.Unlock()
			if fn != nil {
				if err := fn(o, n); err != nil {
					return err
				}
			}
			return os.Rename(o, n)
		},
		OpenFile: func(name string, flag int, perm os.FileMode) (*os.File, error) {
			h.mu.Lock()
			fn := h.openFn
			h.mu.Unlock()
			if fn != nil {
				if err := fn(name); err != nil {
					return nil, err
				}
			}
			return os.OpenFile(name, flag, perm)
		},
		Logf: func(format string, args ...any) {
			h.mu.Lock()
			h.logs = append(h.logs, fmt.Sprintf(format, args...))
			h.mu.Unlock()
		},
	}
	return h
}

func (h *updHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *updHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *updHarness) setBusy(b bool) {
	h.mu.Lock()
	h.busy = b
	h.mu.Unlock()
}

func (h *updHarness) path(name string) string { return filepath.Join(h.data, "bin", name) }

func (h *updHarness) write(name string, data []byte) {
	h.t.Helper()
	if err := os.WriteFile(h.path(name), data, 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *updHarness) read(name string) []byte {
	h.t.Helper()
	data, err := os.ReadFile(h.path(name))
	if err != nil {
		h.t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func (h *updHarness) exists(name string) bool {
	_, err := os.Stat(h.path(name))
	return err == nil
}

func (h *updHarness) advert(build string) *UpdateAdvert {
	return &UpdateAdvert{Build: build, Platforms: map[string]UpdatePlatform{
		testPlat: {SHA256: shaHex(newBin), Path: "/download/foreman/foreman-" + testPlat},
	}}
}

func (h *updHarness) step(adv *UpdateAdvert) {
	h.u.Observe(adv)
	h.u.Step(context.Background())
}

func (h *updHarness) exitCodes() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.exits...)
}

func (h *updHarness) logCount(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, l := range h.logs {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func (h *updHarness) isPaused() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.paused
}

func (h *updHarness) marker() updateMarker {
	h.t.Helper()
	var m updateMarker
	if err := json.Unmarshal(h.read(updateMarkerName), &m); err != nil {
		h.t.Fatalf("marker: %v", err)
	}
	return m
}

// assertUntouched: the live binary is the old one and no update artifact
// is left behind - the all-or-nothing guarantee.
func (h *updHarness) assertUntouched() {
	h.t.Helper()
	if got := h.read("foreman"); string(got) != string(oldBin) {
		h.t.Fatalf("live binary changed: %q", got)
	}
	for _, name := range []string{updateNewName, updateMarkerName, updatePrevName + updateTmpSuffix, updateMarkerName + updateTmpSuffix} {
		if h.exists(name) {
			h.t.Fatalf("%s left behind", name)
		}
	}
	if len(h.exitCodes()) != 0 {
		h.t.Fatalf("exit called: %v", h.exitCodes())
	}
	if h.isPaused() {
		h.t.Fatal("intake left paused after a failed update")
	}
}

func (h *updHarness) assertSwapped(attempts int) {
	h.t.Helper()
	if got := h.read("foreman"); string(got) != string(newBin) {
		h.t.Fatalf("live binary = %q, want new", got)
	}
	st, err := os.Stat(h.path("foreman"))
	if err != nil || st.Mode().Perm() != 0o755 {
		h.t.Fatalf("live mode = %v (%v), want 0755", st.Mode(), err)
	}
	if got := h.read(updatePrevName); string(got) != string(oldBin) {
		h.t.Fatalf("foreman.prev = %q, want old", got)
	}
	if h.exists(updateNewName) {
		h.t.Fatal("foreman.new left behind")
	}
	m := h.marker()
	if m.From != buildOld || m.To != buildNew || m.Attempts != attempts || m.RolledBack {
		h.t.Fatalf("marker = %+v", m)
	}
	if _, err := time.Parse(time.RFC3339, m.StartedAt); err != nil {
		h.t.Fatalf("started_at %q: %v", m.StartedAt, err)
	}
	if got := h.exitCodes(); len(got) != 1 || got[0] != 0 {
		h.t.Fatalf("exits = %v, want [0]", got)
	}
}

func TestParseCapsResponse(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{"204 no body", http.StatusNoContent, "", "", false},
		{"200 empty body", http.StatusOK, "  ", "", false},
		{"200 no advert", http.StatusOK, `{}`, "", false},
		{"200 null advert", http.StatusOK, `{"foreman_update":null}`, "", false},
		{"200 advert", http.StatusOK, `{"foreman_update":{"build":"` + buildNew + `","platforms":{"linux-amd64":{"sha256":"ab","path":"/download/foreman/foreman-linux-amd64"}}}}`, buildNew, false},
		{"200 unknown fields ignored", http.StatusOK, `{"x":1,"foreman_update":{"build":"b"}}`, "b", false},
		{"200 malformed", http.StatusOK, `{"foreman_update":`, "", true},
		{"202 ignored", http.StatusAccepted, `{"foreman_update":{"build":"b"}}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adv, err := parseCapsResponse(tc.status, []byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			got := ""
			if adv != nil {
				got = adv.Build
			}
			if got != tc.want {
				t.Fatalf("build = %q, want %q", got, tc.want)
			}
		})
	}
	adv, _ := parseCapsResponse(200, []byte(`{"foreman_update":{"build":"b","platforms":{"linux-arm64":{"sha256":"s","path":"/p"}}}}`))
	if p := adv.Platforms["linux-arm64"]; p.SHA256 != "s" || p.Path != "/p" {
		t.Fatalf("platform = %+v", p)
	}
}

func TestUpdaterIdleSwap(t *testing.T) {
	h := newUpdHarness(t)
	h.write(updateBootsName, []byte("2\n")) // stale guard counter
	h.write(updatePrevName, []byte("older"))
	h.step(h.advert(buildNew))
	h.assertSwapped(1)
	if h.exists(updateBootsName) {
		t.Fatal("stale update-boots not reset before the swap")
	}
	if !h.isPaused() {
		t.Fatal("intake must stay paused between the swap and the exit")
	}
	if got, _ := h.auth.Load().(string); got != "Bearer tok" {
		t.Fatalf("download Authorization = %q", got)
	}
	// Exiting: further steps and polls are inert.
	h.step(h.advert(buildNewer))
	h.u.PollOnce(context.Background())
	if h.hits.Load() != 1 || len(h.exitCodes()) != 1 {
		t.Fatalf("acted after exit: hits=%d exits=%v", h.hits.Load(), h.exitCodes())
	}
}

func TestUpdaterAbsoluteDownloadURL(t *testing.T) {
	h := newUpdHarness(t)
	h.u.FactoryURL = "http://unused.invalid"
	adv := h.advert(buildNew)
	adv.Platforms[testPlat] = UpdatePlatform{SHA256: strings.ToUpper(shaHex(newBin)), Path: h.srv.URL + "/download/foreman/foreman-" + testPlat}
	h.step(adv)
	h.assertSwapped(1)
}

func TestUpdaterIgnoresNotNewer(t *testing.T) {
	for _, build := range []string{buildOld, "20261116T120000Z-ffffffffffff", "20261115T000000Z-aaaaaaaaaaaa", "dev", "", "garbage"} {
		t.Run(build, func(t *testing.T) {
			h := newUpdHarness(t)
			h.step(h.advert(build))
			h.step(nil)
			h.assertUntouched()
			if h.hits.Load() != 0 || h.pauses != 0 {
				t.Fatalf("acted on %q: hits=%d pauses=%d", build, h.hits.Load(), h.pauses)
			}
		})
	}
}

func TestUpdaterPlatformNotAdvertised(t *testing.T) {
	h := newUpdHarness(t)
	adv := h.advert(buildNew)
	adv.Platforms = map[string]UpdatePlatform{"linux-arm64": {SHA256: "x", Path: "/p"}}
	h.step(adv)
	h.step(adv)
	h.assertUntouched()
	if n := h.logCount("without a linux-amd64 binary"); n != 1 {
		t.Fatalf("platform log count = %d, want 1", n)
	}
	adv.Platforms = map[string]UpdatePlatform{testPlat: {Path: "/p"}} // no sha
	h.step(adv)
	h.assertUntouched()
}

func TestUpdaterFailureInjection(t *testing.T) {
	short := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(newBin)+10))
		w.Write(newBin)
	}
	cases := []struct {
		name    string
		setup   func(h *updHarness)
		wantLog string
	}{
		{"bad sha", func(h *updHarness) { h.body.Store([]byte("tampered")) }, "sha256 mismatch"},
		{"empty download", func(h *updHarness) { h.body.Store([]byte{}) }, "sha256 mismatch"},
		{"short download", func(h *updHarness) { h.handler.Store(http.HandlerFunc(short)) }, "unexpected EOF"},
		{"http 404", func(h *updHarness) {
			h.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
		}, "status 404"},
		{"download timeout", func(h *updHarness) {
			h.u.DownloadTimeout = 50 * time.Millisecond
			h.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(len(newBin)))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
		}, "deadline exceeded"},
		{"connection refused", func(h *updHarness) { h.u.FactoryURL = "http://127.0.0.1:1" }, "download"},
		{"relative path", nil, "not absolute"},
		{"stage unwritable", func(h *updHarness) {
			h.openFn = func(n string) error {
				if strings.HasSuffix(n, updateNewName) {
					return os.ErrPermission
				}
				return nil
			}
		}, "stage"},
		{"prev copy fails", func(h *updHarness) {
			h.openFn = func(n string) error {
				if strings.HasSuffix(n, updatePrevName+updateTmpSuffix) {
					return errors.New("disk full")
				}
				return nil
			}
		}, "save foreman.prev"},
		{"prev rename fails", func(h *updHarness) {
			h.renameFn = func(o, n string) error {
				if strings.HasSuffix(n, updatePrevName) {
					return errors.New("EXDEV")
				}
				return nil
			}
		}, "save foreman.prev"},
		{"marker write fails", func(h *updHarness) {
			h.openFn = func(n string) error {
				if strings.HasSuffix(n, updateMarkerName+updateTmpSuffix) {
					return errors.New("read-only")
				}
				return nil
			}
		}, "write marker"},
		{"swap rename fails", func(h *updHarness) {
			h.renameFn = func(o, n string) error {
				if n == h.path("foreman") {
					return errors.New("ETXTBSY")
				}
				return nil
			}
		}, "swap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newUpdHarness(t)
			if tc.name == "relative path" {
				adv := h.advert(buildNew)
				adv.Platforms[testPlat] = UpdatePlatform{SHA256: shaHex(newBin), Path: "download/foreman"}
				h.step(adv)
			} else {
				h.mu.Lock()
				tc.setup(h)
				h.mu.Unlock()
				h.step(h.advert(buildNew))
			}
			h.assertUntouched()
			if n := h.logCount("AUTO-UPDATE FAILED"); n != 1 {
				t.Fatalf("failure log count = %d, logs=%v", n, h.logs)
			}
			if h.logCount(tc.wantLog) == 0 {
				t.Fatalf("no %q in logs %v", tc.wantLog, h.logs)
			}
			// Repair the fault: the next tick retries and succeeds, with
			// the attempt counted.
			h.mu.Lock()
			h.renameFn, h.openFn = nil, nil
			h.mu.Unlock()
			h.body.Store(newBin)
			h.handler.Store(http.HandlerFunc(nil))
			h.u.FactoryURL = h.srv.URL
			h.step(h.advert(buildNew))
			h.assertSwapped(2)
		})
	}
}

func TestUpdaterKeepsExistingPrevOnEarlyFailure(t *testing.T) {
	h := newUpdHarness(t)
	h.write(updatePrevName, []byte("older"))
	h.body.Store([]byte("tampered"))
	h.step(h.advert(buildNew))
	if got := h.read(updatePrevName); string(got) != "older" {
		t.Fatalf("foreman.prev clobbered by a failed attempt: %q", got)
	}
}

func TestUpdaterNotInLayout(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *updHarness)
	}{
		{"other path", func(h *updHarness) {
			h.u.Executable = func() (string, error) { return "/usr/local/bin/foreman", nil }
		}},
		{"executable error", func(h *updHarness) {
			h.u.Executable = func() (string, error) { return "", errors.New("no /proc") }
		}},
		{"dir not writable", func(h *updHarness) {
			h.openFn = func(n string) error {
				if strings.Contains(n, updateProbePrefix) {
					return os.ErrPermission
				}
				return nil
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newUpdHarness(t)
			tc.setup(h)
			h.step(h.advert(buildNew))
			h.step(h.advert(buildNew))
			h.assertUntouched()
			if h.hits.Load() != 0 {
				t.Fatal("downloaded outside the layout")
			}
			if n := h.logCount("binary not in writable layout"); n != 1 {
				t.Fatalf("layout log count = %d, want exactly 1 per build", n)
			}
			h.step(h.advert(buildNewer))
			if n := h.logCount("binary not in writable layout"); n != 2 {
				t.Fatalf("layout log count after a new build = %d, want 2", n)
			}
		})
	}
}

func TestUpdaterSymlinkedExecutableInLayout(t *testing.T) {
	h := newUpdHarness(t)
	link := filepath.Join(t.TempDir(), "foreman")
	if err := os.Symlink(h.path("foreman"), link); err != nil {
		t.Fatal(err)
	}
	h.u.Executable = func() (string, error) { return link, nil }
	h.step(h.advert(buildNew))
	h.assertSwapped(1)
}

func TestUpdaterRealUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newUpdHarness(t)
	h.u.OpenFile = nil
	if err := os.Chmod(filepath.Join(h.data, "bin"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(h.data, "bin"), 0o755) })
	h.step(h.advert(buildNew))
	h.assertUntouched()
	if h.logCount("not writable") != 1 {
		t.Fatalf("logs = %v", h.logs)
	}
}

func TestUpdaterDrainThenIdle(t *testing.T) {
	h := newUpdHarness(t)
	h.setBusy(true)
	h.step(h.advert(buildNew))
	if !h.isPaused() || !h.u.Draining() || h.hits.Load() != 0 {
		t.Fatalf("busy: paused=%v draining=%v hits=%d", h.isPaused(), h.u.Draining(), h.hits.Load())
	}
	h.advance(time.Minute)
	h.u.PollOnce(context.Background()) // still busy
	if h.hits.Load() != 0 || !h.isPaused() {
		t.Fatal("updated while busy")
	}
	h.setBusy(false)
	h.u.PollOnce(context.Background()) // drain poll finds it idle
	h.assertSwapped(1)
	if h.pauses != 1 {
		t.Fatalf("pauses = %d, want 1 (one drain)", h.pauses)
	}
}

func TestUpdaterDrainCeiling(t *testing.T) {
	h := newUpdHarness(t)
	h.setBusy(true)
	h.step(h.advert(buildNew))
	h.advance(DefaultDrainCeiling - time.Second)
	h.step(h.advert(buildNew))
	if !h.isPaused() {
		t.Fatal("drain ended before the ceiling")
	}
	h.advance(time.Second)
	h.step(h.advert(buildNew))
	if h.isPaused() || h.u.Draining() {
		t.Fatal("intake not resumed at the ceiling")
	}
	if h.logCount("resuming intake") != 1 {
		t.Fatalf("logs = %v", h.logs)
	}
	// Still busy on later ticks: no new drain for this build.
	h.advance(time.Minute)
	h.step(h.advert(buildNew))
	if h.isPaused() || h.u.Draining() || h.hits.Load() != 0 {
		t.Fatal("re-drained after the ceiling")
	}
	// A tick that finds it idle updates.
	h.setBusy(false)
	h.step(h.advert(buildNew))
	h.assertSwapped(1)
}

func TestUpdaterDrainCeilingNewBuildDrainsAgain(t *testing.T) {
	h := newUpdHarness(t)
	h.setBusy(true)
	h.step(h.advert(buildNew))
	h.advance(DefaultDrainCeiling)
	h.step(h.advert(buildNew))
	if h.isPaused() {
		t.Fatal("ceiling not applied")
	}
	h.step(h.advert(buildNewer))
	if !h.isPaused() || !h.u.Draining() {
		t.Fatal("a newer build must start its own drain")
	}
}

func TestUpdaterDrainEndsWhenAdvertWithdrawn(t *testing.T) {
	h := newUpdHarness(t)
	h.setBusy(true)
	h.step(h.advert(buildNew))
	if !h.isPaused() {
		t.Fatal("not draining")
	}
	h.step(nil)
	if h.isPaused() || h.u.Draining() {
		t.Fatal("drain survived a withdrawn advert")
	}
}

func TestUpdaterDrainFailureResumes(t *testing.T) {
	h := newUpdHarness(t)
	h.setBusy(true)
	h.step(h.advert(buildNew))
	h.setBusy(false)
	h.body.Store([]byte("tampered"))
	h.u.PollOnce(context.Background())
	h.assertUntouched()
	if h.u.Draining() {
		t.Fatal("still draining after a failed update")
	}
}

func TestUpdaterBootNoMarker(t *testing.T) {
	h := newUpdHarness(t)
	h.u.Boot()
	h.u.Boot() // idempotent
	if h.u.Pending() {
		t.Fatal("pending without a marker")
	}
	h.u.Confirm(context.Background()) // no-op
	h.advance(time.Hour)
	h.u.PollOnce(context.Background())
	if len(h.exitCodes()) != 0 {
		t.Fatal("exit without a marker")
	}
}

func (h *updHarness) writeMarker(m updateMarker) {
	h.t.Helper()
	data, _ := json.Marshal(m)
	h.write(updateMarkerName, data)
}

func TestUpdaterConfirm(t *testing.T) {
	h := newUpdHarness(t)
	h.u.Running = buildNew
	h.writeMarker(updateMarker{From: buildOld, To: buildNew, StartedAt: "2026-11-16T15:00:00Z", Attempts: 1})
	h.write(updateBootsName, []byte("1"))
	h.u.Boot()
	if !h.u.Pending() {
		t.Fatal("not pending")
	}
	// Pending: no chained update.
	h.step(h.advert(buildNewer))
	if h.hits.Load() != 0 || h.pauses != 0 {
		t.Fatal("updated again before confirming")
	}
	h.selfErr = errors.New("listener not up")
	h.u.Confirm(context.Background())
	if !h.u.Pending() || !h.exists(updateMarkerName) {
		t.Fatal("confirmed despite a failed self-check")
	}
	h.selfErr = nil
	h.advance(2 * time.Minute)
	h.u.Confirm(context.Background())
	if h.u.Pending() || h.exists(updateMarkerName) || h.exists(updateBootsName) {
		t.Fatal("confirm did not delete marker + update-boots")
	}
	if h.logCount("confirmed (updated from") != 1 {
		t.Fatalf("logs %v", h.logs)
	}
	h.advance(time.Hour)
	h.u.PollOnce(context.Background())
	if len(h.exitCodes()) != 0 {
		t.Fatal("rolled back after confirming")
	}
}

func TestUpdaterConfirmTimeoutRollsBack(t *testing.T) {
	h := newUpdHarness(t)
	h.u.Running = buildNew
	h.write("foreman", newBin)
	h.write(updatePrevName, oldBin)
	h.writeMarker(updateMarker{From: buildOld, To: buildNew, StartedAt: "2026-11-16T15:00:00Z", Attempts: 1})
	h.u.Boot()
	h.selfErr = errors.New("stores broken")
	h.advance(DefaultConfirmTimeout - time.Second)
	h.u.Confirm(context.Background())
	h.u.PollOnce(context.Background())
	if len(h.exitCodes()) != 0 {
		t.Fatal("rolled back before the timeout")
	}
	h.advance(time.Second)
	h.u.PollOnce(context.Background())
	if got := h.read("foreman"); string(got) != string(oldBin) {
		t.Fatalf("live after rollback = %q", got)
	}
	if st, _ := os.Stat(h.path("foreman")); st.Mode().Perm() != 0o755 {
		t.Fatalf("rolled-back mode %v", st.Mode())
	}
	if m := h.marker(); !m.RolledBack || m.To != buildNew || m.From != buildOld {
		t.Fatalf("marker after rollback = %+v", m)
	}
	if got := h.exitCodes(); len(got) != 1 || got[0] == 0 {
		t.Fatalf("exits = %v, want one non-zero", got)
	}
	h.u.Confirm(context.Background()) // too late: inert
	if !h.exists(updateMarkerName) {
		t.Fatal("confirm after rollback deleted the marker")
	}
}

func TestUpdaterRollbackRetriesAfterRenameFailure(t *testing.T) {
	h := newUpdHarness(t)
	h.u.Running = buildNew
	h.write("foreman", newBin)
	h.write(updatePrevName, oldBin)
	h.writeMarker(updateMarker{From: buildOld, To: buildNew, Attempts: 1})
	h.u.Boot()
	h.renameFn = func(o, n string) error {
		if n == h.path("foreman") {
			return errors.New("EBUSY")
		}
		return nil
	}
	h.advance(DefaultConfirmTimeout)
	h.u.PollOnce(context.Background())
	if len(h.exitCodes()) != 0 || string(h.read("foreman")) != string(newBin) || !h.u.Pending() {
		t.Fatal("failed rollback must leave state for a retry")
	}
	if h.exists("foreman.rollback.tmp") {
		t.Fatal("rollback temp left behind")
	}
	h.mu.Lock()
	h.renameFn = nil
	h.mu.Unlock()
	h.u.PollOnce(context.Background())
	if got := h.exitCodes(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("exits = %v", got)
	}
}

func TestUpdaterRollbackCopyFailureRetries(t *testing.T) {
	h := newUpdHarness(t)
	h.u.Running = buildNew
	h.write(updatePrevName, oldBin)
	h.writeMarker(updateMarker{From: buildOld, To: buildNew, Attempts: 1})
	h.u.Boot()
	h.openFn = func(n string) error {
		if strings.HasSuffix(n, ".rollback.tmp") {
			return errors.New("ENOSPC")
		}
		return nil
	}
	h.advance(DefaultConfirmTimeout)
	h.u.PollOnce(context.Background())
	if len(h.exitCodes()) != 0 || !h.u.Pending() {
		t.Fatal("copy failure must keep pending for a retry")
	}
	if h.logCount("ROLLBACK FAILED") != 1 {
		t.Fatalf("logs %v", h.logs)
	}
}

func TestUpdaterRollbackImpossible(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *updHarness)
	}{
		{"no foreman.prev", func(h *updHarness) {}},
		{"not in layout", func(h *updHarness) {
			h.write(updatePrevName, oldBin)
			h.u.Executable = func() (string, error) { return "/opt/elsewhere/foreman", nil }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newUpdHarness(t)
			h.u.Running = buildNew
			h.write("foreman", newBin)
			h.writeMarker(updateMarker{From: buildOld, To: buildNew, Attempts: 1})
			h.write(updateBootsName, []byte("1"))
			tc.setup(h)
			h.u.Boot()
			h.advance(DefaultConfirmTimeout)
			h.u.PollOnce(context.Background())
			if len(h.exitCodes()) != 0 || string(h.read("foreman")) != string(newBin) {
				t.Fatal("must keep running the new build")
			}
			if h.u.Pending() || h.exists(updateMarkerName) || h.exists(updateBootsName) {
				t.Fatal("marker/boots must be cleared so the guard stays inert")
			}
			if h.logCount("ROLLBACK IMPOSSIBLE") != 1 {
				t.Fatalf("logs %v", h.logs)
			}
		})
	}
}

func TestUpdaterBootAfterRollbackBlocklists(t *testing.T) {
	h := newUpdHarness(t)
	h.writeMarker(updateMarker{From: buildOld, To: buildNew, Attempts: 1, RolledBack: true})
	h.write(updateBootsName, []byte("3"))
	h.u.Boot()
	if h.u.Pending() || h.exists(updateMarkerName) || h.exists(updateBootsName) {
		t.Fatal("marker/boots not cleared after a rollback")
	}
	if h.logCount("ROLLED BACK") != 1 {
		t.Fatalf("logs %v", h.logs)
	}
	if got := strings.TrimSpace(string(h.read(updateBlockName))); got != buildNew {
		t.Fatalf("blocklist = %q", got)
	}
	// The same build is refused (logged once), as is an older-but-newer-
	// than-running one.
	h.step(h.advert(buildNew))
	h.step(h.advert(buildNew))
	h.step(h.advert(buildMid))
	if h.hits.Load() != 0 || h.pauses != 0 {
		t.Fatal("blocklisted build attempted")
	}
	if n := h.logCount("blocklisted"); n != 2 {
		t.Fatalf("blocklist log count = %d (one per build)", n)
	}
	// A fresh process (the restarted .prev) still refuses it.
	h2 := newUpdHarness(t)
	h2.data = h.data
	h2.u.DataDir = h.data
	h2.u.Executable = h.u.Executable
	h2.u.Boot()
	if !h2.u.Blocked(buildNew) || h2.u.Blocked(buildNewer) {
		t.Fatal("blocklist not persisted / too broad")
	}
	// A newer build is attempted.
	h.body.Store(newBin)
	h.step(h.advert(buildNewer))
	if got := h.read("foreman"); string(got) != string(newBin) {
		t.Fatal("newer build not applied after a blocklist")
	}
	if m := h.marker(); m.To != buildNewer {
		t.Fatalf("marker %+v", m)
	}
}

func TestUpdaterBootInterruptedSwapNotBlocklisted(t *testing.T) {
	h := newUpdHarness(t)
	h.writeMarker(updateMarker{From: buildOld, To: buildNew, Attempts: 1})
	h.u.Boot()
	if h.u.Pending() || h.exists(updateMarkerName) || h.exists(updateBlockName) {
		t.Fatal("interrupted swap must be discarded without a blocklist")
	}
	if h.logCount("INTERRUPTED") != 1 {
		t.Fatalf("logs %v", h.logs)
	}
	h.step(h.advert(buildNew))
	h.assertSwapped(1)
}

func TestUpdaterBootCorruptMarker(t *testing.T) {
	for _, body := range []string{"{not json", `{"from":"x"}`} {
		h := newUpdHarness(t)
		h.write(updateMarkerName, []byte(body))
		h.write(updateBootsName, []byte("1"))
		h.u.Boot()
		if h.u.Pending() || h.exists(updateMarkerName) || h.exists(updateBootsName) {
			t.Fatalf("corrupt marker %q not discarded", body)
		}
	}
}

func TestUpdaterBootUnreadableMarker(t *testing.T) {
	h := newUpdHarness(t)
	if err := os.Mkdir(h.path(updateMarkerName), 0o755); err != nil { // read -> EISDIR
		t.Fatal(err)
	}
	h.u.Boot()
	if h.u.Pending() || h.logCount("read marker") != 1 {
		t.Fatalf("logs %v", h.logs)
	}
}

func TestUpdaterBlocklistFileTolerant(t *testing.T) {
	h := newUpdHarness(t)
	h.write(updateBlockName, []byte("\n  garbage \n"+buildNew+"\n\n"))
	h.u.Boot()
	if !h.u.Blocked(buildNew) || !h.u.Blocked(buildMid) || h.u.Blocked(buildNewer) {
		t.Fatal("blocklist semantics")
	}
	if !h.u.Blocked("garbage") {
		t.Fatal("exact match must block even an unparseable entry")
	}
}

func TestUpdaterBlocklistWriteFailure(t *testing.T) {
	h := newUpdHarness(t)
	h.writeMarker(updateMarker{From: buildOld, To: buildNew, RolledBack: true})
	h.openFn = func(n string) error {
		if strings.HasSuffix(n, updateBlockName+updateTmpSuffix) {
			return errors.New("EROFS")
		}
		return nil
	}
	h.u.Boot()
	if !h.u.Blocked(buildNew) {
		t.Fatal("in-memory blocklist must still refuse the build")
	}
	if h.logCount("write blocklist") != 1 {
		t.Fatalf("logs %v", h.logs)
	}
}

func TestUpdaterRunLoop(t *testing.T) {
	h := newUpdHarness(t)
	h.u.Poll = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.u.Run(ctx); close(done) }()
	h.setBusy(true)
	h.u.Observe(h.advert(buildNew))
	deadline := time.Now().Add(5 * time.Second)
	for !h.u.Draining() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !h.u.Draining() {
		t.Fatal("Run did not step on Observe")
	}
	h.setBusy(false) // the poll ticker completes the update
	for len(h.exitCodes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	h.assertSwapped(1)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestUpdaterDefaults(t *testing.T) {
	u := &Updater{}
	if u.drainCeiling() != DefaultDrainCeiling || u.confirmTimeout() != DefaultConfirmTimeout || u.poll() != DefaultUpdatePoll {
		t.Fatal("defaults")
	}
	if u.now().IsZero() {
		t.Fatal("clock")
	}
	u.logf("auto-update: default logger smoke")
	dir := t.TempDir()
	if err := u.rename(filepath.Join(dir, "missing"), filepath.Join(dir, "x")); err == nil {
		t.Fatal("default rename")
	}
	f, err := u.openFile(filepath.Join(dir, "f"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	u.DataDir = dir
	if err := u.checkLayout(); err == nil {
		t.Fatal("the test binary is not in the layout")
	}
}
