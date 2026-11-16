package foremanapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

// orphFakeRegistry is a fake factory: GET /v1/jobs honours the state and
// foreman filters (unless sloppy), and the factory:main inbox records every
// posted envelope. Failure switches reproduce an unreachable registry and a
// rejected post.
type orphFakeRegistry struct {
	srv *httptest.Server

	mu        sync.Mutex
	jobs      []registryJob
	sloppy    bool            // ignore query filters, like a misbehaving registry
	failGets  int             // the next N GETs answer 500
	postFail  map[string]bool // job ids whose result post answers 500
	queries   []url.Values
	posted    []wire.MessageEnvelope
	postedRaw []string
}

func newOrphFakeRegistry(t *testing.T, jobs ...registryJob) *orphFakeRegistry {
	t.Helper()
	r := &orphFakeRegistry{jobs: jobs, postFail: map[string]bool{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *orphFakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/v1/jobs":
		r.queries = append(r.queries, req.URL.Query())
		if r.failGets > 0 {
			r.failGets--
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		q := req.URL.Query()
		out := []registryJob{}
		for _, j := range r.jobs {
			if !r.sloppy {
				if s := q.Get("state"); s != "" && j.State != s {
					continue
				}
				if f := q.Get("foreman"); f != "" && j.ForemanID != f {
					continue
				}
			}
			out = append(out, j)
		}
		_ = json.NewEncoder(w).Encode(out)
	case req.Method == http.MethodPost && req.URL.Path == "/v1/inboxes/"+FactoryMainInbox+"/messages":
		var env wire.MessageEnvelope
		if err := json.NewDecoder(req.Body).Decode(&env); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.postFail[env.JobID] {
			http.Error(w, "rejected", http.StatusInternalServerError)
			return
		}
		r.posted = append(r.posted, env)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	default:
		http.NotFound(w, req)
	}
}

// results returns the job ids of every posted job.result, sorted.
func (r *orphFakeRegistry) results(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, env := range r.posted {
		if env.Type != "job.result" {
			t.Errorf("posted envelope type %q, want job.result", env.Type)
		}
		ids = append(ids, env.JobID)
	}
	sort.Strings(ids)
	return ids
}

func (r *orphFakeRegistry) postedEnvelopes() []wire.MessageEnvelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]wire.MessageEnvelope(nil), r.posted...)
}

// orphNewForeman builds a real foreman App as "alpha" against reg, with a
// stored pairing so New never talks to a factory, and procRoot as its
// process table. Run is never called: these tests drive the pass directly.
func orphNewForeman(t *testing.T, reg *orphFakeRegistry, procRoot string) *App {
	t.Helper()
	return orphNewForemanIn(t, reg, procRoot, t.TempDir())
}

