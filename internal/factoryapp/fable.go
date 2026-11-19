package factoryapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// Fable gate (spec §6.10): a job whose explicit model is models.Fable is
// created held ("fable: awaiting operator approval") and raises an alert
// (and needs_operator on its task). POST /v1/jobs/{id}/approve records the
// approval; the releaser then still runs the usage gate, so approval never
// bypasses the throttle (only the P0 valve does). Rejecting = cancel_job.

// DefaultFableMaxConcurrent caps simultaneously active approved Fable jobs.
const DefaultFableMaxConcurrent = 1

const alertPriorityFable = 2

// LoadFableMaxConcurrent reads FACTORY_FABLE_MAX_CONCURRENT (values < 1 or
// unset mean the default, 1).
func LoadFableMaxConcurrent() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("FACTORY_FABLE_MAX_CONCURRENT")))
	if err != nil || n < 1 {
		return DefaultFableMaxConcurrent
	}
	return n
}

// fableNotify tells the operator a Fable job awaits approval.
func (a *App) fableNotify(ctx context.Context, spec wire.JobSpec, taskID string) {
	if a.tasks == nil {
		return
	}
	if taskID != "" {
		flag := true
		if _, err := a.tasks.UpdateTask(ctx, taskID, tasks.TaskUpdate{NeedsOperator: &flag}); err != nil {
			log.Printf("factoryapp: fable: flag task %s: %v", taskID, err)
		}
	}
	a.raiseSystemAlertForTask(ctx, alertPriorityFable, "Fable job awaiting approval: "+spec.ID,
		"Job "+spec.ID+" asks for model "+models.Fable+" and is held until you approve it (Usage panel, held jobs, Approve) or cancel it.",
		"factory:fable", taskID)
}

// fableAdmit is the admission stage for NEW jobs: unapproved Fable holds.
func (a *App) fableAdmit(ctx context.Context, spec wire.JobSpec, taskID string) (bool, string) {
	if !models.IsFable(spec.Model) {
		return false, ""
	}
	a.fableNotify(ctx, spec, taskID)
	return true, jobs.FableAwaitingReason
}

// fableRelease is the releaser stage for held Fable jobs.
func (a *App) fableRelease(ctx context.Context, j jobs.Job) (bool, string) {
	if !models.IsFable(j.Spec.Model) {
		return false, ""
	}
	ok, err := a.jobs.FableApproved(ctx, j.ID)
	if err != nil || !ok {
		return true, jobs.FableAwaitingReason
	}
	max := a.fableMax
	if max < 1 {
		max = DefaultFableMaxConcurrent
	}
	n := 0
	ids, err := a.jobs.ActiveJobIDs(ctx)
	if err != nil {
		return true, jobs.FableHoldPrefix + "checking concurrent Fable jobs failed; retrying"
	}
	for _, id := range ids {
		if aj, found, err := a.jobs.Get(ctx, id); err == nil && found && models.IsFable(aj.Spec.Model) {
			n++
		}
	}
	if n >= max {
		return true, fmt.Sprintf("%sapproved; waiting for a free slot (max %d concurrent Fable jobs)", jobs.FableHoldPrefix, max)
	}
	return false, ""
}

// handleApproveJob serves POST /v1/jobs/{id}/approve: the operator's single
// approval action for a held Fable job.
func (a *App) handleApproveJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok, err := a.jobs.Get(r.Context(), id)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	case !ok:
		writeError(w, http.StatusNotFound, "not_found", "no such job")
		return
	case !models.IsFable(job.Spec.Model):
		writeError(w, http.StatusConflict, "not_gated", "job does not use a model that needs approval")
		return
	}
	applied, err := a.jobs.ApproveFable(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !applied {
		writeError(w, http.StatusConflict, "not_held", "job is not held (state "+job.State+")")
		return
	}
	log.Printf("factoryapp: fable: job %s approved by operator", id)
	if a.tasks != nil {
		if taskID, linked, err := a.tasks.TaskForJob(r.Context(), id); err == nil && linked {
			if err := a.tasks.ClearNeedsOperator(r.Context(), taskID); err != nil && !errors.Is(err, tasks.ErrNotFound) {
				log.Printf("factoryapp: fable: clear needs_operator on %s: %v", taskID, err)
			}
			a.publishTaskChanged(r.Context(), taskID)
		}
	}
	a.releaseNow(r.Context()) // usage gate still applies
	cur, _, _ := a.jobs.Get(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": id, "state": cur.State, "held_reason": cur.HeldReason})
}
