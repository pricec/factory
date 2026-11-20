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

// installServerScript is the factory-server bootstrap script template (spec
// §7.1, #25). It has exactly one server-side placeholder -
// {{FACTORY_URL}} - replaced per-request with the serving factory's
// advertise URL; everything else (port, planner token) is client-side.
//
//go:embed install-server.sh
var installServerScript string

// downloadPattern is the pinned set of servable binaries (spec §7.1):
// factory, foreman and vision, linux amd64 + arm64 only. The anchored regexp
// doubles as the path-safety check - no user-controlled subdirectory ever
// reaches the filesystem path.
var downloadPattern = regexp.MustCompile(`^(factory|foreman|vision)-(linux)-(amd64|arm64)$`)

// renderInstall substitutes the factory URL and a fresh pairing token into
// the onboarding script.
func renderInstall(url, token string) []byte {
	s := strings.ReplaceAll(installScript, "{{FACTORY_URL}}", url)
	s = strings.ReplaceAll(s, "{{PAIRING_TOKEN}}", token)
	return []byte(s)
}

// plannerAuth enforces the planner-credential gate shared by the install
// script endpoints (#24, #25): 401 unknown/revoked bearer, 403 non-planner
// identity - the same trust class as POST /v1/pairing-tokens.
func (a *App) plannerAuth(w http.ResponseWriter, r *http.Request) bool {
	identity, _, err := a.auth.Identify(r.Context(), bearerOf(r.Header.Get("Authorization")))
	var ue *auth.UnauthorizedError
	if errors.As(err, &ue) {
		writeError(w, http.StatusUnauthorized, "unauthorized", ue.Reason)
		return false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return false
	}
	if identity != auth.FactoryPlannerIdentity {
		writeError(w, http.StatusForbidden, "forbidden", "credential "+identity+" may not fetch the install script")
		return false
	}
	return true
}

// renderInstallServer substitutes the serving factory's advertise URL into
// the bootstrap script (#25).
func renderInstallServer(url string) []byte {
	return []byte(strings.ReplaceAll(installServerScript, "{{FACTORY_URL}}", url))
}

// handleInstall serves GET /install.sh (#24): planner-credential auth
// exactly like POST /v1/pairing-tokens - 401 unknown/revoked bearer, 403
// non-planner identity - then embeds a fresh one-time pairing token and the
// advertise URL so onboarding needs no manual token copy-paste. The pairing
// token rides the rendered script: anyone holding the planner token (the v1
// operator credential) can onboard machines (spec §7.1).
func (a *App) handleInstall(w http.ResponseWriter, r *http.Request) {
	if !a.plannerAuth(w, r) {
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

// handleInstallServer serves GET /install-server.sh (#25): the
// curl|bash bootstrap for the factory server itself, hosted by a running
// factory. Planner-credential auth exactly like /install.sh; rendered
// per-request with the serving factory's advertise URL as the download
// source (spec §7.1 bootstrap rule: the first factory is manual; thereafter
// factories serve factories).
func (a *App) handleInstallServer(w http.ResponseWriter, r *http.Request) {
	if !a.plannerAuth(w, r) {
		return
	}
	body := renderInstallServer(a.advertise)
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Header().Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		log.Printf("factoryapp: write install-server script: %v", err)
	}
}

// handleDownload serves GET /download/{kind}/{platform} (#24, #25) - kind =
// factory | foreman - from the downloads dir. Unauthenticated (trusted-LAN
// posture, spec §7.5). 400 for a platform outside the pinned regexp (which
// also blocks traversal); 404 when the binary file is absent.
func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	if !downloadPattern.MatchString(platform) {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown platform "+platform)
		return
	}
	data, err := os.ReadFile(filepath.Join(a.downloadsDir, platform))
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not_found", "no binary for "+platform)
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
