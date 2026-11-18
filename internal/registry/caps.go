package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Capabilities is a foreman's flat tag map (spec §6.7): tag name -> value.
type Capabilities map[string]string

// Has reports whether a requires entry (spec §6.7) is satisfied. A bare
// entry is a tag name: present with a non-empty value. A "name:value"
// entry additionally requires the tag's value - or its comma-separated
// value list ("tools" -> "git,docker") - to contain value, so "os:linux",
// "arch:amd64", "tools:git" work against the standard os/arch/tools tags.
// ':' separates name from value (tag names must not contain it); an empty
// value part degrades to the bare-name form. A missing or empty-valued
// tag counts as absent. Safe on a nil Capabilities.
func (c Capabilities) Has(entry string) bool {
	name, value, constrained := strings.Cut(entry, ":")
	if !constrained || value == "" {
		return c.has(name)
	}
	if name == "" {
		return false
	}
	v := c[name]
	if v == "" {
		return false
	}
	for _, part := range strings.Split(v, ",") {
		if strings.TrimSpace(part) == value {
			return true
		}
	}
	return false
}

// has reports whether tag is present with a non-empty value. An empty
// value counts as absent. Safe on a nil Capabilities.
func (c Capabilities) has(tag string) bool {
	v, ok := c[tag]
	return ok && v != ""
}

// HasAll reports whether every requires entry is satisfied (AND
// semantics; entries are bare tag names or "name:value", see Has). Empty
// requires matches any foreman, including one with no capabilities at
// all.
func (c Capabilities) HasAll(requires []string) bool {
	for _, entry := range requires {
		if !c.Has(entry) {
			return false
		}
	}
	return true
}

// Tags returns the sorted tag names, for tests and logging.
func (c Capabilities) Tags() []string {
	tags := make([]string, 0, len(c))
	for k := range c {
		tags = append(tags, k)
	}
	sort.Strings(tags)
	return tags
}

// refreshBody is the POST /v1/foreman/capabilities payload (spec §6.7).
type refreshBody struct {
	Capabilities map[string]string `json:"capabilities"`
	// ActiveJobs is the ids the foreman has live workers for (spec §6.2
	// reconciliation). nil (absent) means an older foreman that does not
	// report: nothing is reconciled.
	ActiveJobs *[]string `json:"active_jobs,omitempty"`
	// ClaudeAuth is the foreman's Claude CLI auth health report (spec
	// §13.3); absent from older foremen.
	ClaudeAuth json.RawMessage `json:"claude_auth,omitempty"`
}

// ExtensionFunc sees the refresh's optional `claude_auth` report (nil
// when absent) after the upsert and returns extra top-level fields for
// the response body (nil or empty = none). Spec §13.3.
type ExtensionFunc func(ctx context.Context, identity string, claudeAuth json.RawMessage) map[string]any

// ReconcileFunc receives the foreman's claimed in-flight jobs on a refresh.
type ReconcileFunc func(ctx context.Context, identity string, active []string) error

// maxRefreshBody caps the refresh payload; capabilities are small tags.
const maxRefreshBody = 64 << 10

// AuthzFunc authenticates the request and resolves the caller's identity
// (a foreman inbox id like "foreman:alpha", spec §3.2/§7.3) into *identity.
// Integration wires auth-style error types: an error implementing
// HTTPStatus() int keeps its status (401/403); any other error maps to 401.
type AuthzFunc func(r *http.Request, identity *string) error

// RefreshHandler serves POST /v1/foreman/capabilities (spec §6.7 refresh):
// the foreman upserts its capability tags, and the upsert bumps last_seen.
// The registry package imports neither auth nor jobs; integration wires
// the authz and upsert closures.
func RefreshHandler(authz AuthzFunc, upsert func(ctx context.Context, identity string, caps map[string]string) error) http.Handler {
	return RefreshHandlerWithAdvert(authz, upsert, nil)
}