// orphNewForemanIn is orphNewForeman with a caller-chosen data dir, so a
// test can place fake worker directories inside it.
func orphNewForemanIn(t *testing.T, reg *orphFakeRegistry, procRoot, dataDir string) *App {
	t.Helper()
	paired := pairingRecord{Bearer: "test-bearer", InboxID: "foreman:alpha", ParentURL: reg.srv.URL}
	data, err := json.Marshal(paired)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, pairedFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{
		FactoryURL: reg.srv.URL,
		ID:         "alpha",
		DataDir:    dataDir,
		Addr:       "127.0.0.1:0",
		ProcRoot:   procRoot,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// orphFakeProc builds a fake process table: each pid gets a cwd symlink to
// its target, plus a non-numeric entry that must be ignored.
func orphFakeProc(t *testing.T, cwds map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	for pid, target := range cwds {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "cwd")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func orphRunning(id, owner string) registryJob {
	return registryJob{ID: id, State: "running", ForemanID: owner}
}

// orphMkJobsRoot creates the jobs root so symlink resolution sees it, and
// returns the resolved root the probe compares against.
func orphMkJobsRoot(t *testing.T, dataDir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataDir, jobsDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	return jobsRootFor(dataDir)
}

func TestReconcileFailsOwnedOrphansOnly(t *testing.T) {
	reg := newOrphFakeRegistry(t,
		orphRunning("job_a1", "foreman:alpha"),
		orphRunning("job_a2", "alpha"), // bare owner spelling is this foreman too
		orphRunning("job_b1", "foreman:beta"),
		registryJob{ID: "job_a3", State: "dispatched", ForemanID: "foreman:alpha"},
		registryJob{ID: "job_a4", State: "done", ForemanID: "foreman:alpha"},
	)
	a := orphNewForeman(t, reg, orphFakeProc(t, nil))

	rec, err := a.ReconcileOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if want := []string{"job_a1", "job_a2"}; !reflect.DeepEqual(sortedCopy(rec.Orphaned), want) {
		t.Errorf("Orphaned = %v, want %v", rec.Orphaned, want)
	}
	if rec.Listed != 2 {
		t.Errorf("Listed = %d, want 2 (only this foreman's running jobs)", rec.Listed)
	}
	if got := reg.results(t); !reflect.DeepEqual(got, []string{"job_a1", "job_a2"}) {
		t.Errorf("posted results = %v, want exactly this foreman's orphans", got)
	}
	for _, q := range reg.queries {
		if q.Get("state") != "running" {
			t.Errorf("query %v: state filter must be running", q)
		}
		if q.Get("limit") != "1000" {
			t.Errorf("query %v: limit must be 1000", q)
		}
	}
	for _, env := range reg.postedEnvelopes() {
		if env.From != "foreman:alpha" {
			t.Errorf("result for %s From = %q, want foreman:alpha", env.JobID, env.From)
		}
		var res wire.JobResult
		if err := json.Unmarshal(env.Payload, &res); err != nil {
			t.Fatalf("payload for %s: %v", env.JobID, err)
		}
		if res.Status != wire.StatusFailed || res.Error == nil || res.Error.Kind != wire.KindCrash {
			t.Errorf("result for %s = %+v, want failed/crash so the retry arms", env.JobID, res)
		}
		if res.Error != nil && !strings.Contains(res.Error.Message, "orphaned") {
			t.Errorf("result for %s message %q: must be loud and name the orphan", env.JobID, res.Error.Message)
		}
	}
}

func TestReconcileLeavesLiveWorkersAlone(t *testing.T) {
	reg := newOrphFakeRegistry(t,
		orphRunning("job_live", "foreman:alpha"),
		orphRunning("job_sub", "foreman:alpha"),
		orphRunning("job_del", "foreman:alpha"),
		orphRunning("job_sib", "foreman:alpha"),
		orphRunning("job_dead", "foreman:alpha"),
	)
	// Probe the process table relative to the resolved jobs root.
	dataDir := t.TempDir()
	root := orphMkJobsRoot(t, dataDir)
	procRoot := orphFakeProc(t, map[string]string{
		"100": filepath.Join(root, "job_live"),       // the worker itself
		"101": filepath.Join(root, "job_sub", "mcp"), // a grandchild still in the job dir
		"102": filepath.Join(root, "job_del") + " (deleted)",
		"103": filepath.Join(root, "job_sibling"), // prefix sibling of job_sib, not a worker
		"104": "/usr/bin",                         // unrelated
	})
	a := orphNewForemanIn(t, reg, procRoot, dataDir)

	rec, err := a.ReconcileOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if want := []string{"job_del", "job_live", "job_sub"}; !reflect.DeepEqual(sortedCopy(rec.Live), want) {
		t.Errorf("Live = %v, want %v", sortedCopy(rec.Live), want)
	}
	if want := []string{"job_dead", "job_sib"}; !reflect.DeepEqual(sortedCopy(rec.Orphaned), want) {
		t.Errorf("Orphaned = %v, want %v", sortedCopy(rec.Orphaned), want)
	}
	if got := reg.results(t); !reflect.DeepEqual(got, []string{"job_dead", "job_sib"}) {
		t.Errorf("posted results = %v: a live worker must never be failed", got)
	}
}

func TestReconcileSkipsJobsThisProcessSpawned(t *testing.T) {
	reg := newOrphFakeRegistry(t, orphRunning("job_local", "foreman:alpha"))
	a := orphNewForeman(t, reg, orphFakeProc(t, nil))
	a.markSpawned("job_local")

	rec, err := a.ReconcileOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if want := []string{"job_local"}; !reflect.DeepEqual(rec.Local, want) {
		t.Errorf("Local = %v, want %v", rec.Local, want)
	}
	if len(reg.postedEnvelopes()) != 0 {
		t.Errorf("a job this process spawned was posted as an orphan")
	}
}

func TestReconcileFailsClosedWithoutACompleteProcessTable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-proc")

	// A pid entry whose cwd is a regular file: readlink fails with EINVAL,
	// which is neither "exited" nor "not ours", so the pass must abort.
	odd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(odd, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(odd, "42", "cwd"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, procRoot := range map[string]string{"missing table": missing, "unreadable cwd": odd} {
		t.Run(name, func(t *testing.T) {
			reg := newOrphFakeRegistry(t, orphRunning("job_x", "foreman:alpha"))
			a := orphNewForeman(t, reg, procRoot)
			rec, err := a.ReconcileOrphans(context.Background())
			if err == nil {
				t.Fatal("want an error: nothing may be failed without a complete process view")
			}
			if len(rec.Orphaned) != 0 || len(reg.postedEnvelopes()) != 0 {
				t.Errorf("failed jobs %v / posted %d with an incomplete process view", rec.Orphaned, len(reg.postedEnvelopes()))
			}
		})
	}
}

func TestReconcileNothingRunningReadsNoProcessTable(t *testing.T) {
	reg := newOrphFakeRegistry(t)
	a := orphNewForeman(t, reg, filepath.Join(t.TempDir(), "no-such-proc"))
	rec, err := a.ReconcileOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOrphans with no running jobs: %v", err)
	}
	if rec.Listed != 0 || len(rec.Orphaned) != 0 {
		t.Errorf("rec = %+v, want empty", rec)
	}
}

func TestReconcileRejectsForeignAndNonRunningRowsFromASloppyRegistry(t *testing.T) {
	reg := newOrphFakeRegistry(t,
		orphRunning("job_a1", "foreman:alpha"),
		orphRunning("job_b1", "foreman:beta"),
		registryJob{ID: "job_a2", State: "dispatched", ForemanID: "foreman:alpha"},
		registryJob{ID: "job_a3", State: "done", ForemanID: "foreman:alpha"},
		orphRunning("job_bad/../x", "foreman:alpha"),
	)
	reg.sloppy = true
	a := orphNewForeman(t, reg, orphFakeProc(t, nil))

	rec, err := a.ReconcileOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if want := []string{"job_a1"}; !reflect.DeepEqual(rec.Orphaned, want) {
		t.Errorf("Orphaned = %v, want %v", rec.Orphaned, want)
	}
	if got := reg.results(t); !reflect.DeepEqual(got, []string{"job_a1"}) {
		t.Errorf("posted results = %v, want only job_a1", got)
	}
}

func TestReconcileFailsClosedWhenRegistryUnreachable(t *testing.T) {
	reg := newOrphFakeRegistry(t, orphRunning("job_a1", "foreman:alpha"))
	reg.failGets = 1
	a := orphNewForeman(t, reg, orphFakeProc(t, nil))

	if _, err := a.ReconcileOrphans(context.Background()); err == nil {
		t.Fatal("want an error when the registry query fails")
	}
	if len(reg.postedEnvelopes()) != 0 {
		t.Error("posted results without a registry listing")
	}
}

func TestReconcileContinuesPastAFailedPost(t *testing.T) {
	reg := newOrphFakeRegistry(t,
		orphRunning("job_a1", "foreman:alpha"),
		orphRunning("job_a2", "foreman:alpha"),
	)
	reg.postFail["job_a1"] = true
	a := orphNewForeman(t, reg, orphFakeProc(t, nil))

	rec, err := a.ReconcileOrphans(context.Background())
	if err == nil || !strings.Contains(err.Error(), "job_a1") {
		t.Fatalf("err = %v, want an error naming job_a1", err)
	}
	if want := []string{"job_a2"}; !reflect.DeepEqual(rec.Orphaned, want) {
		t.Errorf("Orphaned = %v, want %v: one failed post must not stop the pass", rec.Orphaned, want)
	}
	if got := reg.results(t); !reflect.DeepEqual(got, []string{"job_a2"}) {
		t.Errorf("posted results = %v, want [job_a2]", got)
	}
}

func TestRecoverOrphansRetriesUntilTheRegistryAnswers(t *testing.T) {
	reg := newOrphFakeRegistry(t, orphRunning("job_a1", "foreman:alpha"))
	reg.failGets = 1 // first attempt fails, the retry succeeds
	a := orphNewForeman(t, reg, orphFakeProc(t, nil))

	a.recoverOrphans(context.Background())
	if got := reg.results(t); !reflect.DeepEqual(got, []string{"job_a1"}) {
		t.Errorf("posted results = %v, want [job_a1] after the retry", got)
	}
}

func TestOwnerFormsAreTheForemansOwnSpellings(t *testing.T) {
	reg := newOrphFakeRegistry(t)
	a := orphNewForeman(t, reg, orphFakeProc(t, nil))
	want := []string{"foreman:alpha", "alpha"}
	if got := a.ownerForms(); !reflect.DeepEqual(got, want) {
		t.Errorf("ownerForms = %v, want %v", got, want)
	}
}

func TestHasWorkerIn(t *testing.T) {
	root := filepath.FromSlash("/data/jobs")
	job := filepath.Join(root, "job_1")
	cases := []struct {
		name string
		cwds []string
		want bool
	}{
		{"exact job dir", []string{job}, true},
		{"nested in job dir", []string{filepath.Join(job, "x", "y")}, true},
		{"sibling with shared prefix", []string{filepath.Join(root, "job_12")}, false},
		{"parent of job dir", []string{root}, false},
		{"unrelated", []string{"/usr/bin"}, false},
		{"no processes", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasWorkerIn(job, c.cwds); got != c.want {
				t.Errorf("hasWorkerIn(%v) = %v, want %v", c.cwds, got, c.want)
			}
		})
	}
}

func TestPlausibleJobID(t *testing.T) {
	cases := map[string]bool{
		"job_01M7J9PZPJVM899EN3XE9M5KNJ": true,
		"job_1":                          true,
		"":                               false,
		".":                              false,
		"..":                             false,
		"job_a/../../etc":                false,
		`job_a\b`:                        false,
	}
	for id, want := range cases {
		if got := plausibleJobID(id); got != want {
			t.Errorf("plausibleJobID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestProcessCwdsSkipsExitedAndForeignAndTrimsDeleted(t *testing.T) {
	root := t.TempDir()
	mk := func(name, target string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if target != "" {
			if err := os.Symlink(target, filepath.Join(root, name, "cwd")); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("1", "/a")
	mk("2", "/b (deleted)")
	mk("3", "") // no cwd entry: the process has exited
	mk("self", "/not-a-pid")
	got, err := processCwds(root)
	if err != nil {
		t.Fatalf("processCwds: %v", err)
	}
	sort.Strings(got)
	if want := []string{"/a", "/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("processCwds = %v, want %v", got, want)
	}
}

func TestProcessCwdsIgnoresOtherUsersProcesses(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/x", filepath.Join(dir, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory; permission denial cannot be simulated")
	}
	got, err := processCwds(root)
	if err != nil {
		t.Fatalf("processCwds: %v (a process we cannot read is not our worker and must not abort the pass)", err)
	}
	if len(got) != 0 {
		t.Errorf("processCwds = %v, want none", got)
	}
}

func TestJobsRootForResolvesRelativeDataDir(t *testing.T) {
	got := jobsRootFor("relative-data")
	if !filepath.IsAbs(got) {
		t.Fatalf("jobsRootFor = %q, want an absolute path", got)
	}
	if !strings.HasSuffix(got, filepath.Join("relative-data", jobsDirName)) {
		t.Errorf("jobsRootFor = %q, want it to end in relative-data/jobs", got)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
