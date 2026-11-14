package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, auth Authenticator) (*Service, http.Handler) {
	t.Helper()
	svc := openTemp(t)
	return svc, NewHandler(svc, auth)
}

func doJSON(t *testing.T, h http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	// Bound every request so long-poll query-param tests cannot hang.
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	t.Cleanup(cancel)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode response (%d): %v: %s", rec.Code, err, rec.Body.String())
	}
}

func TestHTTPRoundTrip(t *testing.T) {
	_, h := newTestServer(t, nil)

	rec := doJSON(t, h, "POST", "/v1/inboxes/foreman:alpha/messages", map[string]any{
		"type":    "job.result",
		"job_id":  "job_abc",
		"from":    "worker:1",
		"payload": map[string]any{"status": "done"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		ID      string          `json:"id"`
		TS      string          `json:"ts"`
		Payload json.RawMessage `json:"payload"`
	}
	decode(t, rec, &env)
	if !strings.HasPrefix(env.ID, "msg_") {
		t.Errorf("id = %q, want msg_ prefix", env.ID)
	}
	if env.TS == "" {
		t.Error("ts not assigned")
	}
	if string(env.Payload) != `{"status":"done"}` {
		t.Errorf("payload = %s, want preserved object", env.Payload)
	}

	// Visible via GET.
	rec = doJSON(t, h, "GET", "/v1/inboxes/foreman:alpha/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	var envs []map[string]any
	decode(t, rec, &envs)
	if len(envs) != 1 || envs[0]["id"] != env.ID {
		t.Fatalf("GET returned %v", envs)
	}

	// Replay by id.
	rec = doJSON(t, h, "GET", "/v1/inboxes/foreman:alpha/messages/"+env.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("single GET status = %d", rec.Code)
	}

	// Ack, then double-ack: both 204.
	ackBody := map[string]any{"ids": []string{env.ID}}
	if rec := doJSON(t, h, "POST", "/v1/inboxes/foreman:alpha/ack", ackBody); rec.Code != http.StatusNoContent {
		t.Fatalf("ack status = %d, want 204", rec.Code)
	}
	if rec := doJSON(t, h, "POST", "/v1/inboxes/foreman:alpha/ack", ackBody); rec.Code != http.StatusNoContent {
		t.Fatalf("double-ack status = %d, want 204", rec.Code)
	}

	// Emptied by ack.
	rec = doJSON(t, h, "GET", "/v1/inboxes/foreman:alpha/messages", nil)
	var after []map[string]any
	decode(t, rec, &after)
	if len(after) != 0 {
		t.Fatalf("after ack, got %d envelopes", len(after))
	}
}

func TestHTTPQueryValidation(t *testing.T) {
	_, h := newTestServer(t, nil)

	cases := []struct {
		query  string
		wantOK bool
	}{
		{"", true}, // defaults
		{"?limit=1", true},
		{"?limit=500", true},
		{"?limit=0", false},
		{"?limit=501", false},
		{"?limit=abc", false},
		{"?wait_seconds=0", true},
		{"?wait_seconds=60", true},
		{"?wait_seconds=-1", false},
		{"?wait_seconds=61", false},
		{"?wait_seconds=abc", false},
	}
	for _, tc := range cases {
		rec := doJSON(t, h, "GET", "/v1/inboxes/foreman:alpha/messages"+tc.query, nil)
		if tc.wantOK && rec.Code != http.StatusOK {
			t.Errorf("%q: status = %d, want 200: %s", tc.query, rec.Code, rec.Body.String())
		}
		if !tc.wantOK && rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", tc.query, rec.Code)
		}
	}
}

func TestHTTP409Shape(t *testing.T) {
	_, h := newTestServer(t, nil)

	body := map[string]any{"type": "job.result", "job_id": "job_abc", "from": "worker:1", "payload": map[string]any{"n": 1}}
	rec := doJSON(t, h, "POST", "/v1/inboxes/factory:main/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first post status = %d", rec.Code)
	}
	var first struct {
		ID string `json:"id"`
	}
	decode(t, rec, &first)

	rec = doJSON(t, h, "POST", "/v1/inboxes/factory:main/messages", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", rec.Code)
	}
	var errBody struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	decode(t, rec, &errBody)
	if errBody.Error.Code != "duplicate" {
		t.Errorf("code = %q, want duplicate", errBody.Error.Code)
	}
	if !strings.Contains(errBody.Error.Message, first.ID) {
		t.Errorf("message %q does not carry original id %q", errBody.Error.Message, first.ID)
	}
}

func TestHTTPNotFoundShape(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := doJSON(t, h, "GET", "/v1/inboxes/foreman:alpha/messages/msg_missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode(t, rec, &errBody)
	if errBody.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", errBody.Error.Code)
	}
}

func TestHTTPBadEnvelope(t *testing.T) {
	_, h := newTestServer(t, nil)
	// Missing required client-side fields (from).
	rec := doJSON(t, h, "POST", "/v1/inboxes/factory:main/messages", map[string]any{"type": "job.event"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	// job-scoped type without job_id.
	rec = doJSON(t, h, "POST", "/v1/inboxes/factory:main/messages", map[string]any{"type": "job.result", "from": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("job-scoped without job_id: status = %d, want 400", rec.Code)
	}
}

type denyAll struct{}

func (denyAll) Authorize(*http.Request, string, string) error {
	return fmt.Errorf("denied")
}

type unauthorized struct{}

func (unauthorized) Error() string   { return "bad token" }
func (unauthorized) HTTPStatus() int { return http.StatusUnauthorized }

func TestHTTPAuthenticatorSeam(t *testing.T) {
	_, h := newTestServer(t, denyAll{})
	rec := doJSON(t, h, "POST", "/v1/inboxes/factory:main/messages", map[string]any{"type": "job.event", "from": "x"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decode(t, rec, &errBody)
	if errBody.Error.Code != "forbidden" {
		t.Errorf("code = %q, want forbidden", errBody.Error.Code)
	}

	// A StatusError carrying 401 maps to 401 unauthorized (spec §7.3).
	_, h2 := newTestServer(t, unauthorized{})
	rec = doJSON(t, h2, "GET", "/v1/inboxes/factory:main/messages", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var errBody2 struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decode(t, rec, &errBody2)
	if errBody2.Error.Code != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", errBody2.Error.Code)
	}
}
