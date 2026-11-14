package factoryapp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// createJobRequest is the POST /v1/jobs body (spec §6.9): the same field set
// as the MCP create_job tool (§9.4) so the UI drives the identical path -
// spend breaker, capability match, create, dispatch, optional schedule.
type createJobRequest struct {
	Task         string          `json:"task"`
	Requires     []string        `json:"requires,omitempty"`
	ResultSchema json.RawMessage `json:"result_schema,omitempty"`
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
	writeJSON(w, http.StatusCreated, map[string]any{
		"job_id":  spec.ID,
		"state":   state,
		"foreman": foreman,
	})
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
// plane; the {id} path segment is the bare foreman id (pairing self.id).
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
	posted, err := a.inbox.Post(r.Context(), "foreman:"+id, env)
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
