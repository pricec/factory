package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/inbox"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// fakeClock starts at a fixed time and advances only when moved.
func fakeClock() (now *time.Time) {
	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	return &base
}

func pairForeman(t *testing.T, s *Store, selfID string) (bearer, inboxID string) {
	t.Helper()
	ctx := context.Background()
	token, _, err := s.IssuePairingToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bearer, inboxID, err = s.PairForeman(ctx, token, selfID)
	if err != nil {
		t.Fatalf("PairForeman: %v", err)
	}
	return bearer, inboxID
}

func TestPairFlowHappy(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	token, expires, err := s.IssuePairingToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(expires); d < 14*time.Minute || d > 16*time.Minute {
		t.Errorf("pairing TTL = %v, want ~15min", d)
	}
	bearer, inboxID, err := s.PairForeman(ctx, token, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if inboxID != "foreman:alpha" {
		t.Errorf("inboxID = %q, want foreman:alpha", inboxID)
	}
	if len(bearer) != 64 { // 32B hex
		t.Errorf("bearer length = %d, want 64 hex chars", len(bearer))
	}
}

func TestPairTokenExpired(t *testing.T) {
	s := openStore(t)
	base := fakeClock()
	s.Now = func() time.Time { return *base }

	ctx := context.Background()
	token, _, err := s.IssuePairingToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	*base = base.Add(16 * time.Minute) // past the 15-min TTL
	if _, _, err := s.PairForeman(ctx, token, "alpha"); err == nil {
		t.Fatal("pair with expired token: want error")
	} else if !isUnauthorized(err) {
		t.Fatalf("want UnauthorizedError, got %T: %v", err, err)
	}
}

func TestPairTokenReused(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	token, _, err := s.IssuePairingToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PairForeman(ctx, token, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PairForeman(ctx, token, "beta"); err == nil {
		t.Fatal("pair with reused token: want error")
	} else if !isUnauthorized(err) {
		t.Fatalf("want UnauthorizedError, got %T: %v", err, err)
	}
}

// scopeMatrix mounts a real inbox behind the real Authenticator and probes
// every pinned scope case (spec §7.3 test list).
func TestScopeMatrix(t *testing.T) {
	store := openStore(t)
	svc, err := inbox.Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })

	foremanBearer, foremanInbox := pairForeman(t, store, "alpha")
	workerBearer, err := store.MintWorkerToken(context.Background(), foremanInbox, "job_abc")
	if err != nil {
		t.Fatal(err)
	}

	authn := NewAuthenticator(store)
	handler := inbox.NewHandler(svc, authn)

	cases := []struct {
		name   string
		bearer string
		method string
		path   string
		want   int
	}{
		{"foreman posts to own inbox", foremanBearer, "POST", "/v1/inboxes/foreman:alpha/messages", 201},
		{"foreman reads own inbox", foremanBearer, "GET", "/v1/inboxes/foreman:alpha/messages", 200},
		{"foreman posts to factory:main", foremanBearer, "POST", "/v1/inboxes/factory:main/messages", 201},
		{"foreman posts to foreign inbox", foremanBearer, "POST", "/v1/inboxes/foreman:beta/messages", 403},
		{"foreman reads foreign inbox", foremanBearer, "GET", "/v1/inboxes/foreman:beta/messages", 403},

		{"worker posts result to foreman inbox", workerBearer, "POST", "/v1/inboxes/foreman:alpha/messages", 201},
		{"worker reads job inbox", workerBearer, "GET", "/v1/inboxes/job:job_abc/messages", 200},
		{"worker posts to job inbox", workerBearer, "POST", "/v1/inboxes/job:job_abc/messages", 403},
		{"worker reads foreman inbox", workerBearer, "GET", "/v1/inboxes/foreman:alpha/messages", 403},

		{"worker acks without read on foreman inbox", workerBearer, "POST", "/v1/inboxes/foreman:alpha/ack", 403},
		{"no bearer", "", "GET", "/v1/inboxes/foreman:alpha/messages", 401},
		{"unknown bearer", "deadbeef", "GET", "/v1/inboxes/foreman:alpha/messages", 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(envelopeBody(t, tc.method)))
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// Foreman ack requires may_read on the inbox: foreman has it -> 204.
	// Grab the posted message id from the foreman inbox first.
	envs := getJSON(t, handler, foremanBearer, "/v1/inboxes/foreman:alpha/messages")
	ids := make([]string, 0, len(envs))
	for _, e := range envs {
		ids = append(ids, str(e["id"]))
	}
	ackBody, _ := json.Marshal(map[string]any{"ids": ids})
	req := httptest.NewRequest("POST", "/v1/inboxes/foreman:alpha/ack", strings.NewReader(string(ackBody)))
	req.Header.Set("Authorization", "Bearer "+foremanBearer)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("foreman ack status = %d, want 204", rec.Code)
	}
}

