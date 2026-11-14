package inbox

import "net/http"

// Authenticator decides whether a request may perform an action on an inbox.
// The real implementation lives in internal/auth (#11: pairing + scoped
// bearer tokens); PermissiveAuth allows everything.
type Authenticator interface {
	Authorize(r *http.Request, inboxID, action string) error
}

// StatusError may be implemented by Authenticator errors to select the
// exact HTTP status (spec §7.3: 401 unknown/invalid token, 403 wrong
// scope). Plain errors map to 403.
type StatusError interface {
	error
	HTTPStatus() int
}

// PermissiveAuth allows every action (v1 default until #11 lands).
type PermissiveAuth struct{}

// Authorize implements Authenticator by always succeeding.
func (PermissiveAuth) Authorize(*http.Request, string, string) error { return nil }
