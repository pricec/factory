package notify

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/wire"
)

// newTestServer spins up a realistic in-process factory: a real inbox
// Service + REST handler, a Hub attached via the publish hook, and the
// stream handler mounted at its own path. hubMod and wrap are optional.
func newTestServer(t *testing.T, hubMod func(*Hub), wrap func(http.Handler) http.Handler) (*inbox.Service, *Hub, *httptest.Server) {
	t.Helper()
	svc, err := inbox.Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatalf("open inbox: %v", err)
	}
	t.Cleanup(func() { svc.Close() })

	hub := NewHub()
	if hubMod != nil {
		hubMod(hub)
	}
	hub.AttachTo(svc) // the integration wiring: Post -> hook -> Publish

	mux := http.NewServeMux()
	mux.Handle("/v1/", inbox.NewHandler(svc, nil))
	mux.Handle("GET /v1/inboxes/{id}/stream", StreamHandler(hub, nil))
	var h http.Handler = mux
	if wrap != nil {
		h = wrap(mux)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return svc, hub, srv
}

// openStream connects one SSE stream to the test server.
func openStream(t *testing.T, srv *httptest.Server, inboxID string) *http.Response {
	t.Helper()
	resp, err := http.Get(srv.URL + "/v1/inboxes/" + url.PathEscape(inboxID) + "/stream")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("stream Content-Type = %q, want text/event-stream", ct)
	}
	return resp
}

// sseLines forwards raw lines from r on a channel, closing it at EOF.
func sseLines(t *testing.T, r io.Reader) <-chan string {
	t.Helper()
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

func nextLine(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatal("stream closed unexpectedly")
		}
		return l
	case <-time.After(2 * time.Second):
		t.Fatal("no SSE line within 2s")
		return ""
	}
}

func noLine(t *testing.T, lines <-chan string, wait time.Duration) {
	t.Helper()
	select {
	case l := <-lines:
		t.Fatalf("unexpected SSE line %q", l)
	case <-time.After(wait):
	}
}

