package factoryapp

import (
	_ "embed"
	"log"
	"net/http"
	"strconv"

	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/wire"
)

// uiHTML is the single-page operator UI (spec §2: the factory serves the web
// UI; §9.1: the UI is a plain-HTTP, non-MCP surface). It is one static page:
// all data is fetched from the JSON endpoints below and the existing
// unauthenticated control-plane endpoints (§6.9: GET /v1/foremen,
// GET /v1/jobs).
//
//go:embed ui.html
var uiHTML []byte

// handleUI serves the operator UI shell at GET /: one static page.
func (a *App) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(uiHTML); err != nil {
		log.Printf("factoryapp: ui: %v", err)
	}
}

// handleInboxSummaries serves GET /v1/inboxes (spec §4.1: UI listing): one
// aggregate row per inbox that has ever received a message. Unauthenticated
// like the rest of the §6.9 control plane. Empty store -> [].
func (a *App) handleInboxSummaries(w http.ResponseWriter, r *http.Request) {
	sums, err := a.inbox.Summaries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if sums == nil {
		sums = []inbox.InboxSummary{}
	}
	writeJSON(w, http.StatusOK, sums)
}

// handleInboxRecent serves GET /v1/inboxes/{id}/recent?limit= (spec §4.1: UI
// replay): the newest limit envelopes for one inbox, acked included, in
// chronological order. Unauthenticated like the listing; the acked-including
// history is what distinguishes it from the long-poll GET .../messages.
func (a *App) handleInboxRecent(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be an integer in [1, 500]")
			return
		}
		limit = n
	}
	envs, err := a.inbox.Recent(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if envs == nil {
		envs = []wire.MessageEnvelope{}
	}
	writeJSON(w, http.StatusOK, envs)
}
