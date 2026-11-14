package auth

import (
	"net/http"
	"sync"

	"github.com/pricec/factory/internal/inbox"
)

// Authenticator implements inbox.Authenticator against the credential Store
// (spec §7.3): 401 for unknown/invalid/revoked bearers, 403 for scope
// misses; ack requires may_read.
type Authenticator struct {
	store *Store

	mu       sync.RWMutex
	activity func(identity string)
}

// NewAuthenticator wraps store for inbox request authorization.
func NewAuthenticator(store *Store) *Authenticator { return &Authenticator{store: store} }

// SetActivityHook installs fn to be called with the credential's identity
// on every successful Authorize (spec §6.3: #9 liveness wiring). The hook
// runs on the request goroutine and must not block.
func (a *Authenticator) SetActivityHook(fn func(identity string)) {
	a.mu.Lock()
	a.activity = fn
	a.mu.Unlock()
}

// Authorize implements inbox.Authenticator.
func (a *Authenticator) Authorize(r *http.Request, inboxID, action string) error {
	bearer := trimBearer(r.Header.Get("Authorization"))
	cred, err := a.store.lookup(r.Context(), bearer)
	if err != nil {
		return err
	}
	ok, err := a.store.canDo(r.Context(), cred.credID, inboxID, action)
	if err != nil {
		return err
	}
	if !ok {
		return &ForbiddenError{Reason: "credential " + cred.identity + " may not " + action + " on " + inboxID}
	}
	a.mu.RLock()
	hook := a.activity
	a.mu.RUnlock()
	if hook != nil {
		hook(cred.identity)
	}
	return nil
}

var _ inbox.Authenticator = (*Authenticator)(nil)

// PairHandler exposes POST /v1/pair (spec §7.1). parentURL is echoed back to
// the pairing child. Note for integrators: the request's self.capabilities
// are not persisted here; the factory wiring upserts them via #9/#12.
func PairHandler(store *Store, parentURL string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pair", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PairingToken string `json:"pairing_token"`
			Self         struct {
				ID           string            `json:"id"`
				Capabilities map[string]string `json:"capabilities"`
			} `json:"self"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		if req.Self.ID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "self.id: required", false)
			return
		}
		bearer, inboxID, err := store.PairForeman(r.Context(), req.PairingToken, req.Self.ID)
		if err != nil {
			if isUnauthorized(err) {
				writeError(w, http.StatusUnauthorized, "unauthorized", err.Error(), false)
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"bearer":     bearer,
			"inbox_id":   inboxID,
			"parent_url": parentURL,
		})
	})
	return mux
}
