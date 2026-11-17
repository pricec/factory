package factoryapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// TestUIThrottleControls (token-throttle S6): the served page carries the
// gate status, held list, PAUSED banner, override controls and gate alerts,
// and reads only keys the backend emits. There is no JS runner in the repo;
// the render logic was also executed offline (see investigations/token-s6-report.md).
func TestUIThrottleControls(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	a, err := New(Config{
		DataDir: t.TempDir(), Addr: ":8091", AdvertiseURL: "http://factory.test",
		Now: func() time.Time { return now }, Usage: UsageConfig{F: 1},
	})
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
		// regions
		`id="u-pause"`, `id="u-gate"`, `id="u-ctl"`, `id="u-held"`, `id="u-alerts"`,
		// banner, controls, confirmation, errors
		"Paused: ", " limit reached. Resumes ", "Throttle off for ", "Throttle off until reset", "Back to auto",
		"window.confirm(", "throttle change failed: ", `postJSON("/v1/throttle"`, `{ mode: "off", until:`, `{ mode: "auto" }`,
		"override until ", "FAIL-OPEN", "PAUSED",
		// data sources: existing endpoints only
		`getJSON("/v1/jobs?state=held&limit=50")`, `getJSON("/v1/alerts")`, `"factory:throttle"`,
		// keys read
		"g.paused_until", "g.paused_window", "g.pause_reason", "g.override_until", "g.mode_source", "g.fail_open", "g.held", "g.summary",
		"j.held_reason", "j.spec.task", "j.updated_at", "a.source", "a.title", "a.priority",
		// click opens job detail; dim on failure
		"openJob(", "classList.toggle(\"unavail\"",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("UI page missing %q", want)
		}
	}
	for _, sel := range []string{".badge.PAUSED", ".badge.OFF", ".badge.AUTO", ".upause", "#usagepanel.unavail"} {
		if !strings.Contains(page, sel) {
			t.Errorf("no CSS rule %s", sel)
		}
	}

	// /v1/usage .gate carries every key the UI reads (always-present ones).
	var u struct {
		Gate map[string]json.RawMessage `json:"gate"`
	}
	if err := json.Unmarshal(do("GET", "/v1/usage", "").Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"mode", "mode_source", "raw_state", "level", "fail_open", "paused", "held"} {
		if _, ok := u.Gate[k]; !ok {
			t.Errorf("/v1/usage gate missing %q", k)
		}
	}

	// POST /v1/throttle round trip as the buttons send it.
	until := now.Add(10 * time.Minute).Format(time.RFC3339)
	rec := do("POST", "/v1/throttle", `{"mode":"off","until":"`+until+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST off: %d %s", rec.Code, rec.Body)
	}
	var g struct {
		M                string `json:"mode"`
		S                string `json:"mode_source"`
		U                string `json:"override_until"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	if g.M != "off" || g.S != "override" || g.U == "" {
		t.Errorf("override gate = %+v", g)
	}
	if rec := do("POST", "/v1/throttle", `{"mode":"auto"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"auto"`) {
		t.Errorf("POST auto: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/v1/throttle", `{"mode":"bogus"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "message") {
		t.Errorf("bad mode should be a visible 400 with error.message: %d %s", rec.Code, rec.Body)
	}

	// Held list rows come from GET /v1/jobs: held_reason, updated_at, spec.task.
	b, _ := json.Marshal(jobs.Job{ID: "j", State: "held", HeldReason: "r", UpdatedAt: "t", Spec: wire.JobSpec{Task: "title"}})
	var row map[string]any
	_ = json.Unmarshal(b, &row)
	spec, _ := row["spec"].(map[string]any)
	if row["held_reason"] != "r" || row["updated_at"] != "t" || spec["task"] != "title" {
		t.Errorf("job JSON shape changed: %s", b)
	}
	// Alerts keep the source/title/priority/created_at keys the panel filters on.
	b, _ = json.Marshal(tasks.Alert{ID: "a", Priority: 1, Title: "t", Source: "factory:throttle", CreatedAt: now})
	row = nil
	_ = json.Unmarshal(b, &row)
	for _, k := range []string{"id", "priority", "title", "source", "created_at"} {
		if _, ok := row[k]; !ok {
			t.Errorf("alert JSON missing %q", k)
		}
	}
}
