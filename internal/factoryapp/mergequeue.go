// Merge queue serialization loop (2026-11-17 amendment, spec §6.11): one
// merge at a time, factory-tracked. Every pass (ticker, or a kick after an
// API mutation / merge-job terminal transition):
//
//  1. reconciles: a merging request whose merge job is already terminal
//     (a terminal hook missed across a restart) or missing is closed;
//  2. holds when the persisted pause flag is set;
//  3. holds while any request is merging or any merge job is non-terminal;
//  4. otherwise claims the oldest queued request (queued -> merging, with
//     the new job id) and creates its merge job, pinned to the integration
//     foreman, from the code-generated task (jobs.MergeJobSpec).
//
// The merge job's terminal transition (onTerminal -> finishMerge) closes
// the request: done -> landed; failed|timed_out|dead -> needs_attention
// (plus an operator alert, source factory:merge_queue); cancelled ->
// cancelled - and kicks the loop so the next request dispatches at once.
//
// Exactly-one is guaranteed three ways: passes are serialized by
// mergeMu (the loop, kicks and test-driven passes never interleave), the
// pass checks both the merging rows and live merge jobs before claiming,
// and the store's unique partial index refuses a second merging row.
package factoryapp

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/wire"
)

const (
	// DefaultMergeForeman is the integration foreman merge jobs are pinned
	// to (spec §6.11); FACTORY_MERGE_FOREMAN or Config.MergeForeman
	// overrides it.
	DefaultMergeForeman = "foreman:server"
	// EnvMergeForeman overrides the integration foreman.
	EnvMergeForeman = "FACTORY_MERGE_FOREMAN"
	// mergeAlertSource is the alert source of needs_attention alerts.
	mergeAlertSource = "factory:merge_queue"
	// alertPriorityMerge: P1, never expires - a stuck merge blocks
	// nothing else but always needs a human.
	alertPriorityMerge = 1
)

// mergeQueueTick is the safety-net cadence; kicks make the common path
// immediate. A var so tests can shorten it.
var mergeQueueTick = 5 * time.Second

// kickMergeQueue asks the loop for a pass now (non-blocking, coalescing).
func (a *App) kickMergeQueue() {
	select {
	case a.mergeKick <- struct{}{}:
	default:
	}
}

