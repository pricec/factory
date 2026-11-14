package factoryapp

import (
	_ "embed"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pricec/factory/internal/auth"
)

// installScript is the onboarding script template (spec §7.1, #24). It has
// exactly two server-side placeholders - {{FACTORY_URL}} and
// {{PAIRING_TOKEN}} - replaced per-request; the foreman id is substituted
// client-side (positional arg or hostname).
//
//go:embed install.sh
var installScript string

// downloadPattern is the pinned set of servable foreman binaries (spec
// §7.1): linux amd64 + arm64 only. The anchored regexp doubles as the
// path-safety check - no user-controlled subdirectory ever reaches the
// filesystem path.
var downloadPattern = regexp.MustCompile(`^foreman-(linux)-(amd64|arm64)$`)

// renderInstall substitutes the factory URL and a fresh pairing token into
// the onboarding script.
func renderInstall(url, token string) []byte {
	s := strings.ReplaceAll(installScript, "{{FACTORY_URL}}", url)
	s = strings.ReplaceAll(s, "{{PAIRING_TOKEN}}", token)
	return []byte(s)
}

// handleInstall serves GET /install.sh (#24): planner-credential auth
// exactly like POST /v1/pairing-tokens - 401 unknown/revoked bearer, 403
// non-planner identity - then embeds a fresh one-time pairing token and the
// advertise URL so onboarding needs no manual token copy-paste. The pairing
// token rides the rendered script: anyone holding the planner token (the v1
// operator credential) can onboard machines (spec §7.1).
func (a *App) handleInstall(w http.ResponseWriter, r *http.Request) {
	identity, _, err := a.auth.Identify(r.Context(), bearerOf(r.Header.Get("Authorization")))
	var ue *auth.UnauthorizedError
	if errors.As(err, &ue) {
		writeError(w, http.StatusUnauthorized, "unauthorized", ue.Reason)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if identity != auth.FactoryPlannerIdentity {
		writeError(w, http.StatusForbidden, "forbidden", "credential "+identity+" may not fetch the install script")
		return
	}
	token, _, err := a.auth.IssuePairingToken(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	body := renderInstall(a.advertise, token)
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Header().Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		log.Printf("factoryapp: write install script: %v", err)
	}
}

// handleDownload serves GET /download/foreman/{platform} (#24) from the
// downloads dir. Unauthenticated (trusted-LAN posture, spec §7.5). 400 for
// a platform outside the pinned regexp (which also blocks traversal); 404
// when the binary file is absent.
func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	if !downloadPattern.MatchString(platform) {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown platform "+platform)
		return
	}
	data, err := os.ReadFile(filepath.Join(a.downloadsDir, platform))
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not_found", "no foreman binary for "+platform)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+platform+`"`)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		log.Printf("factoryapp: write download %s: %v", platform, err)
	}
}
