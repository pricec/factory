package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// ackingHandler returns a Handler (spec §4.5 step 3: process + ack) that
// acks each batch through the real REST API and records it on batches.
func ackingHandler(t *testing.T, srv *httptest.Server, inboxID string, batches chan<- []wire.MessageEnvelope) func(context.Context, []wire.MessageEnvelope) error {
	t.Helper()
	return func(ctx context.Context, envs []wire.MessageEnvelope) error {
		ids := make([]string, len(envs))
		for i, env := range envs {
			ids[i] = env.ID
		}
		body, err := json.Marshal(map[string]any{"ids": ids})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			srv.URL+"/v1/inboxes/"+url.PathEscape(inboxID)+"/ack", bytes.NewReader(body))
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("ack: status %d", resp.StatusCode)
		}
		batches <- envs
		return nil
	}
}

// runClient starts c.Run under a context canceled at test cleanup; the
// terminal error (if any) arrives on the returned channel.
func runClient(t *testing.T, c *Client) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- c.Run(ctx) }()
	return cancel, errCh
}

// collectBatch waits for the next handler batch.
func collectBatch(t *testing.T, batches <-chan []wire.MessageEnvelope) []wire.MessageEnvelope {
	t.Helper()
	select {
	case b := <-batches:
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("no handler batch within 2s")
		return nil
	}
}

// collectBatches gathers batches until stop is satisfied or the deadline.
func collectBatches(t *testing.T, batches <-chan []wire.MessageEnvelope, stop func(seen [][]wire.MessageEnvelope) bool) [][]wire.MessageEnvelope {
	t.Helper()
	var seen [][]wire.MessageEnvelope
	for {
		if stop(seen) {
			return seen
		}
		select {
		case b := <-batches:
			seen = append(seen, b)
		case <-time.After(2 * time.Second):
			t.Fatalf("batch condition not met; seen so far: %v", flattenIDs(seen))
			return nil
		}
	}
}

func noMoreBatches(t *testing.T, batches <-chan []wire.MessageEnvelope, wait time.Duration) {
	t.Helper()
	select {
	case b := <-batches:
		t.Fatalf("unexpected extra batch %+v", b)
	case <-time.After(wait):
	}
}

// flattenIDs returns every message id the handler saw, in order.
func flattenIDs(batches [][]wire.MessageEnvelope) []string {
	var ids []string
	for _, b := range batches {
		for _, env := range b {
			ids = append(ids, env.ID)
		}
	}
	return ids
}