// mergeQueueLoop runs passes on the ticker and on kicks until ctx is done.
// Pass errors are logged, never fatal (the next tick retries).
func (a *App) mergeQueueLoop(ctx context.Context) {
	ticker := time.NewTicker(mergeQueueTick)
	defer ticker.Stop()
	for {
		if _, err := a.MergeQueuePass(ctx); err != nil && ctx.Err() == nil {
			log.Printf("factoryapp: merge queue: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.mergeKick:
		}
	}
}

// MergeQueuePass runs one serialized pass (see the file comment) and
// returns the id of the request it dispatched ("" when none). Exported for
// tests and the loop alike (the PendingAlertPass pattern).
func (a *App) MergeQueuePass(ctx context.Context) (string, error) {
	a.mergeMu.Lock()
	defer a.mergeMu.Unlock()

	if err := a.reconcileMerging(ctx); err != nil {
		return "", fmt.Errorf("reconcile: %w", err)
	}
	a.alertStalledDependencies(ctx)
	paused, _, err := a.jobs.MergePaused(ctx)
	if err != nil {
		return "", fmt.Errorf("pause flag: %w", err)
	}
	if paused {
		return "", nil
	}
	merging, err := a.jobs.MergingRequests(ctx)
	if err != nil {
		return "", fmt.Errorf("merging: %w", err)
	}
	if len(merging) > 0 {
		return "", nil
	}
	if n, err := a.jobs.ActiveMergeJobs(ctx); err != nil {
		return "", fmt.Errorf("active merge jobs: %w", err)
	} else if n > 0 {
		return "", nil
	}
	mr, ok, err := a.jobs.OldestQueuedMerge(ctx)
	if err != nil || !ok {
		return "", err
	}

	// Claim first, then create: a crash in between leaves a merging row
	// whose job is missing, which the next pass's reconcile closes as
	// needs_attention - never two merges.
	jobID := wire.NewJobID()
	claimed, err := a.jobs.ClaimMerge(ctx, mr.ID, jobID)
	if err != nil || !claimed {
		return "", err
	}
	mr.JobID, mr.State = jobID, jobs.MergeMerging
	spec := jobs.MergeJobSpec(mr, jobID, a.mergeForeman)
	taskID := a.applyMergeTask(ctx, &spec, mr)
	if err := a.createPinnedJob(ctx, spec, taskID); err != nil {
		a.mergeNeedsAttention(ctx, mr.ID, "could not create merge job "+jobID+": "+err.Error())
		return "", fmt.Errorf("create merge job for %s: %w", mr.ID, err)
	}
	// A cancel that landed between claim and create found no job to
	// cancel; cancel the fresh job now so it never runs for a dead request.
	if cur, ok, err := a.jobs.GetMergeRequest(ctx, mr.ID); err == nil && ok && cur.State != jobs.MergeMerging {
		if _, err := a.jobs.Cancel(ctx, jobID, jobs.StateCancelled, "merge request "+mr.ID+" is "+cur.State); err != nil {
			log.Printf("factoryapp: merge queue: cancel orphaned merge job %s: %v", jobID, err)
		}
		return "", nil
	}
	log.Printf("factoryapp: merge queue: %s (%s @ %s) merging as %s on %s", mr.ID, mr.Branch, mr.Head, jobID, a.mergeForeman)
	return mr.ID, nil
}

// applyMergeTask gives a merge job the priority of the task owning the
// request's source job (spec §6.11) and returns that task's id, so the job
// is created and linked under it and a P0 task's merge passes the usage
// gate. JobSpec priority is 1-9, so a P0 task maps to 1 (the P0 exemption
// itself comes from the task link). No source job, no task, or a failed
// lookup keeps the default jobs.DefaultMergeJobPriority and returns "".
// Evaluated at job creation, so re-queues see the current task priority.
func (a *App) applyMergeTask(ctx context.Context, spec *wire.JobSpec, mr jobs.MergeRequest) string {
	if mr.SourceJobID == "" || a.tasks == nil {
		return ""
	}
	taskID, ok, err := a.resolveJobTask(ctx, mr.SourceJobID)
	if err != nil || !ok {
		return ""
	}
	t, err := a.tasks.GetTask(ctx, taskID)
	if err != nil {
		return ""
	}
	spec.Priority = min(max(t.Priority, wire.MinPriority), wire.MaxPriority)
	return taskID
}

// createPinnedJob stores spec through the same gate and pin path as the
// scheduler (spec §6.7, §6.10): held when the usage throttle holds,
// otherwise created and dispatched to the pinned foreman when it is
// online (pending - and alerted on by the long-pending alert - when not).
func (a *App) createPinnedJob(ctx context.Context, spec wire.JobSpec, taskID string) error {
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
			log.Printf("factoryapp: link merge job %s to task %s: %v", spec.ID, taskID, err)
		}
	}
	if hold {
		return nil
	}
	pin, found, err := jobs.FindForeman(ctx, a.jobs, spec.Foreman)
	switch {
	case err != nil:
		log.Printf("factoryapp: merge job %s left pending: pin lookup: %v", spec.ID, err)
	case !found:
		log.Printf("factoryapp: merge job %s left pending: integration foreman %s not registered", spec.ID, spec.Foreman)
	case !pin.Online:
		log.Printf("factoryapp: merge job %s left pending: integration foreman %s offline", spec.ID, spec.Foreman)
	default:
		return a.jobs.Dispatch(ctx, spec.ID, pin.ID)
	}
	return nil
}

