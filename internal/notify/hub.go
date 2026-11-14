package notify

import (
	"sync"
	"time"

	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/wire"
)

// DefaultHeartbeat is the SSE heartbeat interval (spec §4.4, §11: a ":hb"
// comment every 15s).
const DefaultHeartbeat = 15 * time.Second

// subscriberBuffer bounds each stream's event backlog. Events are wake-ups,
// not the delivery path (spec §4.4), so a slow consumer does not stall
// Publish: when the buffer is full the event is dropped - the consumer
// already has subscriberBuffer pending wake-ups, each of which triggers a
// full snapshot resync that observes every posted message.
const subscriberBuffer = 16

// subscriber is one live SSE stream registered for an inbox.
type subscriber struct {
	inbox string
	ch    chan wire.MessageEnvelope
}

// Hub fans out posted envelopes to every live SSE stream subscribed to the
// target inbox (spec §4.4). The zero Hub is usable; NewHub sets the default
// heartbeat.
type Hub struct {
	// Heartbeat is the interval between ":hb" comment lines written to
	// each stream. Configure before StreamHandler serves traffic; zero or
	// negative falls back to DefaultHeartbeat.
	Heartbeat time.Duration

	mu   sync.Mutex
	subs map[string]map[*subscriber]struct{}
	obs  Observer
}

// NewHub returns a Hub with the default heartbeat.
func NewHub() *Hub {
	return &Hub{Heartbeat: DefaultHeartbeat}
}

// SetObserver sets the liveness observer (spec §4.7). OnConnect fires when a
// stream subscribes, OnDisconnect when it drops; OnActivity is the auth
// layer's concern (later ticket). Observer callbacks run in their own
// goroutines and must never block the hub. A nil obs disables emission.
func (h *Hub) SetObserver(obs Observer) {
	h.mu.Lock()
	h.obs = obs
	h.mu.Unlock()
}

func (h *Hub) observer() Observer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.obs
}

// AttachTo wires svc's publish hook to Publish: every successful Post fans
// out to the hub's subscribers. The hook contract (inbox.SetPublishHook)
// requires a non-blocking, concurrency-safe callback; Publish satisfies it
// with a buffered, drop-on-full send.
func (h *Hub) AttachTo(svc *inbox.Service) {
	svc.SetPublishHook(h.Publish)
}

// Publish fans env out to every stream subscribed to inboxID. It never
// blocks: a full subscriber buffer drops the event (see subscriberBuffer).
func (h *Hub) Publish(inboxID string, env wire.MessageEnvelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs[inboxID] {
		select {
		case sub.ch <- env:
		default: // buffer full: a pending wake-up already guarantees resync
		}
	}
}

// Subscribers reports how many live streams are subscribed to inboxID.
func (h *Hub) Subscribers(inboxID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[inboxID])
}

// subscribe registers a stream for inboxID and fires OnConnect.
func (h *Hub) subscribe(inboxID string) *subscriber {
	sub := &subscriber{inbox: inboxID, ch: make(chan wire.MessageEnvelope, subscriberBuffer)}
	h.mu.Lock()
	if h.subs == nil {
		h.subs = make(map[string]map[*subscriber]struct{})
	}
	if h.subs[inboxID] == nil {
		h.subs[inboxID] = make(map[*subscriber]struct{})
	}
	h.subs[inboxID][sub] = struct{}{}
	h.mu.Unlock()
	if obs := h.observer(); obs != nil {
		go obs.OnConnect(inboxID)
	}
	return sub
}

// unsubscribe removes sub and fires OnDisconnect. Idempotent.
func (h *Hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	if m := h.subs[sub.inbox]; m != nil {
		delete(m, sub)
		if len(m) == 0 {
			delete(h.subs, sub.inbox)
		}
	}
	h.mu.Unlock()
	if obs := h.observer(); obs != nil {
		go obs.OnDisconnect(sub.inbox)
	}
}

// closeInbox forcibly ends every live stream for inboxID by closing its
// channel; the stream handlers return and their clients reconnect. It
// returns the number of streams closed. Test seam and future admin lever.
func (h *Hub) closeInbox(inboxID string) int {
	h.mu.Lock()
	n := 0
	for sub := range h.subs[inboxID] {
		close(sub.ch)
		n++
	}
	delete(h.subs, inboxID)
	h.mu.Unlock()
	return n
}