// assertEachExactlyOnce fails unless the handler saw exactly want, each once.
func assertEachExactlyOnce(t *testing.T, batches [][]wire.MessageEnvelope, want ...string) {
	t.Helper()
	got := flattenIDs(batches)
	counts := make(map[string]int, len(got))
	for _, id := range got {
		counts[id]++
	}
	for _, w := range want {
		if counts[w] != 1 {
			t.Fatalf("message %s seen %d times across batches (%v), want exactly once", w, counts[w], got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("handler saw %v, want exactly %q", got, want)
	}
}

func hasIDs(seen [][]wire.MessageEnvelope, want ...string) bool {
	ids := make(map[string]bool)
	for _, b := range seen {
		for _, env := range b {
			ids[env.ID] = true
		}
	}
	for _, w := range want {
		if !ids[w] {
			return false
		}
	}
	return true
}

// snapshotGate parks the blockAt-th GET snapshot request (1-based) until
// release, so tests can interleave a POST deterministically between two
// client steps. The parked request's arrival is announced on arrived.
type snapshotGate struct {
	count   atomic.Int64
	blockAt int64
	arrived chan struct{}
	release chan struct{}
}

func newSnapshotGate(blockAt int64) *snapshotGate {
	return &snapshotGate{
		blockAt: blockAt,
		arrived: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (g *snapshotGate) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/messages") &&
			g.count.Add(1) == g.blockAt {
			g.arrived <- struct{}{}
			select {
			case <-g.release:
			case <-time.After(5 * time.Second): // test-failure watchdog
			}
		}
		next.ServeHTTP(w, r)
	})
}

func newTestClient(srv *httptest.Server, inboxID string, h func(context.Context, []wire.MessageEnvelope) error) *Client {
	return &Client{
		BaseURL: srv.URL,
		InboxID: inboxID,
		Handler: h,
		Backoff: func(int) time.Duration { return 5 * time.Millisecond },
		Rand:    func() float64 { return 0.5 }, // deterministic, no jitter spread
	}
}

func TestClientSnapshotOnConnect(t *testing.T) {
	const inboxID = "foreman:alpha"
	svc, hub, srv := newTestServer(t, nil, nil)

	ctx := context.Background()
	env1, err := svc.Post(ctx, inboxID, wire.MessageEnvelope{Type: "brain.wake", From: "factory:main", Payload: []byte(`{"n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	env2, err := svc.Post(ctx, inboxID, wire.MessageEnvelope{Type: "brain.wake", From: "factory:main", Payload: []byte(`{"n":2}`)})
	if err != nil {
		t.Fatal(err)
	}

	batches := make(chan []wire.MessageEnvelope, 8)
	cancel, errCh := runClient(t, newTestClient(srv, inboxID, ackingHandler(t, srv, inboxID, batches)))

	// Initial snapshot delivers both pre-existing messages in one FIFO batch.
	b1 := collectBatch(t, batches)
	if len(b1) != 2 || b1[0].ID != env1.ID || b1[1].ID != env2.ID {
		t.Fatalf("initial batch = %+v, want [%s %s] in FIFO order", b1, env1.ID, env2.ID)
	}
	noMoreBatches(t, batches, 150*time.Millisecond)

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}
	// Stream torn down with the run: no leaked subscriber.
	waitSubscribers(t, hub, inboxID, 0)
}

func TestClientWakeTriggersResnapshot(t *testing.T) {
	const inboxID = "foreman:alpha"
	_, hub, srv := newTestServer(t, nil, nil)

	batches := make(chan []wire.MessageEnvelope, 8)
	cancel, _ := runClient(t, newTestClient(srv, inboxID, ackingHandler(t, srv, inboxID, batches)))
	waitSubscribers(t, hub, inboxID, 1)

	m1 := postEnvelope(t, srv, inboxID, wire.MessageEnvelope{Type: "brain.wake", From: "factory:main"})
	m2 := postEnvelope(t, srv, inboxID, wire.MessageEnvelope{Type: "brain.wake", From: "factory:main"})

	// Wakes may coalesce (both messages in one batch) but every message
	// must arrive exactly once.
	seen := collectBatches(t, batches, func(seen [][]wire.MessageEnvelope) bool {
		return hasIDs(seen, m1.ID, m2.ID)
	})
	assertEachExactlyOnce(t, seen, m1.ID, m2.ID)

	cancel()
	noMoreBatches(t, batches, 150*time.Millisecond)
}

// TestClientLostWakeupRace proves the lost-wakeup guard (spec §4.5): a POST
// landing after stream-open but with the initial snapshot still in flight
// is still processed exactly once. The gate parks the initial snapshot so
// the POST provably interleaves between steps 1 and 2.
func TestClientLostWakeupRace(t *testing.T) {
	const inboxID = "foreman:alpha"
	gate := newSnapshotGate(1)
	_, hub, srv := newTestServer(t, nil, gate.middleware)

	batches := make(chan []wire.MessageEnvelope, 8)
	cancel, _ := runClient(t, newTestClient(srv, inboxID, ackingHandler(t, srv, inboxID, batches)))

	// Stream is open and the initial snapshot GET is parked: the
	// interleaving window is now guaranteed.
	select {
	case <-gate.arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("initial snapshot request never arrived at the gate")
	}

	// POST lands between stream-open and the snapshot completing: its
	// wake-up is buffered (the stream is subscribed) and its row commits
	// before the snapshot query runs.
	m1 := postEnvelope(t, srv, inboxID, wire.MessageEnvelope{Type: "brain.wake", From: "factory:main"})
	close(gate.release)

	b1 := collectBatch(t, batches)
	if len(b1) != 1 || b1[0].ID != m1.ID {
		t.Fatalf("batch = %+v, want exactly [%s]", b1, m1.ID)
	}

	// The buffered wake-up fires one more sync, which is empty (acked) and
	// must not re-deliver.
	noMoreBatches(t, batches, 250*time.Millisecond)

	// The stream never dropped: no reconnect was involved.
	if n := hub.Subscribers(inboxID); n != 1 {
		t.Fatalf("subscribers = %d, want 1 (stream should have survived)", n)
	}
	cancel()
}

// TestClientReconnectResync proves spec §4.6: after a server-side stream
// kill the client reconnects with backoff and resyncs via snapshot -
// catching a message posted while it was down, even though that wake-up
// was lost.
func TestClientReconnectResync(t *testing.T) {
	const inboxID = "foreman:alpha"
	// Park the reconnect resync (snapshot #2) so the dead-window POST
	// provably precedes it.
	gate := newSnapshotGate(2)
	svc, hub, srv := newTestServer(t, nil, gate.middleware)

	ctx := context.Background()
	m1, err := svc.Post(ctx, inboxID, wire.MessageEnvelope{Type: "brain.wake", From: "factory:main"})
	if err != nil {
		t.Fatal(err)
	}

	obs := &chanObserver{
		connect:    make(chan string, 8),
		disconnect: make(chan string, 8),
		activity:   make(chan string, 8),
	}
	hub.SetObserver(obs)

	batches := make(chan []wire.MessageEnvelope, 8)
	cancel, _ := runClient(t, newTestClient(srv, inboxID, ackingHandler(t, srv, inboxID, batches)))

	// Snapshot #1 (initial, ungated) delivers m1 and the handler acks it.
	b1 := collectBatch(t, batches)
	if len(b1) != 1 || b1[0].ID != m1.ID {
		t.Fatalf("initial batch = %+v, want [%s]", b1, m1.ID)
	}
	expectEvent(t, obs.connect, "OnConnect", inboxID)
	waitSubscribers(t, hub, inboxID, 1)

	// Server kills the stream; the client reconnects (5ms backoff) and its
	// resync snapshot (#2) parks at the gate.
	if n := hub.closeInbox(inboxID); n != 1 {
		t.Fatalf("closeInbox closed %d streams, want 1", n)
	}
	expectEvent(t, obs.disconnect, "OnDisconnect", inboxID)
	waitSubscribers(t, hub, inboxID, 1)
	expectEvent(t, obs.connect, "OnConnect", inboxID) // reconnect observed
	select {
	case <-gate.arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect resync snapshot never arrived at the gate")
	}

	// m2 is posted while no wake-up can help: the stream just re-opened and
	// the resync is parked, so only the resync can catch it.
	m2, err := svc.Post(ctx, inboxID, wire.MessageEnvelope{Type: "brain.wake", From: "factory:main"})
	if err != nil {
		t.Fatal(err)
	}
	close(gate.release)

	b2 := collectBatch(t, batches)
	if len(b2) != 1 || b2[0].ID != m2.ID {
		t.Fatalf("resync batch = %+v, want exactly [%s]", b2, m2.ID)
	}
	assertEachExactlyOnce(t, [][]wire.MessageEnvelope{b1, b2}, m1.ID, m2.ID)
	noMoreBatches(t, batches, 250*time.Millisecond)

	cancel()
	waitSubscribers(t, hub, inboxID, 0)
}

// TestClientStreamRefusedRetries pins that connection failures are retried
// (not fatal) and that Run still exits on ctx cancel.
func TestClientStreamRefusedRetries(t *testing.T) {
	_, _, srv := newTestServer(t, nil, nil)
	deadURL := srv.URL
	srv.Close() // port now refuses connections

	batches := make(chan []wire.MessageEnvelope, 8)
	client := &Client{
		BaseURL: deadURL,
		InboxID: "foreman:alpha",
		Handler: ackingHandler(t, srv, "foreman:alpha", batches),
		Backoff: func(int) time.Duration { return 10 * time.Millisecond },
		Rand:    func() float64 { return 0.5 },
	}
	cancel, errCh := runClient(t, client)

	// Still retrying after 250ms: no exit, no batch.
	select {
	case err := <-errCh:
		t.Fatalf("Run gave up on refused stream: %v", err)
	case b := <-batches:
		t.Fatalf("unexpected batch %+v", b)
	case <-time.After(250 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}
}

func TestDefaultBackoffCurve(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 60 * time.Second}, // 1s<<6 = 64s hits the cap
		{7, 60 * time.Second},
		{30, 60 * time.Second},
		{-1, 1 * time.Second},
	}
	for _, tc := range cases {
		if got := DefaultBackoff(tc.attempt); got != tc.want {
			t.Errorf("DefaultBackoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestReconnectDelayJitter(t *testing.T) {
	// Deterministic Rand: factor = 0.75 + 0.5*r around the curve's base.
	cases := []struct {
		name    string
		base    time.Duration // injected curve value
		rand    func() float64
		attempt int
		want    time.Duration
	}{
		{"factor 1.0 at r=0.5", 2 * time.Second, func() float64 { return 0.5 }, 1, 2 * time.Second},
		{"low bound at r=0", 2 * time.Second, func() float64 { return 0 }, 1, 1500 * time.Millisecond},
		{"near high bound at r=0.9", 2 * time.Second, func() float64 { return 0.9 }, 1, 2400 * time.Millisecond},
		{"cap attempt 6", 60 * time.Second, func() float64 { return 0.5 }, 6, 60 * time.Second},
		{"injected curve is jittered too", 100 * time.Millisecond, func() float64 { return 0.25 }, 0, 87500 * time.Microsecond},
	}
	for _, tc := range cases {
		c := &Client{Backoff: func(int) time.Duration { return tc.base }, Rand: tc.rand}
		if got := c.reconnectDelay(tc.attempt); got != tc.want {
			t.Errorf("%s: reconnectDelay(%d) = %v, want %v", tc.name, tc.attempt, got, tc.want)
		}
	}

	// The default curve + default Rand keeps the delay within [0.75, 1.25]
	// of the base (spec §4.6).
	c := &Client{}
	for attempt := 0; attempt < 7; attempt++ {
		base := DefaultBackoff(attempt)
		for i := 0; i < 50; i++ {
			d := c.reconnectDelay(attempt)
			if d < 3*base/4 || d > 5*base/4 {
				t.Fatalf("reconnectDelay(%d) = %v, outside [0.75,1.25] of %v", attempt, d, base)
			}
		}
	}
}

func TestSSEParser(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		fired int
	}{
		{"heartbeat only", []string{":hb", ":hb"}, 0},
		{"message event", []string{"event: message", "data: {}", ""}, 1},
		{"no dispatch without blank line", []string{"event: message", "data: {}"}, 0},
		{"unnamed event defaults to message", []string{"data: {}", ""}, 1},
		{"other event type ignored", []string{"event: other", "data: {}", ""}, 0},
		{"event without data not dispatched", []string{"event: message", ""}, 0},
		{"multi-data line event", []string{"event: message", "data: a", "data: b", ""}, 1},
		{
			"full stream with interleaved heartbeats",
			[]string{":hb", "event: message", "data: x", "", ":hb", "event: message", "data: y", ""},
			2,
		},
		{"crlf lines", []string{":hb\r", "event: message\r", "data: {}\r", "\r"}, 1},
		// A bare "data" field name (no colon) is an empty-valued data
		// field per the SSE spec, so the event still dispatches.
		{"field without colon", []string{"data", ""}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p sseParser
			fired := 0
			for _, l := range tc.lines {
				if p.line(l) {
					fired++
				}
			}
			if fired != tc.fired {
				t.Fatalf("fired %d times, want %d", fired, tc.fired)
			}
		})
	}
}

// TestClientURLs pins the request targets (spec §4.1, §4.4).
func TestClientURLs(t *testing.T) {
	c := &Client{BaseURL: "http://factory:8080/", InboxID: "foreman:alpha"}
	if got, want := c.streamURL(), "http://factory:8080/v1/inboxes/foreman:alpha/stream"; got != want {
		t.Errorf("streamURL = %q, want %q", got, want)
	}
	if got, want := c.snapshotURL(), "http://factory:8080/v1/inboxes/foreman:alpha/messages?limit="+strconv.Itoa(DefaultSnapshotLimit); got != want {
		t.Errorf("snapshotURL = %q, want %q", got, want)
	}
	c.SnapshotLimit = 3
	if got, want := c.snapshotURL(), "http://factory:8080/v1/inboxes/foreman:alpha/messages?limit=3"; got != want {
		t.Errorf("snapshotURL = %q, want %q", got, want)
	}
	// A slash-bearing inbox id stays a single escaped path segment.
	unsafe := &Client{BaseURL: "http://f:1", InboxID: "a/b"}
	if !strings.Contains(unsafe.streamURL(), "/v1/inboxes/a%2Fb/stream") {
		t.Errorf("streamURL = %q, want escaped inbox segment", unsafe.streamURL())
	}
}
