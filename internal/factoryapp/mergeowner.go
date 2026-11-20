// Job-owned merges (2026-11-17 amendment, spec §6.11 "Job-owned merges").
// A code-change job files its own merge request (worker request_merge,
// §9.2) and the factory tracks the outcome against that job's task, so no
// planner session has to remember a follow-up:
//
//   - landed: a notify.merge on the task wakes the planner once, only to
//     tell the operator;
//   - needs_attention with a failure the branch author can fix (red
//     checks, conflict, head drift): the factory dispatches a merge-fix
//     job on the same branch (jobs.MergeFixJobSpec), linked to the task,
//     which fixes, pushes and files the next request - at most
//     jobs.MaxMergeFixRounds times per chain;
//   - anything else (deploy/preflight failure, rounds exhausted, a fix job
//     that could not be created): the P1 alert as before, linked to the
//     task, plus a waking notify.merge so the planner coordinates;
//   - a failed or cancelled request that queued requests wait on (after):
//     their tasks get a waking notify.merge, because sequencing branches
//     of different jobs is the planner's coordination job.
//
// Requests without a source job (operator, planner, auditor) keep the
// previous behavior exactly.
package factoryapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/pricec/factory/internal/auditor"
	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// notifyMergeType is the merge-outcome envelope on a task thread (spec
// §9.5): internal (never shown by default), waking the planner when its
// payload says so.
const notifyMergeType = "notify.merge"

// notify.merge actions.
const (
	mergeActionLanded           = "landed"
	mergeActionFixDispatched    = "fix_dispatched"
	mergeActionEscalated        = "escalated"
	mergeActionCancelled        = "cancelled"
	mergeActionDependencyFailed = "dependency_failed"
	mergeActionDependencyStall  = "dependency_stalled"
)

// mergeBlockedAlertAfter is how long a request may wait on an after
// branch that nobody has queued before the operator and planner hear
// about it. A var so tests can shorten it.
var mergeBlockedAlertAfter = time.Hour

// mergeNote is the notify.merge payload.
type mergeNote struct {
	Wake           bool     `json:"wake"`
	Action         string   `json:"action"`
	MergeRequestID string   `json:"merge_request_id"`
	Repo           string   `json:"repo"`
	Branch         string   `json:"branch"`
	Head           string   `json:"head"`
	State          string   `json:"state"`
	Failure        string   `json:"failure,omitempty"`
	Result         string   `json:"result,omitempty"`
	SourceJobID    string   `json:"source_job_id,omitempty"`
	FixRound       int      `json:"fix_round,omitempty"`
	FixJobID       string   `json:"fix_job_id,omitempty"`
	WaitingOn      []string `json:"waiting_on,omitempty"`
	// Text says, for the planner, what happened and what (if anything)
	// is left for it to do.
	Text string `json:"text"`
}

// wakingMerge reports whether a notify.merge envelope asks for a planner
// session (landed, escalated, dependency trouble); fix_dispatched and
// cancelled are a record only.
func wakingMerge(env wire.MessageEnvelope) bool {
	var p struct {
		Wake bool `json:"wake"`
	}
	return json.Unmarshal(env.Payload, &p) == nil && p.Wake
}

// mergeActionActionable reports whether a note asks the planner to do
// something (as opposed to tell the operator or record): only these
// reopen a closed task.
func mergeActionActionable(action string) bool {
	switch action {
	case mergeActionEscalated, mergeActionDependencyFailed, mergeActionDependencyStall:
		return true
	}
	return false
}

// mergeNoteDuplicate reports whether the thread already carries a
// notify.merge for the same request, action, state, failure and text: the
// same event again (a re-run reconcile, a repeated escalation) must not
// wake the planner twice.
func (a *App) mergeNoteDuplicate(ctx context.Context, taskID string, n mergeNote) bool {
	rows, err := a.inbox.DB().QueryContext(ctx,
		`SELECT payload FROM messages WHERE thread_id = ? AND type = ?`, taskID, notifyMergeType)
	if err != nil {
		log.Printf("factoryapp: merge queue: dedupe lookup on %s: %v", taskID, err)
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var prev mergeNote
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &prev) != nil {
			continue
		}
		if prev.MergeRequestID == n.MergeRequestID && prev.Action == n.Action && prev.State == n.State &&
			prev.Failure == n.Failure && prev.FixRound == n.FixRound && prev.Text == n.Text {
			return true
		}
	}
	return false
}