func TestRevocation(t *testing.T) {
	store := openStore(t)
	bearer, inboxID := pairForeman(t, store, "alpha")
	svc, _ := inbox.Open(filepath.Join(t.TempDir(), "inbox.db"))
	t.Cleanup(func() { svc.Close() })
	handler := inbox.NewHandler(svc, NewAuthenticator(store))

	req := httptest.NewRequest("GET", "/v1/inboxes/"+inboxID+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("before revoke: status = %d", rec.Code)
	}

	if err := store.Revoke(context.Background(), inboxID); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/v1/inboxes/"+inboxID+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("after revoke: status = %d, want 401", rec.Code)
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.Error.Code != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", errBody.Error.Code)
	}
}

func TestInvalidateJobKillsWorkerToken(t *testing.T) {
	store := openStore(t)
	_, foremanInbox := pairForeman(t, store, "alpha")
	workerBearer, err := store.MintWorkerToken(context.Background(), foremanInbox, "job_abc")
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := inbox.Open(filepath.Join(t.TempDir(), "inbox.db"))
	t.Cleanup(func() { svc.Close() })
	handler := inbox.NewHandler(svc, NewAuthenticator(store))

	post := func() int {
		req := httptest.NewRequest("POST", "/v1/inboxes/foreman:alpha/messages",
			strings.NewReader(`{"type":"job.event","job_id":"job_abc","from":"worker:job_abc"}`))
		req.Header.Set("Authorization", "Bearer "+workerBearer)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(); code != 201 {
		t.Fatalf("before invalidation: %d", code)
	}
	if err := store.InvalidateJob(context.Background(), "job_abc"); err != nil {
		t.Fatal(err)
	}
	if code := post(); code != 401 {
		t.Fatalf("after invalidation: %d, want 401", code)
	}
}

func TestNoPlaintextBearerAtRest(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	bearer, _ := pairForeman(t, store, "alpha")
	workerBearer, err := store.MintWorkerToken(ctx, "foreman:alpha", "job_abc")
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+dbPath(t, store))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT token_hash FROM credentials`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	if len(hashes) < 2 {
		t.Fatalf("want >= 2 stored credential hashes, got %d", len(hashes))
	}
	for _, token := range []string{bearer, workerBearer} {
		for _, h := range hashes {
			if h == token {
				t.Errorf("plaintext bearer stored: %s", token)
			}
		}
	}
}

func TestActivityHookFiresOnSuccess(t *testing.T) {
	store := openStore(t)
	bearer, inboxID := pairForeman(t, store, "alpha")
	svc, _ := inbox.Open(filepath.Join(t.TempDir(), "inbox.db"))
	t.Cleanup(func() { svc.Close() })

	authn := NewAuthenticator(store)
	got := make(chan string, 4)
	authn.SetActivityHook(func(identity string) { got <- identity })
	handler := inbox.NewHandler(svc, authn)

	req := httptest.NewRequest("GET", "/v1/inboxes/"+inboxID+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	select {
	case id := <-got:
		if id != inboxID {
			t.Errorf("activity identity = %q, want %q", id, inboxID)
		}
	default:
		t.Fatal("activity hook did not fire")
	}
}

func TestPairHandlerFlow(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	token, _, err := store.IssuePairingToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	handler := PairHandler(store, "http://factory:8080")

	body := `{"pairing_token":"` + token + `","self":{"id":"alpha","capabilities":{"gpu":"true"}}}`
	req := httptest.NewRequest("POST", "/v1/pair", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("pair status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Bearer    string `json:"bearer"`
		InboxID   string `json:"inbox_id"`
		ParentURL string `json:"parent_url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.InboxID != "foreman:alpha" || resp.ParentURL != "http://factory:8080" {
		t.Errorf("pair response = %+v", resp)
	}

	// Bad body -> 400.
	req = httptest.NewRequest("POST", "/v1/pair", strings.NewReader("not json"))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("malformed pair: %d, want 400", rec.Code)
	}
	// Missing self.id -> 400.
	req = httptest.NewRequest("POST", "/v1/pair", strings.NewReader(`{"pairing_token":"x"}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("missing self.id: %d, want 400", rec.Code)
	}
	// Invalid token -> 401.
	req = httptest.NewRequest("POST", "/v1/pair", strings.NewReader(`{"pairing_token":"bogus","self":{"id":"alpha"}}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("bogus token: %d, want 401", rec.Code)
	}
}

// helpers -------------------------------------------------------------

func envelopeBody(t *testing.T, method string) string {
	t.Helper()
	if method != "POST" {
		return ""
	}
	return `{"type":"job.event","job_id":"job_abc","from":"worker:job_abc"}`
}

func getJSON(t *testing.T, h http.Handler, bearer, path string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v (%s)", path, err, rec.Body.String())
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// dbPath returns the sqlite file path behind an open store.
func dbPath(t *testing.T, s *Store) string {
	t.Helper()
	return s.path
}