// RefreshHandlerWithAdvert is RefreshHandler plus the foreman_update
// advert (spec §6.7): when advert returns non-nil the refresh answers 200
// {"foreman_update":...}; nil advert or nil result keeps the 204.
func RefreshHandlerWithAdvert(authz AuthzFunc, upsert func(ctx context.Context, identity string, caps map[string]string) error, advert AdvertFunc) http.Handler {
	return RefreshHandlerReconciling(authz, upsert, advert, nil)
}

// RefreshHandlerReconciling is RefreshHandlerWithAdvert plus job
// reconciliation: when the body carries active_jobs, reconcile runs after
// the upsert (spec §6.2).
func RefreshHandlerReconciling(authz AuthzFunc, upsert func(ctx context.Context, identity string, caps map[string]string) error, advert AdvertFunc, reconcile ReconcileFunc) http.Handler {
	return RefreshHandlerExtended(authz, upsert, advert, reconcile, nil)
}

// RefreshHandlerExtended is RefreshHandlerReconciling plus the extension
// hook (spec §13.3): ext's fields join the response body, which turns a
// 204 into a 200 when the advert is empty but ext has something to say.
func RefreshHandlerExtended(authz AuthzFunc, upsert func(ctx context.Context, identity string, caps map[string]string) error, advert AdvertFunc, reconcile ReconcileFunc, ext ExtensionFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required", false)
			return
		}
		var identity string
		if err := authz(r, &identity); err != nil {
			writeAuthzError(w, err)
			return
		}
		if identity == "" {
			writeError(w, http.StatusInternalServerError, "internal", "authz resolved an empty identity", true)
			return
		}
		var body refreshBody
		if err := json.NewDecoder(io.LimitReader(r.Body, maxRefreshBody)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error(), false)
			return
		}
		if err := upsert(r.Context(), identity, body.Capabilities); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
			return
		}
		if reconcile != nil && body.ActiveJobs != nil {
			if err := reconcile(r.Context(), identity, *body.ActiveJobs); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
				return
			}
		}
		resp := map[string]any{}
		if ext != nil {
			for k, v := range ext(r.Context(), identity, body.ClaudeAuth) {
				resp[k] = v
			}
		}
		if advert != nil {
			if fu := advert(); fu != nil {
				resp["foreman_update"] = fu
			}
		}
		if len(resp) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("registry: encode refresh response: %v", err)
		}
	})
}

// statusCarrier is satisfied by auth error types that carry their HTTP
// status (e.g. auth.UnauthorizedError -> 401, auth.ForbiddenError -> 403).
type statusCarrier interface{ HTTPStatus() int }

func writeAuthzError(w http.ResponseWriter, err error) {
	status := http.StatusUnauthorized
	var sc statusCarrier
	if errors.As(err, &sc) && sc.HTTPStatus() > 0 {
		status = sc.HTTPStatus()
	}
	code := "unauthorized"
	if status == http.StatusForbidden {
		code = "forbidden"
	}
	writeError(w, status, code, err.Error(), false)
}

// writeError emits the normative error model (spec §10).
func writeError(w http.ResponseWriter, status int, code, msg string, retryable bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   msg,
			"retryable": retryable,
		},
	}); err != nil {
		log.Printf("registry: encode response: %v", err)
	}
}

// detectTargets are the tools probed at foreman startup (spec §6.7).
var detectTargets = []string{"docker", "git", "opencode"}

// DetectTools probes dirs (PATH entries) for the v1 tool set (opencode,
// docker, git) and returns the foreman-startup capabilities entry
// {"tools": "git,docker"}: found tools comma-joined in sorted order.
// A tool is found when a regular executable file named exactly like the
// tool exists in one of the dirs. None found yields an empty "tools" value.
func DetectTools(dirs []string) map[string]string {
	var found []string
	for _, name := range detectTargets {
		if toolInDirs(dirs, name) {
			found = append(found, name)
		}
	}
	return map[string]string{"tools": strings.Join(found, ",")}
}

func toolInDirs(dirs []string, name string) bool {
	for _, dir := range dirs {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return true
	}
	return false
}
