package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// Client-side constants (spec §4.5, §4.6, §4.1).
const (
	// DefaultSnapshotLimit is the snapshot GET's limit (spec §4.1 default).
	DefaultSnapshotLimit = 50
	// DefaultMaxReconnect caps the backoff curve at 60s (spec §4.6).
	DefaultMaxReconnect = 60 * time.Second
	// wakeBuffer bounds pending wake-ups inside one connection. Wake-ups
	// coalesce: a pending one already guarantees a snapshot resync, so a
	// full buffer drops extras.
	wakeBuffer = 16
	// maxEnvelopeBytes bounds one SSE line (envelopes are ~2KB payload,
	// spec §7; 1MB is generous headroom).
	maxEnvelopeBytes = 1 << 20
)

// DefaultBackoff returns the SSE reconnect curve (spec §4.6): 1s, 2s, 4s,
// ... capped at 60s. attempt is 0-based.
func DefaultBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= 6 { // 1s<<6 = 64s > 60s cap
		return DefaultMaxReconnect
	}
	return time.Second << attempt
}

// handlerError wraps a caller Handler failure so Run can distinguish it
// from transient connection errors (which are retried).
type handlerError struct{ err error }

func (e *handlerError) Error() string { return "notify: handler: " + e.err.Error() }
func (e *handlerError) Unwrap() error { return e.err }

// Client runs the subscribe-before-check loop (spec §4.5) against one
// inbox: open stream, GET snapshot, process+ack, wait for "message" events
// and re-snapshot. On any stream error or disconnect it backs off and
// reconnects, always resyncing via a fresh snapshot first.
type Client struct {
	// BaseURL is the server root, e.g. "http://factory:8080".
	BaseURL string
	// InboxID is the inbox to stream and snapshot.
	InboxID string
	// Token is the bearer token sent on every request; empty means no
	// Authorization header (dev/test servers).
	Token string
	// Handler receives each unacked snapshot batch (steps 3 of spec §4.5):
	// it processes and acks. It is caller-owned: Run returns its error
	// unchanged (the caller owns retry policy); empty batches are not
	// delivered.
	Handler func(ctx context.Context, envs []wire.MessageEnvelope) error

	// Backoff returns the base reconnect delay for attempt n (0-based);
	// nil means DefaultBackoff. The ±25% jitter (spec §4.6) is applied on
	// top via Rand.
	Backoff func(attempt int) time.Duration
	// Rand supplies jitter in [0,1); nil means math/rand/v2's global
	// source.
	Rand func() float64

	// HTTPClient issues requests; nil means http.DefaultClient. It must
	// not set Timeout, which would cap the long-lived stream; use request
	// contexts instead.
	HTTPClient *http.Client
	// SnapshotLimit is the snapshot GET limit; 0 means DefaultSnapshotLimit.
	SnapshotLimit int
}

// Run drives the loop until ctx is canceled (returns ctx.Err()) or the
// Handler fails (returns its error). Stream and snapshot failures are
// retried forever with backoff - every (re)connect begins with a fresh
// snapshot (spec §4.6). The backoff curve advances per consecutive failed
// cycle and resets after any cycle that completed its initial sync.
func (c *Client) Run(ctx context.Context) error {
	if c.BaseURL == "" {
		return errors.New("notify: client: BaseURL required")
	}
	if c.InboxID == "" {
		return errors.New("notify: client: InboxID required")
	}
	if c.Handler == nil {
		return errors.New("notify: client: Handler required")
	}

	attempt := 0 // consecutive cycles that never completed a sync
	for {
		synced, err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var he *handlerError
		if errors.As(err, &he) {
			return he.err // caller-owned policy: no retry here
		}
		if synced {
			attempt = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.reconnectDelay(attempt)):
		}
		attempt++
	}
}

// runOnce runs one connection cycle: open stream, sync snapshot, then keep
// syncing on wake-ups until the stream breaks. synced reports whether the
// cycle got through its initial sync. It returns nil only when ctx is
// done; every other return value means "reconnect with backoff". The
// stream body is closed on return, unparking the reader goroutine.
func (c *Client) runOnce(ctx context.Context) (bool, error) {
	wake := make(chan struct{}, wakeBuffer)
	done := make(chan error, 1)

	// 1. Open stream. The subscription is live once Do returns.
	resp, err := c.openStream(ctx, wake, done)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	// 2-4. Snapshot now, then after every wake-up (spec §4.5).
	if err := c.sync(ctx); err != nil {
		return false, err
	}
	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case err := <-done: // stream error/EOF: reconnect (and thus resync)
			return true, err
		case <-wake:
			if err := c.sync(ctx); err != nil {
				return true, err
			}
		}
	}
}