// notifyMerge routes a merge event for mr to the task that owns its
// source job (never a new task):
//
//   - no-op events are dropped: the request is already landed/cancelled
//     and the event is neither, or the same event was already posted;
//   - a closed owner task is reopened (with a note in the text) when the
//     event needs the planner to act, and only recorded otherwise;
//   - when no task owns the job, the standing Planner chat gets the note
//     if it exists, else (for events that wake) an alert is raised.
func (a *App) notifyMerge(ctx context.Context, mr jobs.MergeRequest, action, text string, wake bool, fixJobID string) {
	if cur, ok, err := a.jobs.GetMergeRequest(ctx, mr.ID); err == nil && ok &&
		(cur.State == jobs.MergeLanded || cur.State == jobs.MergeCancelled) &&
		action != mergeActionLanded && action != mergeActionCancelled {
		return // finished already: nothing left to act on
	}
	taskID := a.jobThread(ctx, mr.SourceJobID)
	fallback := false
	if taskID != "" {
		if _, err := a.tasks.GetTask(ctx, taskID); err != nil {
			taskID = ""
		}
	}
	if taskID == "" {
		if mr.SourceJobID == "" || a.knownTask(ctx, tasks.StandingID) == "" {
			if wake && mr.SourceJobID != "" && action != mergeActionEscalated && action != mergeActionDependencyStall {
				a.raiseSystemAlert(ctx, alertPriorityMerge, "Merge "+mr.ID+" ("+mr.Branch+") has no owning task", text, mergeAlertSource)
			}
			return // an escalation or stall has its own alert
		}
		taskID, fallback = tasks.StandingID, true
		text = "No task owns the job behind this merge request (" + orDefault(mr.SourceJobID, "no source job") + "). " + text
	}
	t, err := a.tasks.GetTask(ctx, taskID)
	if err != nil {
		return
	}
	if t.Status == tasks.StatusClosed && !fallback {
		if wake && mergeActionActionable(action) {
			if _, err := a.reopenTask(ctx, taskID); err != nil {
				log.Printf("factoryapp: merge queue: reopen %s for %s: %v", taskID, mr.ID, err)
				wake = false
			} else {
				text = "Task reopened by the factory because this merge needs the planner (the task was closed; no new task is filed for merges). " + text
			}
		} else {
			wake = false // record only: the operator closed the task
		}
	}
	note := mergeNote{
		Wake: wake, Action: action, MergeRequestID: mr.ID, Repo: mr.Repo, Branch: mr.Branch, Head: mr.Head,
		State: mr.State, Failure: mr.Failure, Result: mr.Result, SourceJobID: mr.SourceJobID,
		FixRound: mr.FixRound, FixJobID: fixJobID, WaitingOn: mr.WaitingOn, Text: text,
	}
	if a.mergeNoteDuplicate(ctx, taskID, note) {
		return
	}
	data, err := json.Marshal(note)
	if err != nil {
		log.Printf("factoryapp: merge queue: note for %s: %v", mr.ID, err)
		return
	}
	if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, wire.MessageEnvelope{
		Type: notifyMergeType, From: "factory", Payload: data, JobID: mr.SourceJobID, ThreadID: taskID,
	}); err != nil {
		log.Printf("factoryapp: merge queue: note for %s: %v", mr.ID, err)
		return
	}
	a.publishTaskChanged(ctx, taskID)
}

// routeMergeOutcome is called once for every request that reached landed,
// needs_attention or cancelled through the queue (finishMerge,
// mergeNeedsAttention). See the file comment for the routing.
func (a *App) routeMergeOutcome(ctx context.Context, mr jobs.MergeRequest) {
	// Auditor auto-fixes keep their own path (§6.12): alert + proposal.
	if a.audit != nil && auditor.IsAuditMerge(mr.RequestedBy, mr.Branch) {
		if mr.State == jobs.MergeNeedsAttention {
			a.alertMerge(ctx, mr, "")
		}
		a.auditMergeOutcome(ctx, mr)
		return
	}
	switch mr.State {
	case jobs.MergeLanded:
		if mr.SourceJobID != "" {
			a.notifyMerge(ctx, mr, mergeActionLanded,
				fmt.Sprintf("Merge request %s landed: branch %s is merged and deployed (%s). Tell the operator; nothing else is needed for this merge.",
					mr.ID, mr.Branch, orDefault(mr.Result, "merged")), true, "")
		}
	case jobs.MergeCancelled:
		if mr.SourceJobID != "" {
			a.notifyMerge(ctx, mr, mergeActionCancelled,
				fmt.Sprintf("Merge request %s for %s was cancelled: %s", mr.ID, mr.Branch, orDefault(mr.Result, "no reason recorded")), false, "")
		}
		a.notifyDependents(ctx, mr)
	case jobs.MergeNeedsAttention:
		if mr.SourceJobID == "" {
			a.alertMerge(ctx, mr, "")
			a.notifyDependents(ctx, mr)
			return
		}
		fixJob, why := a.dispatchMergeFix(ctx, mr)
		if fixJob != "" {
			log.Printf("factoryapp: merge queue: %s (%s) failed (%s); fix job %s dispatched (round %d)",
				mr.ID, mr.Branch, mr.Failure, fixJob, mr.FixRound+1)
			a.notifyMerge(ctx, mr, mergeActionFixDispatched,
				fmt.Sprintf("Merge request %s for %s failed (%s). Fix job %s was dispatched automatically on the same branch (round %d of %d); it re-queues the merge itself. Nothing to do.",
					mr.ID, mr.Branch, mr.Failure, fixJob, mr.FixRound+1, jobs.MaxMergeFixRounds), false, fixJob)
			return
		}
		a.alertMerge(ctx, mr, why)
		a.notifyMerge(ctx, mr, mergeActionEscalated,
			fmt.Sprintf("Merge request %s for %s needs attention and was NOT fixed automatically: %s. Failure: %s. Result: %s. "+
				"Decide the next step (a job to fix it, sequencing with another branch, or the operator's decision) and tell the operator.",
				mr.ID, mr.Branch, why, orDefault(mr.Failure, jobs.MergeFailureOther), mr.Result), true, "")
		a.notifyDependents(ctx, mr)
	}
}

