package inbox

import "net/http"

// Authenticator decides whether a request may perform an action on an inbox.
// The real implementation lands with #11 (pairing + scoped bearer tokens);
// until then PermissiveAuth allows everything.
type Authenticator interface {
	Authorize(r *http.Request, inboxID, action string) error
}

// PermissiveAuth allows every action (v1 default until #11 lands).
type PermissiveAuth struct{}

// Authorize implements Authenticator by always succeeding.
func (PermissiveAuth) Authorize(*http.Request, string, string) error { return nil }