// waitSubscribers polls until the hub reports n subscribers for inboxID.
func waitSubscribers(t *testing.T, hub *Hub, inboxID string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for hub.Subscribers(inboxID) != n {
		if time.Now().After(deadline) {
			t.Fatalf("subscribers for %s = %d, want %d", inboxID, hub.Subscribers(inboxID), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// envelopesEqual compares envelopes field-by-field (json.RawMessage is not
// comparable with ==).
func envelopesEqual(a, b wire.MessageEnvelope) bool {
	return a.ID == b.ID && a.Type == b.Type && a.JobID == b.JobID &&
		a.From == b.From && a.TS == b.TS && string(a.Payload) == string(b.Payload)
}

// postEnvelope posts env through the real REST API and returns the stored
// envelope (server-assigned id/ts).
func postEnvelope(t *testing.T, srv *httptest.Server, inboxID string, env wire.MessageEnvelope) wire.MessageEnvelope {
	t.Helper()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/v1/inboxes/"+url.PathEscape(inboxID)+"/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("post envelope: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post status = %d, want 201", resp.StatusCode)
	}
	var full wire.MessageEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&full); err != nil {
		t.Fatalf("decode posted envelope: %v", err)
	}
	return full
}

func TestStreamEventFiresOnPost(t *testing.T) {
	const inboxID = "foreman:alpha"
	_, hub, srv := newTestServer(t, nil, nil)

	// Two concurrent subscribers on the same inbox.
	respA := openStream(t, srv, inboxID)
	respB := openStream(t, srv, inboxID)
	linesA := sseLines(t, respA.Body)
	linesB := sseLines(t, respB.Body)
	waitSubscribers(t, hub, inboxID, 2)
	streams := map[string]<-chan string{"A": linesA, "B": linesB}

	// Initial heartbeat comment on connect (each stream).
	for name, lines := range streams {
		if l := nextLine(t, lines); l != ":hb" {
			t.Fatalf("stream %s: first line = %q, want :hb", name, l)
		}
	}

	env := wire.MessageEnvelope{
		Type:    "brain.wake",
		From:    "factory:main",
		Payload: []byte(`{"hello":"world"}`),
	}
	full := postEnvelope(t, srv, inboxID, env)

	// Both streams see event: message / data: <full envelope> / blank line.
	for name, lines := range streams {
		if l := nextLine(t, lines); l != "event: message" {
			t.Fatalf("stream %s: event line = %q, want %q", name, l, "event: message")
		}
		data := nextLine(t, lines)
		if !strings.HasPrefix(data, "data: ") {
			t.Fatalf("stream %s: data line = %q, want data: <envelope JSON>", name, data)
		}
		if l := nextLine(t, lines); l != "" {
			t.Fatalf("stream %s: expected blank dispatch line, got %q", name, l)
		}
		var got wire.MessageEnvelope
		if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &got); err != nil {
			t.Fatalf("stream %s: data is not envelope JSON: %v: %q", name, err, data)
		}
		if !envelopesEqual(got, full) {
			t.Fatalf("stream %s: data = %+v, want full envelope %+v", name, got, full)
		}
	}

	// Posts to a different inbox must not leak onto this stream.
	postEnvelope(t, srv, "foreman:other", env)
	noLine(t, linesA, 150*time.Millisecond)
}

func TestStreamHeartbeat(t *testing.T) {
	_, _, srv := newTestServer(t, func(h *Hub) { h.Heartbeat = 50 * time.Millisecond }, nil)
	resp := openStream(t, srv, "foreman:alpha")
	lines := sseLines(t, resp.Body)

	// Initial heartbeat on connect, then the periodic one within ~1s: at
	// the default 15s interval the second would not arrive in time.
	if l := nextLine(t, lines); l != ":hb" {
		t.Fatalf("first line = %q, want :hb", l)
	}
	start := time.Now()
	if l := nextLine(t, lines); l != ":hb" {
		t.Fatalf("second line = %q, want :hb", l)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("periodic heartbeat took %v, want ~50ms", elapsed)
	}
}

func TestStreamUnsubscribeOnDisconnect(t *testing.T) {
	const inboxID = "foreman:alpha"
	_, hub, srv := newTestServer(t, nil, nil)

	resp := openStream(t, srv, inboxID)
	waitSubscribers(t, hub, inboxID, 1)

	resp.Body.Close() // client goes away
	waitSubscribers(t, hub, inboxID, 0)
}

func TestStreamObserverConnectDisconnect(t *testing.T) {
	const inboxID = "foreman:alpha"
	_, hub, srv := newTestServer(t, nil, nil)

	obs := &chanObserver{
		connect:    make(chan string, 8),
		disconnect: make(chan string, 8),
		activity:   make(chan string, 8),
	}
	hub.SetObserver(obs)

	resp := openStream(t, srv, inboxID)
	waitSubscribers(t, hub, inboxID, 1)
	expectEvent(t, obs.connect, "OnConnect", inboxID)
	expectNoEvent(t, obs.activity, "OnActivity", 150*time.Millisecond)

	resp.Body.Close()
	waitSubscribers(t, hub, inboxID, 0)
	expectEvent(t, obs.disconnect, "OnDisconnect", inboxID)
}

func TestStreamForbidden(t *testing.T) {
	h := StreamHandler(NewHub(), denyAuth{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/inboxes/foreman:alpha/stream", nil))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error.Code != "forbidden" {
		t.Errorf("code = %q, want forbidden", errBody.Error.Code)
	}
	if errBody.Error.Message == "" {
		t.Error("message is empty")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestHubPublishNeverBlocks(t *testing.T) {
	hub := NewHub()
	const inboxID = "foreman:alpha"

	publishDone := func() chan struct{} {
		done := make(chan struct{})
		go func() {
			hub.Publish(inboxID, wire.MessageEnvelope{ID: "msg_x"})
			close(done)
		}()
		return done
	}

	// Publishing with no subscribers is a fast no-op.
	select {
	case <-publishDone():
	case <-time.After(2 * time.Second):
		t.Fatal("Publish with no subscribers blocked")
	}

	// A full subscriber buffer drops instead of blocking.
	sub := hub.subscribe(inboxID)
	defer hub.unsubscribe(sub)
	for i := 0; i < subscriberBuffer; i++ {
		sub.ch <- wire.MessageEnvelope{ID: "msg_full"}
	}
	select {
	case <-publishDone():
	case <-time.After(2 * time.Second):
		t.Fatal("Publish with full buffer blocked")
	}
	if got := len(sub.ch); got != subscriberBuffer {
		t.Fatalf("subscriber buffer = %d, want capped at %d", got, subscriberBuffer)
	}
}

func TestHubMultipleInboxesIndependent(t *testing.T) {
	hub := NewHub()
	subA := hub.subscribe("foreman:alpha")
	defer hub.unsubscribe(subA)
	subB := hub.subscribe("foreman:beta")
	defer hub.unsubscribe(subB)

	hub.Publish("foreman:beta", wire.MessageEnvelope{ID: "msg_beta"})

	select {
	case env := <-subB.ch:
		if env.ID != "msg_beta" {
			t.Fatalf("beta got %q", env.ID)
		}
	default:
		t.Fatal("beta subscriber got nothing")
	}
	select {
	case env := <-subA.ch:
		t.Fatalf("alpha subscriber leaked event %q", env.ID)
	default:
	}
}

// chanObserver records Observer events on channels (buffered: late events
// must not stall the hub's fire-and-forget goroutines).
type chanObserver struct {
	connect    chan string
	disconnect chan string
	activity   chan string
}

func (o *chanObserver) OnConnect(identity string)    { o.connect <- identity }
func (o *chanObserver) OnDisconnect(identity string) { o.disconnect <- identity }
func (o *chanObserver) OnActivity(identity string, ts time.Time) {
	o.activity <- identity
}

func expectEvent(t *testing.T, ch <-chan string, what, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("%s identity = %q, want %q", what, got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no %s event within 2s", what)
	}
}

func expectNoEvent(t *testing.T, ch <-chan string, what string, wait time.Duration) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("unexpected %s(%q)", what, got)
	case <-time.After(wait):
	}
}

type denyAuth struct{}

func (denyAuth) Authorize(*http.Request, string, string) error {
	return errors.New("read denied for this token")
}
