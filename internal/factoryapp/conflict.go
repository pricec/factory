// Conflict coordinator (spec §6.10, 2026-11-18 amendment): the second
// admission stage, after the usage gate. Jobs declare the files they touch
// (JobSpec.touches); a job whose footprint overlaps an older, still-active
// job's is parked in the existing `held` state with a
// "conflict: waits for job_X (glob)" held_reason, and ReleasePass releases
// it once the blocker is finished AND its merge has landed. The pure
// decision is jobs.CheckConflict; this file wires it: exemptions, the
// max-wait bound, persistence of the wait start, and task events.
package factoryapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// DefaultConflictMaxWait bounds one conflict hold: after this long the job
// is released anyway (a blocker whose merge never lands must not wedge the
// queue forever). Override with FACTORY_CONFLICT_MAX_WAIT.
const DefaultConflictMaxWait = 60 * time.Minute

// notifyConflictType is the conflict-hold envelope on a task thread:
// internal, never waking the planner (it is a record, like a
// fix_dispatched merge note).
const notifyConflictType = "notify.conflict"

// notify.conflict actions.
const (
	conflictWaiting  = "waiting"
	conflictReleased = "released"
	conflictTimedOut = "timed_out"
)

// conflictExempt reports whether a job is never conflict-checked: merge
// jobs and merge-fix jobs (they exist to land work, so holding them would
// deadlock the very merge a blocker waits for).
func conflictExempt(spec wire.JobSpec) bool {
	return spec.CreatedBy == jobs.MergeJobCreatedBy || spec.CreatedBy == jobs.MergeFixCreatedBy
}

// conflictAdmit is the second admission stage. existing is true for a held
// job being re-evaluated by ReleasePass, false for a NEW job (not stored
// yet). createdAt orders the job against the active ones (never waiting on
// a newer job). It fails open on any error.
func (a *App) conflictAdmit(ctx context.Context, spec wire.JobSpec, createdAt, taskID string, p0, existing bool) (bool, string) {
	if a.conflictOff || len(spec.Touches) == 0 || p0 || conflictExempt(spec) {
		return false, ""
	}
	if createdAt == "" {
		createdAt = a.jobs.Now().UTC().Format(time.RFC3339)
	}
	active, err := a.jobs.ActiveFootprints(ctx)
	if err != nil {
		log.Printf("factoryapp: conflict: active footprints: %v (admitting)", err)
		return false, ""
	}
	hard, soft := jobs.CheckConflict(jobs.Candidate{
		JobID: spec.ID, Touches: spec.Touches, CreatedAt: createdAt, Priority: spec.Priority,
	}, active)
	for _, c := range soft {
		if !existing {
			log.Printf("factoryapp: conflict: job %s overlaps %s on docs only (%s vs %s): not holding", spec.ID, c.JobID, c.OwnGlob, c.Glob)
		}
	}
	if hard == nil {
		if existing {
			_ = a.jobs.ClearConflictSince(ctx, spec.ID)
		}
		return false, ""
	}
	reason := jobs.ConflictReason(hard.JobID, hard.Glob)
	if !existing {
		a.gate.Unadmit(spec.ID) // it is not starting: free its ORANGE slot
		a.noteConflict(ctx, taskID, spec.ID, conflictWaiting, hard.JobID, hard.Glob,
			fmt.Sprintf("job %s is waiting on %s (%s overlaps %s) until its merge lands", spec.ID, hard.JobID, hard.OwnGlob, hard.Glob))
		return true, reason
	}
	now := a.jobs.Now().UTC()
	since, err := a.jobs.ConflictSince(ctx, spec.ID)
	if err != nil {
		log.Printf("factoryapp: conflict: since %s: %v (admitting)", spec.ID, err)
		return false, ""
	}
	t, perr := time.Parse(time.RFC3339, since)
	if since == "" || perr != nil {
		if err := a.jobs.MarkConflictSince(ctx, spec.ID, now.Format(time.RFC3339)); err != nil {
			log.Printf("factoryapp: conflict: mark %s: %v", spec.ID, err)
		}
		t = now
	}
	if now.Sub(t) >= a.conflictMaxWait {
		text := fmt.Sprintf("job %s waited %s on %s (%s) and is released anyway (max wait %s): expect a possible merge conflict",
			spec.ID, now.Sub(t).Truncate(time.Second), hard.JobID, hard.Glob, a.conflictMaxWait)
		log.Printf("factoryapp: conflict: %s", text)
		a.noteConflict(ctx, taskID, spec.ID, conflictTimedOut, hard.JobID, hard.Glob, text)
		return false, ""
	}
	return true, reason
}

// noteConflict records a conflict event on the job's task thread (when it
// has one) and in the log: "waiting on job_X" and the matching release.
func (a *App) noteConflict(ctx context.Context, taskID, jobID, action, blocker, glob, text string) {
	log.Printf("factoryapp: conflict: %s: %s", action, text)
	if taskID == "" || a.inbox == nil {
		return
	}
	data, err := json.Marshal(map[string]any{
		"wake": false, "action": action, "job_id": jobID,
		"waits_for": blocker, "glob": glob, "text": text,
	})
	if err != nil {
		return
	}
	if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, wire.MessageEnvelope{
		Type: notifyConflictType, From: "factory", Payload: data, JobID: jobID, ThreadID: taskID,
	}); err != nil {
		log.Printf("factoryapp: conflict: note for %s: %v", jobID, err)
		return
	}
	a.publishTaskChanged(ctx, taskID)
}