// openStream starts the GET stream and spawns the reader that turns SSE
// "message" events into wake-ups.
func (c *Client) openStream(ctx context.Context, wake chan<- struct{}, done chan<- error) (*http.Response, error) {
	resp, err := c.httpDo(ctx, http.MethodGet, c.streamURL())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("notify: stream: status %d", resp.StatusCode)
	}
	go c.readStream(resp.Body, wake, done)
	return resp, nil
}

// readStream parses the SSE body and forwards "message" events as wake-ups.
// It reports the stream outcome on done exactly once (buffered).
func (c *Client) readStream(body io.Reader, wake chan<- struct{}, done chan<- error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), maxEnvelopeBytes)
	var p sseParser
	for sc.Scan() {
		if p.line(sc.Text()) {
			select {
			case wake <- struct{}{}:
			default: // coalesce: a pending wake-up already guarantees resync
			}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF // clean server close: still a disconnect
	}
	done <- err
}

// sync fetches the snapshot and hands it to Handler (spec §4.5 steps 2-3).
func (c *Client) sync(ctx context.Context) error {
	envs, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	if len(envs) == 0 {
		return nil
	}
	if err := c.Handler(ctx, envs); err != nil {
		return &handlerError{err}
	}
	return nil
}

func (c *Client) snapshot(ctx context.Context) ([]wire.MessageEnvelope, error) {
	resp, err := c.httpDo(ctx, http.MethodGet, c.snapshotURL())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("notify: snapshot: status %d", resp.StatusCode)
	}
	var envs []wire.MessageEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&envs); err != nil {
		return nil, fmt.Errorf("notify: snapshot: decode: %w", err)
	}
	return envs, nil
}

func (c *Client) httpDo(ctx context.Context, method, target string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

// reconnectDelay returns the jittered reconnect delay for attempt (spec
// §4.6): Backoff curve (default 1s, 2s, 4s, ... cap 60s) scaled by
// 0.75..1.25 via Rand.
func (c *Client) reconnectDelay(attempt int) time.Duration {
	base := DefaultBackoff(attempt)
	if c.Backoff != nil {
		base = c.Backoff(attempt)
	}
	rand := c.Rand
	if rand == nil {
		rand = randFloat64
	}
	factor := 0.75 + 0.5*rand() // uniform in [0.75, 1.25)
	if factor < 0.75 {
		factor = 0.75
	}
	if factor > 1.25 {
		factor = 1.25
	}
	return time.Duration(float64(base) * factor)
}

func (c *Client) streamURL() string {
	return strings.TrimSuffix(c.BaseURL, "/") + "/v1/inboxes/" + url.PathEscape(c.InboxID) + "/stream"
}

func (c *Client) snapshotURL() string {
	return strings.TrimSuffix(c.BaseURL, "/") + "/v1/inboxes/" + url.PathEscape(c.InboxID) +
		"/messages?limit=" + strconv.Itoa(c.snapshotLimit())
}

func (c *Client) snapshotLimit() int {
	if c.SnapshotLimit <= 0 {
		return DefaultSnapshotLimit
	}
	return c.SnapshotLimit
}

// randFloat64 is the default jitter source (math/rand/v2 global source,
// auto-seeded).
func randFloat64() float64 { return rand.Float64() }

// sseParser accumulates SSE field state and reports when a "message" event
// dispatches (on the blank line terminating the event). Heartbeat comment
// lines (":hb") are ignored. Events with no "event:" field are "message"
// per the SSE spec, so an unnamed data event also fires.
type sseParser struct {
	event   string
	hasData bool
}

// line processes one SSE line (newline stripped) and reports whether a
// "message" event just dispatched.
func (p *sseParser) line(s string) bool {
	s = strings.TrimSuffix(s, "\r")
	if s == "" { // blank line: dispatch
		event := p.event
		if event == "" {
			event = "message" // SSE default event type
		}
		fired := event == "message" && p.hasData
		p.event = ""
		p.hasData = false
		return fired
	}
	if s[0] == ':' {
		return false // comment (heartbeat)
	}
	field, value := s, ""
	if i := strings.IndexByte(s, ':'); i >= 0 {
		field, value = s[:i], s[i+1:]
		value = strings.TrimPrefix(value, " ")
	}
	switch field {
	case "event":
		p.event = value
	case "data":
		p.hasData = true
	}
	return false
}
