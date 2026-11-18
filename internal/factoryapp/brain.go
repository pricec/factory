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
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// createJobRequest is the POST /v1/jobs body (spec §6.9): the same field set
// as the MCP create_job tool (§9.4) so the UI drives the identical path -
// capability match, create, dispatch, optional schedule.
type createJobRequest struct {
	Task     string   `json:"task"`
	Requires []string `json:"requires,omitempty"`
	// Secrets are credentials-store NAMES (spec §13.2), never values.
	Secrets []string `json:"secrets,omitempty"`
	// Touches is the declared file footprint (spec §6.10).
	Touches []string `json:"touches,omitempty"`
	// Foreman is the explicit routing pin (spec §6.7): bare or
	// foreman:-prefixed id of the foreman the job runs on; empty means
	// the Matcher's capability pick.
	Foreman      string          `json:"foreman,omitempty"`
	ResultSchema json.RawMessage `json:"result_schema,omitempty"`
	Tier         string          `json:"tier,omitempty"`
	// Model is the explicit per-job model (spec §5.1, 2026-11-15
	// amendment): it overrides the tier on the foreman.
	Model        string `json:"model,omitempty"`
	TimeoutS     int    `json:"timeout_s,omitempty"`
	Priority     int    `json:"priority,omitempty"`
	ScheduleCron string `json:"schedule_cron,omitempty"`
	// TaskID files the job under a task (spec §9.5.2): recorded in
	// task_jobs before dispatch, so the job's reports thread onto it.
	TaskID string `json:"task_id,omitempty"`
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
	if in.TaskID != "" {
		if _, err := a.tasks.GetTask(r.Context(), in.TaskID); err != nil {
			if errors.Is(err, tasks.ErrNotFound) {
				writeError(w, http.StatusNotFound, "unknown_task", "unknown task "+in.TaskID)
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}

	spec := wire.NewJobSpec(in.Task, "ui")
	spec.Requires = in.Requires
	spec.Secrets = in.Secrets
	spec.Touches = in.Touches
	spec.ResultSchema = in.ResultSchema
	spec.Tier = in.Tier
	spec.Model = in.Model
	if in.TimeoutS != 0 {
		spec.TimeoutS = in.TimeoutS
	}
	if in.Priority != 0 {
		spec.Priority = in.Priority
	}
	// The explicit routing pin (spec §6.7, 2026-11-16 foreman-pin
	// amendment): normalized to the full foreman:{id} form, it
	// replaces the Matcher's pick - the operator chose the foreman,
	// so no capability fallback applies.
	if in.Foreman != "" {
		spec.Foreman = registry.InboxID(in.Foreman)
	}
	if err := spec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Fail at create, never at dispatch (spec §13.2).
	if err := a.secrets.CheckNames(spec.Secrets); err != nil {
		writeError(w, http.StatusBadRequest, "unknown_secrets", err.Error())
		return
	}

	// Dispatch decision (spec §6.7): the pin wins over the Matcher. A
	// pinned job also skips creation-time requires validation -
	// requires stay informational on its spec, and their tag-name
	// rules would 400 a job whose routing never consults them.
	state, foreman := "pending", ""
	var warnings []string
	if spec.Foreman != "" {
		pin, found, err := jobs.FindForeman(r.Context(), a.jobs, spec.Foreman)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !found {
			// Same contract as message_foreman (§6.9): a pin naming
			// no registered foreman fails loudly - it never opens an
			// inbox nothing consumes and never parks pending.
			writeError(w, http.StatusNotFound, "unknown_foreman",
				fmt.Sprintf("foreman %q is not registered (GET /v1/foremen lists registered foremen)", in.Foreman))
			return
		}
		if pin.Online {
			foreman, state = pin.ID, "dispatched"
		} else {
			// Loud, non-fatal: the pin is honored, the job just
			// waits - the pending picker dispatches it when the
			// foreman returns (§6.7).
			warnings = append(warnings, "pinned foreman "+spec.Foreman+" is offline; the job pends until it returns")
		}
	} else {
		// Creation-time requires validation (spec §6.7, 2026-11-15 amendment):
		// entries must be tag names - bare or name:value - never capability
		// values; an entry naming no registered tag key can never match, and
		// the job would park pending silently.
		var err error
		warnings, err = a.jobs.ValidateRequires(r.Context(), spec.Requires)
		if err != nil {
			writeError(w, http.StatusBadRequest, "unknown_requires", err.Error())
			return
		}

		// Ad hoc dispatch via the Matcher (spec §6.7): no matching foreman
		// leaves the job pending.
		if f, ok, err := a.matcher.Match(r.Context(), spec.Requires); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		} else if ok {
			foreman, state = f.ID, "dispatched"
		}
	}
	// Dispatch gate (S3 token throttle, spec §6.10): same as create_job.
	hold, heldReason := a.jobs.Admit(r.Context(), spec, in.TaskID)
	if hold {
		state, foreman = jobs.StateHeld, ""
		if err := a.jobs.CreateHeld(r.Context(), spec, heldReason); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	} else if err := a.jobs.Create(r.Context(), spec); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Link before dispatch (spec §9.5.2): the job's first report must
	// already resolve to its task.
	if in.TaskID != "" {
		if err := a.tasks.LinkJob(r.Context(), in.TaskID, spec.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "link task: "+err.Error())
			return
		}
		defer a.publishTaskChanged(r.Context(), in.TaskID)
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
	if in.TaskID != "" {
		body["task_id"] = in.TaskID
	}
	if hold {
		body["held_reason"] = heldReason
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