// dispatchMergeFix creates the merge-fix job for the failed job-owned
// request mr. It returns the fix job id, or "" and the reason no fix job
// was dispatched (which is why the failure escalates).
func (a *App) dispatchMergeFix(ctx context.Context, mr jobs.MergeRequest) (fixJobID, why string) {
	switch {
	case !jobs.MergeFailureFixable(mr.Failure):
		return "", "a " + orDefault(mr.Failure, jobs.MergeFailureOther) + " failure is not something the branch's job can fix"
	case mr.FixRound >= jobs.MaxMergeFixRounds:
		return "", fmt.Sprintf("%d automatic fix rounds are used up", jobs.MaxMergeFixRounds)
	}
	source, ok, err := a.jobs.Get(ctx, mr.SourceJobID)
	if err != nil || !ok {
		return "", fmt.Sprintf("source job %s is unknown to the factory (%v)", mr.SourceJobID, err)
	}
	origin := source
	if p, isFix := jobs.MergeFixPayloadOf(source.Spec); isFix && p.OriginJobID != "" {
		if o, ok, err := a.jobs.Get(ctx, p.OriginJobID); err == nil && ok {
			origin = o
		}
	}
	taskID := a.jobThread(ctx, source.ID)
	if taskID != "" {
		if t, err := a.tasks.GetTask(ctx, taskID); err == nil && t.Status == tasks.StatusClosed {
			return "", "its task " + taskID + " is closed"
		}
	}
	spec := jobs.MergeFixJobSpec(mr, mr.Failure, source, origin, wire.NewJobID(), mr.FixRound+1)
	if err := a.createFactoryJob(ctx, spec, taskID); err != nil {
		return "", "the fix job could not be created: " + err.Error()
	}
	return spec.ID, ""
}

// createFactoryJob stores a factory-built job through the usual gate
// (spec §6.7, §6.10), links it to taskID before dispatch (§9.5.2), and
// dispatches it: to its pin when pinned and online, else by capability
// match; with no foreman available it stays pending for the pending
// picker (and the long-pending alert).
func (a *App) createFactoryJob(ctx context.Context, spec wire.JobSpec, taskID string) error {
	hold, reason := a.jobs.Admit(ctx, spec, taskID)
	if hold {
		if err := a.jobs.CreateHeld(ctx, spec, reason); err != nil {
			return err
		}
	} else if err := a.jobs.Create(ctx, spec); err != nil {
		return err
	}
	if taskID != "" {
		if err := a.tasks.LinkJob(ctx, taskID, spec.ID); err != nil {
			log.Printf("factoryapp: link job %s to task %s: %v", spec.ID, taskID, err)
		}
		a.publishTaskChanged(ctx, taskID)
	}
	if hold {
		return nil
	}
	foreman := ""
	if spec.Foreman != "" {
		pin, found, err := jobs.FindForeman(ctx, a.jobs, spec.Foreman)
		switch {
		case err != nil:
			log.Printf("factoryapp: job %s left pending: pin lookup: %v", spec.ID, err)
		case !found || !pin.Online:
			log.Printf("factoryapp: job %s left pending: pinned foreman %s is not online", spec.ID, spec.Foreman)
		default:
			foreman = pin.ID
		}
	} else if f, ok, err := a.matcher.Match(ctx, spec.Requires); err != nil {
		log.Printf("factoryapp: job %s left pending: match: %v", spec.ID, err)
	} else if ok {
		foreman = f.ID
	}
	if foreman == "" {
		return nil
	}
	return a.jobs.Dispatch(ctx, spec.ID, foreman)
}