// reconcileMerging closes merging requests whose job is terminal (a
// missed hook) or missing.
func (a *App) reconcileMerging(ctx context.Context) error {
	merging, err := a.jobs.MergingRequests(ctx)
	if err != nil {
		return err
	}
	for _, mr := range merging {
		job, ok, err := a.jobs.Get(ctx, mr.JobID)
		if err != nil {
			return err
		}
		if !ok {
			a.mergeNeedsAttention(ctx, mr.ID, "merge job "+mr.JobID+" does not exist (factory restarted mid-dispatch?)")
			continue
		}
		switch {
		case jobs.Terminal(job.State):
			a.finishMerge(ctx, job.ID, job.State)
		case job.State == jobs.StateFailed:
			// Merge jobs never retry (the consumer dead-letters them);
			// finish the job here if that step was missed. MarkDead
			// fires the terminal hook, which closes the request.
			if err := a.jobs.MarkDead(ctx, job.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// finishMerge closes jobID's merging request after the job reached
// jobState (spec §6.11) and alerts on needs_attention. Called from the
// terminal hook and reconciliation; a no-op for non-merge jobs.
func (a *App) finishMerge(ctx context.Context, jobID, jobState string) {
	mr, applied, err := a.jobs.FinishMergeForJob(ctx, jobID, jobState)
	if err != nil {
		log.Printf("factoryapp: merge queue: finish for job %s: %v", jobID, err)
		return
	}
	if !applied {
		return
	}
	log.Printf("factoryapp: merge queue: %s %s (job %s %s)", mr.ID, mr.State, jobID, jobState)
	if mr.State == jobs.MergeNeedsAttention {
		mr.Failure = jobs.MergeFailureOther
		if job, ok, err := a.jobs.Get(ctx, jobID); err == nil && ok {
			mr.Failure = jobs.ClassifyMergeFailure(job)
		}
		if err := a.jobs.SetMergeFailure(ctx, mr.ID, mr.Failure); err != nil {
			log.Printf("factoryapp: merge queue: record failure of %s: %v", mr.ID, err)
		}
	}
	// Alert, fix job, planner note, auditor proposal (mergeowner.go).
	a.routeMergeOutcome(ctx, mr)
	a.kickMergeQueue()
}

// mergeNeedsAttention moves a merging request to needs_attention with
// reason and alerts.
func (a *App) mergeNeedsAttention(ctx context.Context, id, reason string) {
	mr, applied, err := a.jobs.TransitionMerge(ctx, id, jobs.MergeNeedsAttention, reason)
	if err != nil {
		log.Printf("factoryapp: merge queue: %s needs_attention: %v", id, err)
		return
	}
	if applied {
		mr.Failure = jobs.MergeFailureOther
		if err := a.jobs.SetMergeFailure(ctx, mr.ID, mr.Failure); err != nil {
			log.Printf("factoryapp: merge queue: record failure of %s: %v", mr.ID, err)
		}
		a.routeMergeOutcome(ctx, mr)
	}
}

// alertMerge raises the operator alert for a needs_attention request
// (spec §6.11, §9.5.3). For a job-owned request the alert is linked to
// the job's task and why says why it was not fixed automatically.
func (a *App) alertMerge(ctx context.Context, mr jobs.MergeRequest, why string) {
	body := fmt.Sprintf("repo %s, branch %s, head %s\nmerge job: %s\n%s\n\nThe merge queue continues with the next request; "+
		"fix the cause and re-request with POST /v1/merge-requests (or the request_merge tool).",
		mr.Repo, mr.Branch, mr.Head, mr.JobID, mr.Result)
	if mr.SourceJobID != "" {
		body += fmt.Sprintf("\n\nRequested by job %s (fix round %d of %d). Not fixed automatically: %s.",
			mr.SourceJobID, mr.FixRound, jobs.MaxMergeFixRounds, why)
	}
	a.raiseSystemAlertForTask(ctx, alertPriorityMerge,
		"Merge "+mr.ID+" ("+mr.Branch+") needs attention", body,
		mergeAlertSource, a.knownTask(ctx, a.jobThread(ctx, mr.SourceJobID)))
}

// escalateMerge gives a failed merge job its one retry on the hardest tier
// (spec §6.11): when jobs.MergeEscalatable, the request is re-pointed at a
// fresh hardest-tier job (same request, same pin) and the failed job is
// dead-lettered. The request stays merging throughout, so the queue never
// sees a gap. The escalated job runs on the hardest tier, so a second
// failure is never escalated: there is no retry loop. Returns whether the
// failure was handled here.
func (a *App) escalateMerge(ctx context.Context, failed jobs.Job) bool {
	if !jobs.MergeEscalatable(failed) {
		return false
	}
	mr, ok, err := a.jobs.MergeRequestForJob(ctx, failed.ID)
	if err != nil || !ok || mr.State != jobs.MergeMerging {
		return false
	}
	a.mergeMu.Lock()
	defer a.mergeMu.Unlock()
	newID := wire.NewJobID()
	if swapped, err := a.jobs.SwapMergeJob(ctx, mr.ID, failed.ID, newID); err != nil || !swapped {
		return false
	}
	// The request no longer points at the failed job, so its terminal
	// hook closes nothing.
	if err := a.jobs.MarkDead(ctx, failed.ID); err != nil {
		log.Printf("factoryapp: merge queue: dead-letter escalated merge job %s: %v", failed.ID, err)
	}
	mr.JobID = newID
	escSpec := jobs.MergeEscalatedJobSpec(mr, newID, a.mergeForeman)
	escTask := a.applyMergeTask(ctx, &escSpec, mr)
	if err := a.createPinnedJob(ctx, escSpec, escTask); err != nil {
		a.mergeNeedsAttention(ctx, mr.ID, "could not create escalated merge job "+newID+": "+err.Error())
		return true
	}
	log.Printf("factoryapp: merge queue: %s escalated from %s to hardest-tier job %s", mr.ID, failed.ID, newID)
	return true
}

// isMergeQueueJob reports whether jobID is a merge-queue job (by request
// or by creator, so an escalated or orphaned merge job still counts).
func (a *App) isMergeQueueJob(ctx context.Context, jobID string) bool {
	if jobID == "" {
		return false
	}
	if a.isMergeJob(ctx, jobID) {
		return true
	}
	j, ok, err := a.jobs.Get(ctx, jobID)
	return err == nil && ok && j.Spec.CreatedBy == jobs.MergeJobCreatedBy
}

// isMergeJob reports whether jobID is a merge-queue job.
func (a *App) isMergeJob(ctx context.Context, jobID string) bool {
	_, ok, err := a.jobs.MergeRequestForJob(ctx, jobID)
	return err == nil && ok
}

// mergeForemanFrom resolves the integration foreman: Config, then env,
// then the default; always the full foreman:{id} form.
func mergeForemanFrom(cfgValue, envValue string) string {
	v := cfgValue
	if v == "" {
		v = envValue
	}
	if v == "" {
		v = DefaultMergeForeman
	}
	return registry.InboxID(v)
}
