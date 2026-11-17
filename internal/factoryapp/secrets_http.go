package factoryapp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/pricec/factory/internal/secrets"
)

// maxSecretBody bounds the POST body: a 64KB value plus JSON escaping
// headroom (a worst-case value of control chars escapes to 6 bytes each).
const maxSecretBody = 6*secrets.MaxValueBytes + 4<<10

// handleListSecrets serves GET /v1/secrets (spec §13): metadata only.
func (a *App) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"secrets": a.secrets.List()})
}

// handlePutSecret serves POST /v1/secrets/{name} (spec §13): upsert. The
// response is metadata only; the value is never echoed or logged.
func (a *App) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Value  string `json:"value"`
		EnvVar string `json:"env_var"`
	}
	// Decode errors can quote body fragments; report a fixed message.
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSecretBody)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body (want {\"value\": string, \"env_var\"?: string})")
		return
	}
	meta, err := a.secrets.Put(r.PathValue("name"), in.Value, in.EnvVar)
	if err != nil {
		if errors.Is(err, secrets.ErrInvalid) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// handleDeleteSecret serves DELETE /v1/secrets/{name} (spec §13).
func (a *App) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := a.secrets.Delete(name); err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown_secret", "no secret named "+name)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "deleted": "true"})
}
