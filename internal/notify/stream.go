package notify

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/pricec/factory/internal/inbox"
)

// StreamHandler returns a handler serving GET /v1/inboxes/{id}/stream (spec
// §4.4): an SSE stream of "message" events (data = full envelope JSON) fired
// on every Post to that inbox, plus a ":hb" comment every hub.Heartbeat.
// Auth is the same seam as the inbox REST API: auth.Authorize(r, inboxID,
// "read"); an error answers 403 with the normative error model (spec §10).
// A nil auth means inbox.PermissiveAuth.
func StreamHandler(hub *Hub, auth inbox.Authenticator) http.Handler {
	if auth == nil {
		auth = inbox.PermissiveAuth{}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /v1/inboxes/{id}/stream", &streamHandler{hub: hub, auth: auth})
	return mux
}

type streamHandler struct {
	hub  *Hub
	auth inbox.Authenticator
}

func (h *streamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	inboxID := r.PathValue("id")
	if err := h.auth.Authorize(r, inboxID, "read"); err != nil {
		writeStreamError(w, http.StatusForbidden, "forbidden", err.Error(), false)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeStreamError(w, http.StatusInternalServerError, "internal", "streaming unsupported", true)
		return
	}

	// Subscribe before the response headers are written: once the client
	// sees the response, the subscription is already live, so a Post
	// landing between stream-open and the client's snapshot GET cannot
	// slip through unsubscribed (the lost-wakeup guard of spec §4.5).
	sub := h.hub.subscribe(inboxID)
	defer h.hub.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	// Immediate heartbeat comment: flushes the headers and defeats proxy
	// buffering; the periodic tick below continues the cadence.
	if !writeSSE(w, ":hb\n") {
		return
	}
	flusher.Flush()

	heartbeat := h.hub.Heartbeat
	if heartbeat <= 0 {
		heartbeat = DefaultHeartbeat
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done(): // client disconnected
			return
		case env, ok := <-sub.ch:
			if !ok {
				return // hub closed the subscription
			}
			data, err := json.Marshal(env)
			if err != nil {
				// A MessageEnvelope always marshals; log and skip rather
				// than emit a malformed event.
				log.Printf("notify: marshal envelope: %v", err)
				continue
			}
			if !writeSSE(w, "event: message\ndata: "+string(data)+"\n\n") {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if !writeSSE(w, ":hb\n") {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSE writes s and reports whether the write succeeded. A failed write
// means the client is gone; the handler returns and request-context cleanup
// unsubscribes it.
func writeSSE(w http.ResponseWriter, s string) bool {
	if _, err := fmt.Fprint(w, s); err != nil {
		return false
	}
	return true
}

// writeStreamError emits the normative error model (spec §10).
func writeStreamError(w http.ResponseWriter, status int, code, msg string, retryable bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   msg,
			"retryable": retryable,
		},
	}); err != nil {
		log.Printf("notify: encode error response: %v", err)
	}
}
