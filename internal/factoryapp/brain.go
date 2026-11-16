package factoryapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/wire"
)

// createJobRequest is the POST /v1/jobs body (spec §6.9): the same field set
// as the MCP create_job tool (§9.4) so the UI drives the identical path -
// spend breaker, capability match, create, dispatch, optional schedule.
type createJobRequest struct {
	Task         string          `json:"task"`
	Requires     []string        `json:"requires,omitempty"`
	ResultSchema json.RawMessage `json:"result_schema,omitempty"`
	Tier         string          `json:"tier,omitempty"`
	Model        string          `json:"model,omitempty"`
	TimeoutS     int             `json:"timeout_s,omitempty"`
	Priority     int             `json:"priority,omitempty"`
	ScheduleCron string          `json:"schedule_cron,omitempty"`
}

// handleCreateJob serves POST /v1/jobs (spec §6.9): the UI-facing twin of the
// MCP create_job tool (§9.4) - same breaker, same matcher, same stores.
// Unauthenticated like the rest of the control plane. Response: 201 + the
// created job (202-style pending/dispatched state is in the body).
func (a *App) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var in createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}

	// Breaker first (spec §6.5), same as the MCP tool.
	if err := a.jobs.RefuseIfSpendCap(r.Context(), a.budget.DailySpendCapUSD, a.budget.SpendOverride); err != nil {
		var cap *jobs.CapError
		if errors.As(err, &cap) {
			writeError(w, http.StatusPaymentRequired, "spend_cap_exceeded", cap.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	spec := wire.NewJobSpec(in.Task, "ui")
	spec.Requires = in.Requires
	spec.ResultSchema = in.ResultSchema
	spec.Tier = in.Tier
	spec.Model = in.Model
	if in.TimeoutS != 0 {
		spec.TimeoutS = in.TimeoutS
	}
	if in.Priority != 0 {
		spec.Priority = in.Priority
	}
	if err := spec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Creation-time requires validation (spec §6.7, 2026-11-15 amendment):
	// entries must be tag names - bare or name:value - never capability
	// values; an entry naming no registered tag key can never match, and
	// the job would park pending silently.
	warnings, err := a.jobs.ValidateRequires(r.Context(), spec.Requires)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown_requires", err.Error())
		return
	}

	// Ad hoc dispatch via the Matcher (spec §6.7): no matching foreman
	// leaves the job pending.
	state, foreman := "pending", ""
	if f, ok, err := a.matcher.Match(r.Context(), spec.Requires); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	} else if ok {
		foreman, state = f.ID, "dispatched"
	}
	if err := a.jobs.Create(r.Context(), spec); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if state == "dispatched" {
		if err := a.jobs.Dispatch(r.Context(), spec.ID, foreman); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}

	// With schedule_cron ALSO create a schedule with the same template
	// (spec §9.4, §6.8), in addition to the one-off job above.
	if in.ScheduleCron != "" {
		template := spec
		template.ID, template.CreatedAt = "", ""
		if _, err := a.jobs.CreateSchedule(r.Context(), in.ScheduleCron, template); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "create schedule: "+err.Error())
			return
		}
	}

	w.Header().Set("Location", "/v1/jobs/"+spec.ID)
	body := map[string]any{
		"job_id":  spec.ID,
		"state":   state,
		"foreman": foreman,
	}
	// Loud, non-fatal warnings (spec §6.7): e.g. requires could not be
	// validated because no foremen are registered yet.
	if len(warnings) > 0 {
		body["warnings"] = warnings
	}
	writeJSON(w, http.StatusCreated, body)
}

// messageForemanRequest is the POST /v1/foremen/{id}/messages body (spec
// §6.9): the UI-facing twin of the MCP message_foreman tool (§9.4).
type messageForemanRequest struct {
	Type    string          `json:"type"`
	JobID   string          `json:"job_id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// handleMessageForeman serves POST /v1/foremen/{id}/messages (spec §6.9):
// posts an envelope from the planner identity into the foreman's inbox,
// same as the MCP message_foreman tool. Unauthenticated like the control
// plane; the {id} path segment is the bare foreman id (pairing self.id)
// or the full "foreman:{id}" form - both normalize to the same inbox
// (spec §7.3). Never opens a dead inbox: an id matching no registered
// foreman is a 404, not a post to an inbox nothing reads, and a missing
// body is a 400 (2026-11-14: double-prefixed ids and null payloads
// dead-ended two operator feature requests).
func (a *App) handleMessageForeman(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "foreman id: required")
		return
	}
	var in messageForemanRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	rows, err := a.jobs.Foremen(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	known := false
	for _, f := range rows {
		if registry.SameForeman(f.ID, id) {
			known = true
			break
		}
	}
	if !known {
		writeError(w, http.StatusNotFound, "unknown_foreman",
			fmt.Sprintf("foreman %q is not registered (GET /v1/foremen lists registered foremen)", id))
		return
	}
	if len(in.Payload) == 0 || bytes.Equal(in.Payload, []byte("null")) {
		writeError(w, http.StatusBadRequest, "bad_request", "payload: required (send {} for an empty body)")
		return
	}
	env := wire.MessageEnvelope{
		Type:    in.Type,
		JobID:   in.JobID,
		From:    auth.FactoryPlannerIdentity,
		Payload: in.Payload,
	}
	if err := env.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	posted, err := a.inbox.Post(r.Context(), registry.InboxID(id), env)
	if err != nil {
		var dup *inbox.DuplicateError
		if errors.As(err, &dup) {
			writeError(w, http.StatusConflict, "duplicate", dup.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, posted)
}
