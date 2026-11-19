package factoryapp

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/pricec/factory/internal/buildinfo"
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

// uiManifest is the web app manifest (GET /manifest.webmanifest): it lets
// the operator add the UI to a phone home screen as a standalone app.
const uiManifest = `{
  "id": "/",
  "name": "Factory",
  "short_name": "Factory",
  "description": "Factory operator console",
  "start_url": "/#/inbox",
  "scope": "/",
  "display": "standalone",
  "background_color": "#0d1117",
  "theme_color": "#0d1117",
  "icons": [
    {"src": "/apple-touch-icon.png", "sizes": "180x180", "type": "image/png"},
    {"src": "/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any"},
    {"src": "/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any"},
    {"src": "/icon-maskable-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"}
  ]
}
`

//go:embed uiassets/apple-touch-icon.png
var uiAppleIcon []byte

//go:embed uiassets/icon-512.png
var uiIcon512 []byte

//go:embed uiassets/icon-192.png
var uiIcon192 []byte

//go:embed uiassets/icon-maskable-512.png
var uiIconMaskable []byte

//go:embed uiassets/sw.js
var uiServiceWorker string

// swVersion is the cache-busting version baked into /sw.js: the build id plus
// a hash of everything the worker caches, so any deploy (and any dev edit)
// changes the worker bytes and replaces the shell cache.
func swVersion() string {
	h := sha256.New()
	h.Write(uiHTML)
	h.Write([]byte(uiManifest))
	h.Write(uiAppleIcon)
	h.Write(uiIcon192)
	h.Write(uiIcon512)
	h.Write(uiIconMaskable)
	return buildinfo.Current() + "-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// handleServiceWorker serves GET /sw.js. It is never HTTP-cached so the
// browser's update check sees a new deploy immediately.
func (a *App) handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Service-Worker-Allowed", "/")
	if _, err := io.WriteString(w, strings.Replace(uiServiceWorker, "__VERSION__", swVersion(), 1)); err != nil {
		log.Printf("factoryapp: sw: %v", err)
	}
}

// handleUIAsset serves the static home-screen assets of the operator UI:
// the manifest and the two PNG icons. Unauthenticated like GET /.
func (a *App) handleUIAsset(w http.ResponseWriter, r *http.Request) {
	var ct string
	var body []byte
	switch r.URL.Path {
	case "/manifest.webmanifest":
		ct, body = "application/manifest+json", []byte(uiManifest)
	case "/apple-touch-icon.png":
		ct, body = "image/png", uiAppleIcon
	case "/icon-512.png":
		ct, body = "image/png", uiIcon512
	case "/icon-192.png":
		ct, body = "image/png", uiIcon192
	case "/icon-maskable-512.png":
		ct, body = "image/png", uiIconMaskable
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if _, err := w.Write(body); err != nil {
		log.Printf("factoryapp: ui asset: %v", err)
	}
}

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
