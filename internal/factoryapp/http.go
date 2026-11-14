package factoryapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/registry"
)

// maxPairBody caps the pair request; pairing payloads are tiny.
const maxPairBody = 16 << 10

// pairRequest is the POST /v1/pair body (spec §7.1).
type pairRequest struct {
	PairingToken string `json:"pairing_token"`
	Self         struct {
		ID           string            `json:"id"`
		Capabilities map[string]string `json:"capabilities"`
	} `json:"self"`
}

// handlePair serves POST /v1/pair (spec §7.1): burns the pairing token,
// mints the foreman credential, and upserts the foreman's capabilities
// (spec §6.7 pairing path, #9/#12 wiring).
func (a *App) handlePair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxPairBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid pair request: "+err.Error())
		return
	}
	if req.Self.ID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "self.id: required")
		return
	}
	bearer, inboxID, err := a.auth.PairForeman(r.Context(), req.PairingToken, req.Self.ID)
	if err != nil {
		var ue *auth.UnauthorizedError
		if errors.As(err, &ue) {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := a.upsertCaps(r.Context(), inboxID, req.Self.Capabilities); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"bearer":     bearer,
		"inbox_id":   inboxID,
		"parent_url": a.advertise,
	})
}

// refreshAuthz resolves the capabilities-refresh caller: parse the Bearer,
// Identify it, map *auth.UnauthorizedError to 401 and everything else to
// 403, and set the identity (a foreman inbox id) via the pointer-out
// param.
func (a *App) refreshAuthz() registry.AuthzFunc {
	return func(r *http.Request, identity *string) error {
		bearer := bearerOf(r.Header.Get("Authorization"))
		id, _, err := a.auth.Identify(r.Context(), bearer)
		if err != nil {
			var ue *auth.UnauthorizedError
			if errors.As(err, &ue) {
				return err // registry maps status carriers: 401
			}
			return &auth.ForbiddenError{Reason: err.Error()} // -> 403
		}
		if !strings.HasPrefix(id, "foreman:") {
			return &auth.ForbiddenError{Reason: "credential " + id + " may not refresh foreman capabilities"}
		}
		*identity = id
		return nil
	}
}

// upsertCaps upserts identity's capability tags (spec §6.7 refresh path).
func (a *App) upsertCaps(ctx context.Context, identity string, caps map[string]string) error {
	if caps == nil {
		caps = map[string]string{}
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return err
	}
	return a.jobs.UpsertForeman(ctx, identity, capsJSON)
}

func bearerOf(h string) string {
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("factoryapp: encode response: %v", err)
	}
}

// writeError emits the normative error model (spec §10).
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   msg,
			"retryable": false,
		},
	})
}
