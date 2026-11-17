package factoryapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestUIMergeQueuePanel (spec §6.11): the served page carries the Merge
// queue tab - list with state badges, create form, pause/resume, per-row
// cancel, the merging job linking into the Jobs view - and reads only keys
// the backend emits; every request the panel sends round-trips. There is
// no JS runner in the repo, so this is the contract half (the
// TestUIThrottleControls pattern).
func TestUIMergeQueuePanel(t *testing.T) {
	t.Setenv(EnvMergeForeman, "")
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	a, err := New(Config{DataDir: t.TempDir(), Addr: ":8095", AdvertiseURL: "http://factory.test", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}

	page := do("GET", "/", "").Body.String()
	for _, want := range []string{
		// regions + controls
		`id="tab-merges"`, ">Merge queue<", `id="mergepanel"`, `id="mq-form"`, `id="mq-branch"`, `id="mq-head"`, `id="mq-note"`,
		`id="mq-status"`, `id="mq-pause"`, `id="mq-resume"`, "queue merge", "PAUSED", "window.confirm(",
		// endpoints, exactly as the backend mounts them
		`getJSON("/v1/merge-requests")`, `postJSON("/v1/merge-requests", body)`,
		`"/v1/merge-requests/" + (paused ? "pause" : "resume")`, `"/v1/merge-requests/" + encodeURIComponent(id) + "/cancel"`,
		// keys read
		"q.paused", "q.requests", "m.state", "m.branch", "m.head", "m.created_at", "m.job_id", "m.note", "m.result", "m.id", "mr.id",
		// merging job links into the Jobs view; loader registered
		"openJob(a.dataset.job)", "merges: loadMerges",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("UI page missing %q", want)
		}
	}
	for _, st := range []string{"queued", "merging", "landed", "needs_attention", "cancelled"} {
		if !strings.Contains(page, ".badge.mq-"+st) {
			t.Errorf("no badge CSS for state %s", st)
		}
	}

	// Create exactly as the form sends it.
	rec := do("POST", "/v1/merge-requests", `{"branch":"feat/ui","head":"0123abcd","note":"UI merge"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var mr map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &mr)
	id, _ := mr["id"].(string)
	if id == "" {
		t.Fatalf("create response lacks id: %s", rec.Body)
	}
	// Pause, then the list carries every key the panel reads.
	if rec := do("POST", "/v1/merge-requests/pause", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body)
	}
	var q struct {
		Paused   *bool                        `json:"paused"`
		Requests []map[string]json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(do("GET", "/v1/merge-requests", "").Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.Paused == nil || !*q.Paused || len(q.Requests) != 1 {
		t.Fatalf("view = %+v", q)
	}
	for _, k := range []string{"id", "state", "branch", "head", "note", "created_at"} {
		if _, ok := q.Requests[0][k]; !ok {
			t.Errorf("request missing key %q", k)
		}
	}
	if rec := do("POST", "/v1/merge-requests/resume", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("resume: %d", rec.Code)
	}
	if rec := do("POST", "/v1/merge-requests/"+id+"/cancel", `{}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cancelled"`) {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
}
