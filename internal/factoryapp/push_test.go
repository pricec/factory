package factoryapp_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/factoryapp"
	"github.com/pricec/factory/internal/mcpfactory"
)

type pushRecv struct {
	mu   sync.Mutex
	hdrs []http.Header
	n    int
	code int
}

func (p *pushRecv) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.n }

// newPushEnv starts a factory whose push client talks to a fake TLS push
// service, and returns a valid subscription pointing at it.
func newPushEnv(t *testing.T) (*testEnv, *pushRecv, map[string]any) {
	t.Helper()
	rcv := &pushRecv{code: http.StatusCreated}
	svc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		rcv.mu.Lock()
		rcv.n++
		rcv.hdrs = append(rcv.hdrs, r.Header.Clone())
		code := rcv.code
		rcv.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(svc.Close)
	e := newEnv(t, func(c *factoryapp.Config) { c.PushHTTPClient = svc.Client(); c.PushAnyHost = true })
	// Pin the planner brain. Alerts and chat wake it, and an unpinned brain
	// shells out to a real CLI whose failure posts brain.reply + a P0 alert
	// (each a legitimate extra push) at an environment-dependent moment -
	// which made the push counts below flaky on CI. A planner that never
	// answers keeps the counts about the one thing under test.
	quiet := make(chan struct{})
	e.a.SetPlannerExec(func(ctx context.Context, _ string) (string, error) {
		select {
		case <-quiet:
		case <-ctx.Done():
		}
		return "", nil
	})
	t.Cleanup(func() { close(quiet) })
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	b := base64.RawURLEncoding
	sub := map[string]any{"endpoint": svc.URL + "/push/abc", "keys": map[string]string{"p256dh": b.EncodeToString(k.PublicKey().Bytes()), "auth": b.EncodeToString(auth)}}
	return e, rcv, sub
}

func pushWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPushKeyPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	get := func() string {
		e := newEnv(t, func(c *factoryapp.Config) { c.DataDir = dir })
		var out struct {
			PublicKey string `json:"public_key"`
		}
		if st := e.do("GET", "/v1/push/key", "", nil, &out); st != 200 {
			t.Fatalf("status %d", st)
		}
		raw, err := base64.RawURLEncoding.DecodeString(out.PublicKey)
		if err != nil || len(raw) != 65 || raw[0] != 4 {
			t.Fatalf("public key %q not an uncompressed P-256 point (%v)", out.PublicKey, err)
		}
		return out.PublicKey
	}
	first := get()
	if second := get(); second != first {
		t.Fatal("VAPID key regenerated after restart")
	}
}

func TestPushSubscribeValidation(t *testing.T) {
	e := newEnv(t) // real allowlist
	good := map[string]any{"endpoint": "https://web.push.apple.com/x", "keys": map[string]string{"p256dh": "x", "auth": "y"}}
	cases := []struct {
		name string
		body any
	}{
		{"garbage json", "{"},
		{"bad keys", good},
		{"http endpoint", map[string]any{"endpoint": "http://web.push.apple.com/x", "keys": map[string]string{}}},
	}
	for _, tc := range cases {
		if st := e.do("POST", "/v1/push/subscribe", "", tc.body, nil); st != 400 {
			t.Errorf("%s: status %d, want 400", tc.name, st)
		}
	}
	// Valid keys but an endpoint that is not a push service: SSRF guard.
	_, _, sub := newPushEnv(t)
	if st := e.do("POST", "/v1/push/subscribe", "", sub, nil); st != 400 {
		t.Errorf("non-push-service endpoint: status %d, want 400", st)
	}
	if st := e.do("POST", "/v1/push/unsubscribe", "", map[string]string{}, nil); st != 400 {
		t.Errorf("unsubscribe without endpoint: status %d", st)
	}
}

func TestPushSentForOperatorNotifications(t *testing.T) {
	e, rcv, sub := newPushEnv(t)
	if st := e.do("POST", "/v1/push/subscribe", "", sub, nil); st != 200 {
		t.Fatalf("subscribe: %d", st)
	}
	if _, err := e.a.RaisePlannerAlert(e.ctx, mcpfactory.RaiseAlertInput{Priority: 1, Title: "disk full"}); err != nil {
		t.Fatal(err)
	}
	pushWaitFor(t, "alert push", func() bool { return rcv.count() == 1 })
	rcv.mu.Lock()
	h := rcv.hdrs[0]
	rcv.mu.Unlock()
	if !strings.HasPrefix(h.Get("Authorization"), "vapid t=") || h.Get("Content-Encoding") != "aes128gcm" || h.Get("TTL") == "" {
		t.Fatalf("bad push headers: %v", h)
	}
	// The operator posting their own chat message does not push.
	if st := e.do("POST", "/v1/chat", "", map[string]string{"text": "hello"}, nil); st >= 500 {
		t.Fatalf("chat: %d", st)
	}
	time.Sleep(100 * time.Millisecond)
	if rcv.count() != 1 {
		t.Fatalf("chat.message pushed (%d sends)", rcv.count())
	}
	// Disabling stops pushes.
	if st := e.do("POST", "/v1/push/unsubscribe", "", map[string]string{"endpoint": sub["endpoint"].(string)}, nil); st != 200 {
		t.Fatalf("unsubscribe: %d", st)
	}
	if _, err := e.a.RaisePlannerAlert(e.ctx, mcpfactory.RaiseAlertInput{Priority: 1, Title: "again"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if rcv.count() != 1 {
		t.Fatalf("push sent after unsubscribe (%d sends)", rcv.count())
	}
}

func TestPushPrunesGoneSubscription(t *testing.T) {
	e, rcv, sub := newPushEnv(t)
	rcv.mu.Lock()
	rcv.code = http.StatusGone
	rcv.mu.Unlock()
	e.do("POST", "/v1/push/subscribe", "", sub, nil)
	_, _ = e.a.RaisePlannerAlert(e.ctx, mcpfactory.RaiseAlertInput{Priority: 1, Title: "one"})
	pushWaitFor(t, "first send", func() bool { return rcv.count() == 1 })
	time.Sleep(150 * time.Millisecond) // prune runs after the send returns
	_, _ = e.a.RaisePlannerAlert(e.ctx, mcpfactory.RaiseAlertInput{Priority: 1, Title: "two"})
	time.Sleep(150 * time.Millisecond)
	if rcv.count() != 1 {
		t.Fatalf("410 subscription not pruned (%d sends)", rcv.count())
	}
}

func TestPushDisabledByEnv(t *testing.T) {
	t.Setenv(factoryapp.EnvPushDisable, "1")
	e := newEnv(t)
	for _, p := range []string{"/v1/push/key"} {
		if st := e.do("GET", p, "", nil, nil); st != 404 {
			t.Errorf("%s: %d, want 404", p, st)
		}
	}
	b, _ := json.Marshal(map[string]string{})
	resp, err := http.Post(e.ts.URL+"/v1/push/subscribe", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("subscribe: %d, want 404", resp.StatusCode)
	}
}
