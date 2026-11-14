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
