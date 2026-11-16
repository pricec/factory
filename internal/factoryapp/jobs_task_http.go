package factoryapp

import (
	"encoding/json"
	"log"
	"net/http"
)

// handleGetJob serves GET /v1/jobs/{id}: the jobs registry record plus the
// task it is linked to (spec §6.9 / §9.5.2, deploy-18 follow-up). task_id
// is the direct task_jobs link only - a read never writes lineage links -
// and is omitted when the job is not linked or no task store is wired.
// Not-found / error bodies keep the §10 error model of the jobs API.
func (a *App) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok, err := a.jobs.Get(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"code": "internal", "message": err.Error(), "retryable": true},
		})
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	raw, err := json.Marshal(job)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"code": "internal", "message": err.Error(), "retryable": true},
		})
		return
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		writeJSON(w, http.StatusOK, job)
		return
	}
	if a.tasks != nil {
		taskID, linked, err := a.tasks.TaskForJob(r.Context(), id)
		if err != nil {
			log.Printf("factoryapp: GET /v1/jobs/%s: task lookup: %v", id, err)
		} else if linked {
			out["task_id"], _ = json.Marshal(taskID)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
