package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// Query-parameter bounds (spec §4.1).
const (
	defaultLimit   = 50
	maxLimit       = 500
	maxWaitSeconds = 60
)

// Handler mounts the inbox REST API under /v1 (spec §4.1).
type Handler struct {
	svc  *Service
	auth Authenticator
}

// NewHandler returns the inbox HTTP handler. A nil auth means PermissiveAuth.
func NewHandler(svc *Service, auth Authenticator) http.Handler {
	if auth == nil {
		auth = PermissiveAuth{}
	}
	h := &Handler{svc: svc, auth: auth}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/inboxes/{id}/messages", h.authz("post", h.postMessages))
	mux.HandleFunc("GET /v1/inboxes/{id}/messages", h.authz("read", h.getMessages))
	mux.HandleFunc("GET /v1/inboxes/{id}/messages/{mid}", h.authz("read", h.getMessage))
	mux.HandleFunc("POST /v1/inboxes/{id}/ack", h.authz("ack", h.ack))
	return mux
}

func (h *Handler) authz(action string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h.auth.Authorize(r, r.PathValue("id"), action); err != nil {
			writeError(w, http.StatusForbidden, "forbidden", err.Error(), false)
			return
		}
		next(w, r)
	}
}

func (h *Handler) postMessages(w http.ResponseWriter, r *http.Request) {
	inboxID := r.PathValue("id")
	var env wire.MessageEnvelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid envelope JSON: "+err.Error(), false)
		return
	}
	if err := env.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error(), false)
		return
	}
	full, err := h.svc.Post(r.Context(), inboxID, env)
	if err != nil {
		var dup *DuplicateError
		if errors.As(err, &dup) {
			writeError(w, http.StatusConflict, "duplicate", dup.Error(), false)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusCreated, full)
}

func (h *Handler) getMessages(w http.ResponseWriter, r *http.Request) {
	inboxID := r.PathValue("id")
	q := r.URL.Query()

	limit := defaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("limit must be an integer in [1, %d], got %q", maxLimit, raw), false)
			return
		}
		limit = n
	}

	wait := time.Duration(0)
	if raw := q.Get("wait_seconds"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > maxWaitSeconds {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("wait_seconds must be an integer in [0, %d], got %q", maxWaitSeconds, raw), false)
			return
		}
		wait = time.Duration(n) * time.Second
	}

	envs, err := h.svc.Get(r.Context(), inboxID, limit, wait)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return // client went away mid-long-poll
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, envs)
}

func (h *Handler) getMessage(w http.ResponseWriter, r *http.Request) {
	env, ok, err := h.svc.GetMessage(r.Context(), r.PathValue("mid"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such message", false)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (h *Handler) ack(w http.ResponseWriter, r *http.Request) {
	inboxID := r.PathValue("id")
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid ack JSON: "+err.Error(), false)
		return
	}
	if err := h.svc.Ack(r.Context(), inboxID, body.IDs); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("inbox: encode response: %v", err)
	}
}

// writeError emits the normative error model (spec §10).
func writeError(w http.ResponseWriter, status int, code, msg string, retryable bool) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   msg,
			"retryable": retryable,
		},
	})
}