// notifyDependents tells the tasks of queued requests that wait on
// failed.Branch (their after list) that the dependency did not land. The
// dependents stay queued: sequencing work across jobs is the planner's
// coordination job, so this is a waking note.
func (a *App) notifyDependents(ctx context.Context, failed jobs.MergeRequest) {
	queued, err := a.jobs.QueuedMerges(ctx)
	if err != nil {
		log.Printf("factoryapp: merge queue: dependents of %s: %v", failed.ID, err)
		return
	}
	for _, dep := range queued {
		if dep.Repo != failed.Repo || dep.SourceJobID == "" || !containsStr(dep.WaitingOn, failed.Branch) {
			continue
		}
		a.notifyMerge(ctx, dep, mergeActionDependencyFailed,
			fmt.Sprintf("Merge request %s (%s) is queued to merge after branch %s, but that branch's merge request %s is %s: %s. "+
				"%s stays queued until a request for %s lands. Coordinate: get %s fixed and re-queued, or cancel %s (POST /v1/merge-requests/%s/cancel) and have it re-requested without the dependency.",
				dep.ID, dep.Branch, failed.Branch, failed.ID, failed.State, orDefault(failed.Result, "no result"),
				dep.ID, failed.Branch, failed.Branch, dep.ID, dep.ID), true, "")
	}
}

// alertStalledDependencies raises one P1 alert (and a waking note) per
// queued request that has waited mergeBlockedAlertAfter on an after
// branch nobody has queued: without it such a request would sit silently.
// Deduped per process (at-least-once across restarts, like the
// long-pending alert).
func (a *App) alertStalledDependencies(ctx context.Context) {
	queued, err := a.jobs.QueuedMerges(ctx)
	if err != nil {
		log.Printf("factoryapp: merge queue: stalled dependencies: %v", err)
		return
	}
	now := a.tasks.Now()
	for _, mr := range queued {
		if len(mr.WaitingOn) == 0 || a.mergeStallAlerted[mr.ID] {
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, mr.CreatedAt)
		if err != nil || now.Sub(created) < mergeBlockedAlertAfter {
			continue
		}
		var idle []string
		for _, branch := range mr.WaitingOn {
			if _, active, err := a.jobs.ActiveMergeForBranch(ctx, mr.Repo, branch); err == nil && !active {
				idle = append(idle, branch)
			}
		}
		if len(idle) == 0 {
			continue
		}
		a.mergeStallAlerted[mr.ID] = true
		title := "Merge " + mr.ID + " (" + mr.Branch + ") is waiting on an unqueued branch"
		if a.hasOpenAlertTitled(ctx, title) {
			continue // an identical alert is still open; don't re-raise
		}
		text := fmt.Sprintf("Merge request %s (%s) has waited %s to merge after %s, and no merge request for %s is queued or merging. "+
			"It will not dispatch until one lands. Queue the dependency, or cancel %s and re-request it without `after`.",
			mr.ID, mr.Branch, now.Sub(created).Round(time.Minute), strings.Join(idle, ", "), strings.Join(idle, ", "), mr.ID)
		a.raiseSystemAlertForTask(ctx, alertPriorityMerge, title,
			text, mergeAlertSource, a.knownTask(ctx, a.jobThread(ctx, mr.SourceJobID)))
		a.notifyMerge(ctx, mr, mergeActionDependencyStall, text, true, "")
	}
}

// jobMergeSummaries lists the merge requests jobID filed for its own
// work, for the notify.job payload: the planner sees at a glance that
// the merge is already queued and tracked.
func (a *App) jobMergeSummaries(ctx context.Context, jobID string) []map[string]any {
	reqs, err := a.jobs.MergeRequestsBySourceJob(ctx, jobID)
	if err != nil {
		log.Printf("factoryapp: merge requests of %s: %v", jobID, err)
		return nil
	}
	var out []map[string]any
	for _, mr := range reqs {
		m := map[string]any{"id": mr.ID, "branch": mr.Branch, "head": mr.Head, "state": mr.State}
		if len(mr.WaitingOn) > 0 {
			m["waiting_on"] = mr.WaitingOn
		}
		out = append(out, m)
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// hasOpenAlertTitled reports whether an open alert has exactly this title.
func (a *App) hasOpenAlertTitled(ctx context.Context, title string) bool {
	open, err := a.tasks.ListAlerts(ctx, tasks.AlertFilter{States: []string{tasks.AlertOpen}})
	if err != nil {
		return false
	}
	for _, al := range open {
		if al.Title == title {
			return true
		}
	}
	return false
}
