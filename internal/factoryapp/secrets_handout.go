package factoryapp

import (
	"errors"
	"net/http"
	"strings"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
)

// handoutSecret is one entry of the handout response (spec §13.2). This
// is the ONLY response in the system that carries secret values.
type handoutSecret struct {
	Name   string `json:"name"`
	EnvVar string `json:"env_var"`
	Value  string `json:"value"`
}

// handleJobSecrets serves GET /v1/jobs/{id}/secrets (spec §13.2): the
// job-scoped handout. The caller must hold the paired foreman credential,
// the job's foreman_id must be that foreman, the job must be live, and only
// names in the job spec's secrets list are ever returned. Every grant is
// audited (secret_grants) BEFORE values leave the process.
func (a *App) handleJobSecrets(w http.ResponseWriter, r *http.Request) {
	identity, jobScope, err := a.auth.Identify(r.Context(), bearerOf(r.Header.Get("Authorization")))
	var ue *auth.UnauthorizedError
	if errors.As(err, &ue) {
		writeError(w, http.StatusUnauthorized, "unauthorized", ue.Reason)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "identify credential")
		return
	}
	if !strings.HasPrefix(identity, "foreman:") || jobScope != "" {
		writeError(w, http.StatusForbidden, "forbidden", "only a paired foreman credential may fetch job secrets")
		return
	}
	id := r.PathValue("id")
	job, ok, err := a.jobs.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "load job")
		return
	}
	// Unknown job and foreign job look identical: no existence oracle.
	if !ok || job.ForemanID != identity {
		writeError(w, http.StatusForbidden, "forbidden", "job is not assigned to this foreman")
		return
	}
	if job.State != jobs.StateDispatched && job.State != jobs.StateRunning {
		writeError(w, http.StatusConflict, "job_not_live", "job is "+job.State+"; secrets are handed out only to dispatched or running jobs")
		return
	}
	out := make([]handoutSecret, 0, len(job.Spec.Secrets))
	var missing []string
	for _, name := range job.Spec.Secrets {
		val, meta, err := a.secrets.Get(name)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		out = append(out, handoutSecret{Name: name, EnvVar: meta.EnvVar, Value: val})
	}
	if len(missing) > 0 {
		writeError(w, http.StatusNotFound, "unknown_secrets", "secrets no longer in the store: "+strings.Join(missing, ", "))
		return
	}
	if err := a.jobs.RecordSecretGrants(r.Context(), id, job.Spec.Secrets); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "audit grant failed; no secrets handed out")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
}
