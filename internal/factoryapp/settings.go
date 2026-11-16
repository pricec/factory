package factoryapp

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/wire"
)

// maxSettingsBody caps the POST /v1/settings body: five short model
// strings, nothing more.
const maxSettingsBody = 8 << 10

// settingRow is one role in the GET /v1/settings response (spec §6.9,
// 2026-11-15 amendment): model is the value stored via the panel (when
// source is "panel") or the pinned default otherwise. A per-host env
// override still applies at spawn time when no panel value is set - the
// factory cannot see foreman-side env, so the panel never claims it does
// (spec §6.5).
type settingRow struct {
	Role         string `json:"role"`
	Model        string `json:"model"`
	DefaultModel string `json:"default_model"`
	Source       string `json:"source"`
}

// settingsResponse is the record-wrapped GET /v1/settings body.
type settingsResponse struct {
	Roles []settingRow `json:"roles"`
}

// handleGetSettings serves GET /v1/settings (spec §6.9): every managed
// role with its panel-set model (source "panel") or the pinned default
// (source "default"), in pinned display order. Unauthenticated like the
// rest of the control plane (spec §7.5).
func (a *App) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	rows, err := a.settingsState(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse{Roles: rows})
}

// handlePostSettings serves POST /v1/settings (spec §6.9): the settings
// panel's save path. Body is a role -> model map; a present key sets the
// stored value, an empty string clears it (back to env/default), an
// absent key is left untouched. Unknown roles and invalid model strings
// are 400s (the mis-typed-call lesson of the 2026-11-14 amendment).
// Response: the updated state, same shape as GET.
func (a *App) handlePostSettings(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSettingsBody)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	for role, model := range in {
		if _, ok := models.RoleDefault(role); !ok {
			writeError(w, http.StatusBadRequest, "bad_request",
				"role: unknown "+role+" (managed roles: planner, foreman_brain, worker_flash, worker_default, worker_hardest)")
			return
		}
		if err := wire.ValidateModelID(model); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", role+": "+err.Error())
			return
		}
	}
	for role, model := range in {
		var err error
		if model == "" {
			err = a.jobs.ClearSetting(r.Context(), role)
		} else {
			err = a.jobs.SetSetting(r.Context(), role, model)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	rows, err := a.settingsState(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse{Roles: rows})
}

// settingsState builds the response rows in pinned role order.
func (a *App) settingsState(ctx context.Context) ([]settingRow, error) {
	stored, err := a.jobs.AllSettings(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]settingRow, 0, len(models.Roles))
	for _, role := range models.Roles {
		row := settingRow{Role: role.Name, DefaultModel: role.Default, Source: "default", Model: role.Default}
		if m, ok := stored[role.Name]; ok {
			row.Model, row.Source = m, "panel"
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// resolvePlannerModel resolves the factory planner's model (spec §6.5):
// stored panel setting > FACTORY_PLANNER_MODEL env > pinned default.
// The factory runs the planner itself, so the env leg is authoritative
// here (foreman-side roles resolve their env on the foreman). A read
// error falls back to env/default - the planner must never fail to
// wake over a settings lookup.
func (a *App) resolvePlannerModel() string {
	ctx := context.Background()
	if m, ok, err := a.jobs.Setting(ctx, "planner"); err != nil {
		log.Printf("factoryapp: read planner setting: %v (using env/default)", err)
	} else if ok {
		return m
	}
	return models.Resolve(os.Getenv("FACTORY_PLANNER_MODEL"), models.PlannerDefault)
}

// PlannerModel returns the planner's current effective model (test
// accessor; the planner brain reads it per session via ModelFn).
func (a *App) PlannerModel() string { return a.resolvePlannerModel() }
