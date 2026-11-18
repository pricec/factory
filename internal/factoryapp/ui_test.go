package factoryapp_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

// TestUIShell: GET / serves the operator UI page (spec §4.1): HTML, not a
// 404, and no auth required.
func TestUIShell(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	for _, want := range []string{"<html", "</html>", "Foremen", "Inboxes"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("UI page missing %q (got %d bytes)", want, len(body))
		}
	}
	// The create-job form's model picker (spec §6.9): the select the UI
	// populates from GET /v1/models, plus its free-text custom escape.
	for _, want := range []string{`id="job-model"`, `id="job-model-custom"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("UI page missing the model picker markup %q", want)
		}
	}
	// The settings panel (spec §6.9, 2026-11-15 amendment): the tab, the
	// save form, and the datalist feeding its inputs.
	for _, want := range []string{`id="tab-settings"`, `id="settingsform"`, `id="model-list"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("UI page missing the settings panel markup %q", want)
		}
	}
}

// TestUISpeechMic: the voice-input client and wiring ship in the page: the
// three composers get a mic, gated on GET /v1/speech/config, and the page
// carries no speech secrets (spec §6.13).
func TestUISpeechMic(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	for _, want := range []string{
		`id="voice-client"`, `window.FactoryVoice`, `getUserMedia`, `echoCancellation: true`, `noiseSuppression: true`,
		`audio/mp4`, `/v1/speech/stt`, `/v1/speech/config`,
		`{ ta: "chat-input"`, `{ ta: "newtask-input"`, `{ ta: "p-input"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("UI page missing voice markup %q", want)
		}
	}
	for _, bad := range []string{"FACTORY_SPEECH", "speech-key"} {
		if strings.Contains(body, bad) {
			t.Errorf("UI page leaks %q", bad)
		}
	}
	// Manual mic transcripts are never auto-sent; only the opt-in
	// hands-free toggle sends (TestUISpokenReplies).
	if strings.Contains(body, "autosend") {
		t.Error("transcripts must not auto-send")
	}
	// The phone new-task / reply sheets are built on demand and get a mic too.
	for _, want := range []string{`wireSheets`, `form.p-sheetform`} {
		if !strings.Contains(body, want) {
			t.Errorf("UI page missing sheet mic wiring %q", want)
		}
	}
}

// TestUISpokenReplies: spoken replies + hands-free ship in the voice block
// (spec §6.14): speaker toggle persisted in localStorage, tts/plan chunking
// with AudioContext scheduling, an energy VAD, live-SSE-only triggering, and
// no speech secrets in the page.
func TestUISpokenReplies(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	for _, want := range []string{
		`createSpeaker`, `createHandsFree`, `spokenFor`, `stripMarkdown`,
		`/v1/speech/tts/plan`, `/v1/speech/tts`, `decodeAudioData`, `webkitAudioContext`,
		`factory.voice.speak`, `bar.id = "voice-out"`, `"factory:envelope"`, `window.FactoryHost`,
		`p.spoken`, `hangoverMs`, `getFloatTimeDomainData`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("UI page missing spoken-reply markup %q", want)
		}
	}
	// Speaking is driven by the live SSE stream only: the history renderers
	// must not reference the speaker.
	for _, fn := range []string{"function bubbleHTML", "function renderThread", "async function loadThread"} {
		i := strings.Index(body, fn)
		if i < 0 {
			t.Fatalf("missing %q", fn)
		}
		end := i + 3000
		if end > len(body) {
			end = len(body)
		}
		if strings.Contains(body[i:end], "speak(") {
			t.Errorf("%s must not speak (no replay of history)", fn)
		}
	}
	if strings.Contains(body, "FACTORY_SPEECH") || strings.Contains(body, "speech-key") {
		t.Error("UI page leaks speech configuration")
	}
}

// inboxSummary mirrors the GET /v1/inboxes row (spec §4.1).
type inboxSummary struct {
	InboxID string `json:"inbox_id"`
	Total   int    `json:"total"`
	Unacked int    `json:"unacked"`
	LastTS  string `json:"last_ts"`
}

// TestInboxSummariesEndpoint: GET /v1/inboxes aggregates every inbox that
// has ever received a message - including acked-only ones - and stays
// unauthenticated like the rest of the control plane.
func TestInboxSummariesEndpoint(t *testing.T) {
	e := newEnv(t)

	// Empty store: [] (never null - the UI renders it directly).
	var empty []inboxSummary
	if status := e.do("GET", "/v1/inboxes", "", nil, &empty); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty store: got %+v, want []", empty)
	}

	alpha := e.pair("alpha", nil)
	// One message in factory:main, two in foreman:alpha, one acked.
	if status := e.postToFactoryMain(alpha, wire.MessageEnvelope{Type: "job.event", JobID: "job_1", From: "foreman:alpha"}); status != http.StatusCreated {
		t.Fatalf("post factory:main status = %d", status)
	}
	var ids []string
	for i := 0; i < 2; i++ {
		var env wire.MessageEnvelope
		if status := e.do("POST", "/v1/inboxes/foreman:alpha/messages", alpha,
			map[string]any{"type": "job.request", "job_id": "job_uitest", "from": "foreman:alpha"}, &env); status != http.StatusCreated {
			t.Fatalf("post %d status = %d, want 201", i, status)
		}
		ids = append(ids, env.ID)
	}
	if status := e.do("POST", "/v1/inboxes/foreman:alpha/ack", alpha, map[string]any{"ids": ids[:1]}, nil); status != http.StatusNoContent {
		t.Fatalf("ack status = %d, want 204", status)
	}

	var sums []inboxSummary
	if status := e.do("GET", "/v1/inboxes", "", nil, &sums); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	want := map[string]inboxSummary{
		// The app's consumeLoop (consumer.go) drains factory:main
		// continuously, so its message is acked almost immediately; do not
		// pin factory:main's Unacked.
		"factory:main":  {InboxID: "factory:main", Total: 1},
		"foreman:alpha": {InboxID: "foreman:alpha", Total: 2, Unacked: 1},
	}
	if len(sums) != len(want) {
		t.Fatalf("got %d summaries (%+v), want %d", len(sums), sums, len(want))
	}
	for _, s := range sums {
		w, ok := want[s.InboxID]
		if !ok {
			t.Errorf("unexpected inbox %q", s.InboxID)
			continue
		}
		s.LastTS = ""
		if s.InboxID == "factory:main" {
			s.Unacked = w.Unacked // consumeLoop may not have drained yet
		}
		if s != w {
			t.Errorf("%s: got %+v, want %+v", s.InboxID, s, w)
		}
	}
}

// TestInboxRecentEndpoint: GET /v1/inboxes/{id}/recent returns the newest
// envelopes acked-included, validates limit, and is unauthenticated.
func TestInboxRecentEndpoint(t *testing.T) {
	e := newEnv(t)
	bearer := e.pair("alpha", nil)

	// Unknown inbox: empty array.
	var envs []wire.MessageEnvelope
	if status := e.do("GET", "/v1/inboxes/foreman:nobody/recent", "", nil, &envs); status != http.StatusOK {
		t.Fatalf("unknown inbox status = %d, want 200", status)
	}
	if envs == nil || len(envs) != 0 {
		t.Fatalf("unknown inbox: got %+v, want []", envs)
	}

	// Post three messages to foreman:alpha, then ack one.
	var ids []string
	for i := 0; i < 3; i++ {
		var env wire.MessageEnvelope
		body := map[string]any{"type": "job.request", "job_id": "job_ui" + string(rune('0'+i)), "from": "foreman:alpha", "payload": map[string]any{"n": i}}
		if status := e.do("POST", "/v1/inboxes/foreman:alpha/messages", bearer, body, &env); status != http.StatusCreated {
			t.Fatalf("post %d status = %d, want 201", i, status)
		}
		ids = append(ids, env.ID)
	}
	if status := e.do("POST", "/v1/inboxes/foreman:alpha/ack", bearer, map[string]any{"ids": ids[:1]}, nil); status != http.StatusNoContent {
		t.Fatalf("ack status = %d, want 204", status)
	}

	// Recent: all three, chronological, acked included.
	envs = nil
	if status := e.do("GET", "/v1/inboxes/foreman:alpha/recent", "", nil, &envs); status != http.StatusOK {
		t.Fatalf("recent status = %d, want 200", status)
	}
	if len(envs) != 3 {
		t.Fatalf("recent: got %d envelopes, want 3", len(envs))
	}
	for i, want := range ids {
		if envs[i].ID != want {
			t.Errorf("row %d: got %s, want %s", i, envs[i].ID, want)
		}
	}
	if envs[0].Type != "job.request" {
		t.Errorf("row 0 type = %q", envs[0].Type)
	}

	// limit keeps the newest.
	envs = nil
	if status := e.do("GET", "/v1/inboxes/foreman:alpha/recent?limit=1", "", nil, &envs); status != http.StatusOK {
		t.Fatalf("recent limit=1 status = %d", status)
	}
	if len(envs) != 1 || envs[0].ID != ids[2] {
		t.Fatalf("limit=1: got %+v, want [%s]", envs, ids[2])
	}

	// Table-driven bad limits.
	for _, tt := range []struct {
		limit string
	}{
		{"0"}, {"-1"}, {"501"}, {"notanumber"},
	} {
		var errResp map[string]any
		if status := e.do("GET", "/v1/inboxes/foreman:alpha/recent?limit="+tt.limit, "", nil, &errResp); status != http.StatusBadRequest {
			t.Errorf("limit=%q: status = %d, want 400", tt.limit, status)
		}
	}

	// The long-poll endpoint stays authed and separate: the recent endpoint
	// must not shadow POST .../messages (routed to the authed inbox mux).
	if status := e.do("POST", "/v1/inboxes/foreman:alpha/messages", bearer, map[string]any{"type": "job.event", "job_id": "job_uiecho", "from": "foreman:alpha"}, nil); status != http.StatusCreated {
		t.Errorf("POST messages after UI routes mounted: status = %d, want 201", status)
	}
}

// TestUIForemenData: the UI's foreman list and detail views are driven by the
// existing GET /v1/foremen + GET /v1/jobs?foreman= endpoints; assert the
// shapes the page fetches actually line up (capabilities is a JSON-encoded
// *string* the page JSON.parses).
func TestUIForemenData(t *testing.T) {
	e := newEnv(t)
	bearer := e.pair("alpha", map[string]string{"os": "linux"})

	// Exercise liveness: an authed read bumps last_seen.
	if status := e.do("GET", "/v1/inboxes/foreman:alpha/messages", bearer, nil, nil); status != http.StatusOK {
		t.Fatalf("read inbox status = %d", status)
	}

	var foremen []struct {
		ID           string `json:"id"`
		Online       bool   `json:"online"`
		Capabilities string `json:"capabilities"`
		LastSeen     string `json:"last_seen"`
	}
	if status := e.do("GET", "/v1/foremen", "", nil, &foremen); status != http.StatusOK {
		t.Fatalf("GET /v1/foremen status = %d", status)
	}
	if len(foremen) != 1 || foremen[0].ID != "foreman:alpha" {
		t.Fatalf("foremen = %+v", foremen)
	}
	f := foremen[0]
	if !f.Online {
		t.Error("foreman:alpha should be online after authed read")
	}
	if f.LastSeen == "" {
		t.Error("last_seen should be set after authed read")
	}
	// capabilities is a JSON-encoded string; the page parses it.
	var caps map[string]string
	if err := json.Unmarshal([]byte(f.Capabilities), &caps); err != nil {
		t.Fatalf("capabilities %q not a JSON string: %v", f.Capabilities, err)
	}
	if caps["os"] != "linux" {
		t.Errorf("caps = %+v", caps)
	}

	// Detail view: jobs filter by foreman.
	var jobs []map[string]any
	if status := e.do("GET", "/v1/jobs?foreman=foreman:alpha", "", nil, &jobs); status != http.StatusOK {
		t.Fatalf("GET /v1/jobs status = %d", status)
	}
	if jobs == nil {
		jobs = []map[string]any{}
	}
	if len(jobs) != 0 {
		t.Errorf("expected no jobs yet, got %d", len(jobs))
	}
}

// TestUIInboxRedesignShell pins the element ids and endpoint strings the
// S4 UI depends on (inbox redesign I3-I6). No browser runner: this guards
// the contract between the page and the S2/S3 endpoints.
func TestUIInboxRedesignShell(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	for _, want := range []string{
		// panes and controls
		`id="alerts"`, `id="tasklist"`, `id="chatlog"`, `id="newtaskform"`,
		`id="only-active"`, `id="needs-input"`, `id="tp-toggle"`, `id="modetoggle"`, `id="waiting-count"`, `id="kbhelp"`, `Nothing needs your input`, `id="task-search"`, `id="show-internal"`, `id="chatform"`,
		// tabs stay
		`id="tab-foremen"`, `id="tab-jobs"`, `id="tab-inboxes"`,
		// contract endpoints
		`/v1/tasks`, `/v1/alerts`, `/ack"`, `/reply"`, `?internal=1`,
		`status=`, `/v1/chat`, `task_id`,
		// SSE hint stream + legacy endpoints
		`EventSource`, `/v1/inboxes/planner:main/stream`, `/v1/foremen`, `/v1/jobs`, `/v1/inboxes`,
		// graceful degradation
		`tasksAvailable`, `alertsAvailable`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("UI page missing %q", want)
		}
	}
	// Decision 2: no job chips in the default thread view.
	if strings.Contains(body, "job-chip") {
		t.Error("UI must not render job chips")
	}
}

// TestUIRenameHooks: the served page carries the in-place task rename UI
// (no browser/JS runtime exists in CI, so this only pins the markup, the
// PATCH call and the key handling; the behaviour is in spec §9.5.5).
func TestUIRenameHooks(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`id="thread-title"`, `id="title-edit"`, `id="title-input"`, `role="alert"`,
		`method: "PATCH"`, `"/v1/tasks/" + encodeURIComponent(t.id)`,
		`ev.key === "Escape"`, `inp.onblur`, `if (titleEdit)`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("UI page missing %q", want)
		}
	}
}

// TestUIPhoneAssets: the home-screen assets are served with the right
// content types and the manifest is valid JSON pointing at them.
func TestUIPhoneAssets(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		path, ct, prefix string
	}{
		{"/manifest.webmanifest", "application/manifest+json", "{"},
		{"/apple-touch-icon.png", "image/png", "\x89PNG"},
		{"/icon-512.png", "image/png", "\x89PNG"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(e.ts.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != tc.ct || !strings.HasPrefix(string(b), tc.prefix) {
				t.Fatalf("status %d, content-type %q, %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(b))
			}
			if tc.ct != "application/manifest+json" {
				return
			}
			var m struct {
				Display string `json:"display"`
				Icons   []struct {
					Src string `json:"src"`
				} `json:"icons"`
			}
			if err := json.Unmarshal(b, &m); err != nil || m.Display != "standalone" || len(m.Icons) != 2 {
				t.Fatalf("manifest = %s (%v)", b, err)
			}
		})
	}
}

// TestUIPhoneShell pins the phone layout's iOS essentials and hooks: the
// page must keep these for the phone UI to work (string pins; the layout is
// exercised in a browser by hand).
func TestUIPhoneShell(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	for _, want := range []string{
		"viewport-fit=cover", `rel="manifest"`, `rel="apple-touch-icon"`, "apple-mobile-web-app-capable",
		"env(safe-area-inset-bottom", "visualViewport", "--kb",
		`id="phone"`, `id="p-tabs"`, `id="p-composer"`, `id="p-sheet"`, `id="p-fab"`,
		"html.phone", "factory.layout", "(max-width: 760px)",
		"preview_from", `/close"`, "needsYou", "touchstart",
		`#/inbox`, `"task"`, "history.pushState",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("UI page missing %q", want)
		}
	}
	// "Needs you" and the Inbox badge come from the server flag only
	// (spec §9.5.2), not from the old preview_from heuristic.
	for _, want := range []string{
		`const needsYou = (t) => t.status === "open" && !!t.needs_operator;`,
		"P.alerts.length + P.tasks.filter(needsYou).length",
		`t.status === "open" && t.needs_operator ? '<span class="needs"`, // desktop "needs input" badge
		"/v1/tasks?status=open&needs_operator=true&limit=500",            // focus/triage queue
		`"&needs_operator=true"`,                                         // needs-input filter
	} {
		if !strings.Contains(body, want) {
			t.Errorf("UI page missing %q", want)
		}
	}
	if strings.Contains(body, "awaiting=operator") || strings.Contains(body, `t.awaiting`) {
		t.Error("desktop needs-input must use needs_operator, not the awaiting heuristic")
	}
	if strings.Contains(body, `data-act="all-read"`) {
		t.Error(`"Mark all read" must not clear the server-side needs_operator section`)
	}
}

