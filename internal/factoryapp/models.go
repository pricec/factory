package factoryapp

import (
	"context"
	"net/http"
	"time"

	"github.com/pricec/factory/internal/models"
)

// modelsCacheTTL bounds how long one `opencode models` listing is reused
// (spec §6.9): the exec is not free, and the UI refetches on every page
// load. New models (e.g. a fresh local ollama pull) show up after the TTL
// rolls over.
const modelsCacheTTL = 5 * time.Minute

// handleModels serves GET /v1/models (spec §6.9): every model id the
// factory host's opencode install reports, provider-split and sorted -
// the UI renders it as the create-job form's model picker (openrouter,
// ollama, ... - whatever the opencode install has configured).
// Unauthenticated like the rest of the control plane (spec §7.5); an empty
// listing is [], never null. Errors surface the normative error model.
func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	list, err := a.listModels(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if list == nil {
		list = []models.ModelInfo{}
	}
	writeJSON(w, http.StatusOK, list)
}

// listModels returns the cached listing when fresh, else execs
// `opencode models` (single-flight: the mutex spans the exec, so
// concurrent page loads share one run). Only non-empty listings are
// cached: an empty result is the degraded mode `opencode models`
// falls into when its background service is unreachable (exit 0, no
// output) - caching it would pin a broken picker for a whole TTL, so
// the next request retries instead.
func (a *App) listModels(ctx context.Context) ([]models.ModelInfo, error) {
	a.modelsMu.Lock()
	defer a.modelsMu.Unlock()
	bin := a.modelsBin
	if bin == "" {
		bin = "opencode"
	}
	if a.modelsCache != nil && a.now().Sub(a.modelsFetchedAt) < modelsCacheTTL {
		return a.modelsCache, nil
	}
	list, err := models.List(ctx, bin)
	if err != nil {
		return nil, err
	}
	if len(list) > 0 {
		a.modelsCache, a.modelsFetchedAt = list, a.now()
	}
	return list, nil
}