// TestUICopyPaste pins the copy/paste behaviour of the operator UI: guarded
// re-renders (so a live refresh cannot destroy a selection), copy buttons for
// code blocks and messages, and the plain-http clipboard fallback.
func TestUICopyPaste(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	for _, want := range []string{
		"function setHTML(",                           // selection-safe render helper
		"function selectionIn(",                       // non-collapsed selection check
		`setHTML(log, shown.length`,                   // desktop thread uses it
		"setHTML(el, envs.length",                     // legacy chat uses it
		`selectionIn($("p-body"))`,                    // phone thread uses it
		`setHTML($("list"), table(`,                   // polled tables use it
		`document.addEventListener("selectionchange"`, // deferred render re-runs
		"document.execCommand(\"copy\")",              // http/LAN fallback
		"navigator.clipboard",                         // secure-context path
		"new MutationObserver",                        // dynamic content gets buttons
		`"pre:not(.has-copy)"`,                        // every code block
		"}, 1500)",                                    // "Copied" feedback duration
		`"Copied"`,
		"data-raw", // whole-message copy source
		".sid { -webkit-user-select: all; user-select: all; }",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("UI page missing %q", want)
		}
	}
	// Nothing bigger than a short id may be user-select:all or none.
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "user-select: all") && !strings.Contains(line, ".sid") {
			t.Errorf("user-select:all on a non-id selector: %s", strings.TrimSpace(line))
		}
	}
}
