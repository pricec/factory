package factoryapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// Operator conversation constants (spec §9.5).
const (
	// plannerThreadCap bounds the task thread replayed per wake (spec
	// §9.5.2: the task's last 40 messages, internal included; each
	// payload is llmctx-capped at 2KB, spec §6.6, so the prompt stays
	// bounded).
	plannerThreadCap = 40
	// plannerLimit / plannerWait bound the planner loop's inbox drain.
	plannerLimit = 20
	plannerWait  = 5 * time.Second
)

// plannerLoop drains the operator inbox (planner:main, spec §9.5):
// chat.message envelopes from the conversational UI, foreman reports
// (foreman.report, brain.failed, relayed from factory:main) and terminal
// notify.job envelopes threaded to a task wake the LLM planner - ONE
// session per touched task, in parallel across tasks, serialized and
// coalesced within a task (spec §9.5.2, wakeDispatcher); everything else
// (brain.reply, unthreaded or planner-initiated notify.job, other
// notify.*) is informational and is acked without a wake. Envelopes are acked once handed to the dispatcher; the
// dispatcher persists its in-flight set so a restart re-runs interrupted
// wakes (recoverWakes). Acked history stays replayable - acking never
// deletes.
func (a *App) plannerLoop(ctx context.Context) {
	a.recoverWakes(ctx)
	defer a.wakes.Wait()
	for {
		envs, err := a.inbox.Get(ctx, auth.PlannerMainInbox, plannerLimit, plannerWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("factoryapp: planner: get: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if len(envs) == 0 {
			continue
		}
		var wakers []wire.MessageEnvelope
		for _, env := range envs {
			if wakesPlanner(env) {
				wakers = append(wakers, env)
			}
		}
		if len(wakers) > 0 {
			a.dispatchWakers(ctx, wakers)
		}
		ids := make([]string, 0, len(envs))
		for _, env := range envs {
			ids = append(ids, env.ID)
		}
		if err := a.inbox.Ack(ctx, auth.PlannerMainInbox, ids); err != nil {
			log.Printf("factoryapp: planner: ack: %v", err)
		}
	}
}

// wakesPlanner reports whether an operator-inbox envelope wakes the
// planner (spec §9.5): operator chat, an operator close, reports from foremen that need the
// operator's attention, and a terminal notify.job so the operator hears
// the outcome without asking. notify.job wakes only when it is threaded
// to a task (an unattributed one must not open a task) and was not
// initiated by the planner itself (cancel_job, see notifyOperatorJob).
// notify.job is only posted on terminal transitions, so non-terminal
// events never reach here. The legacy notify.pending is no longer
// produced (long-pending jobs raise a P1 alert, §9.5.3, which is
// deliberately not a wake) and stays informational.
func wakesPlanner(env wire.MessageEnvelope) bool {
	switch env.Type {
	case "chat.message", alertRaisedType, foremanReportType, brainFailedType, taskClosedByOperatorType:
		return true
	case notifyJobType:
		return env.ThreadID != "" && !selfInitiatedJob(env) && !mergeJobNote(env)
	case notifyMergeType:
		// Job-owned merge outcomes (spec §6.11): only the ones that
		// leave something to say or coordinate wake (mergeowner.go).
		return env.ThreadID != "" && wakingMerge(env)
	}
	return false
}

// notifyJobType is the terminal-job envelope type (spec §9.5).
const notifyJobType = "notify.job"

// mergeJobNote reports whether a notify.job payload is a merge job's own
// terminal record (see notifyOperatorJob): never a wake.
func mergeJobNote(env wire.MessageEnvelope) bool {
	var p struct {
		MergeJob bool `json:"merge_job"`
	}
	return json.Unmarshal(env.Payload, &p) == nil && p.MergeJob
}

// selfInitiatedJob reports whether a notify.job payload is marked as the
// planner's own cancel_job (nothing new to tell the operator).
func selfInitiatedJob(env wire.MessageEnvelope) bool {
	var p struct {
		SelfInitiated bool `json:"self_initiated"`
	}
	return json.Unmarshal(env.Payload, &p) == nil && p.SelfInitiated
}

// dispatchWakers groups waking envelopes by task (spec §9.5.2) and hands
// each group to the wake dispatcher. Every waker gets a task:
// ensureTaskForWaker re-threads or opens one when the envelope carries
// none. If even that fails (store error), the operator gets an unthreaded
// brain.reply note naming the envelope - never silence.
func (a *App) dispatchWakers(ctx context.Context, wakers []wire.MessageEnvelope) {
	groups := map[string][]wire.MessageEnvelope{}
	var order []string
	for _, env := range wakers {
		taskID, err := a.ensureTaskForWaker(ctx, env)
		if err != nil {
			log.Printf("factoryapp: planner: no task for %s %s: %v", env.Type, env.ID, err)
			a.notifyOperator(ctx, "brain.reply", map[string]any{
				"text": fmt.Sprintf("The planner could not file %s %s into a task: %v. Check the factory service logs (journalctl -u factory).",
					env.Type, env.ID, err),
			})
			continue
		}
		env.ThreadID = taskID
		if _, seen := groups[taskID]; !seen {
			order = append(order, taskID)
		}
		groups[taskID] = append(groups[taskID], env)
	}
	for _, taskID := range order {
		a.wakes.Dispatch(ctx, taskID, groups[taskID])
	}
}

// runTaskWake is the dispatcher's session body: one planner session for
// taskID (spec §9.5.2). The prompt carries the task header (id, title,
// priority, status, job ids), the operator's priority criteria, and the
// task's own thread (last plannerThreadCap messages, internal included),
// with the triggering envelopes named. The wake reason names the trigger:
// operator chat needs a reply; a foreman report must be relayed and
// acted on; a terminal job must be reported and followed up.
func (a *App) runTaskWake(ctx context.Context, taskID string, wakers []wire.MessageEnvelope) error {
	t, err := a.tasks.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("load task %s: %w", taskID, err)
	}
	cap := plannerThreadCap
	if t.Standing {
		cap = standingFetchCap(a.standingTurns)
	}
	thread, err := a.inbox.ThreadMessages(ctx, taskID, cap)
	if err != nil {
		log.Printf("factoryapp: planner: thread %s: %v", taskID, err)
		thread = nil
	}
	if cutoff, err := a.tasks.ContextCutoff(ctx, taskID); err != nil {
		log.Printf("factoryapp: planner: context cutoff %s: %v", taskID, err)
	} else {
		thread = afterCutoff(thread, cutoff)
	}
	criteria, err := a.tasks.PriorityCriteria(ctx)
	if err != nil {
		log.Printf("factoryapp: planner: priority criteria: %v", err)
	}
	w := PlannerWake{
		Client:        wakeClient(wakers, thread),
		Reason:        wakeReason(wakers),
		Task:          t,
		Criteria:      criteria,
		Thread:        thread,
		Triggers:      wakers,
		StandingTurns: a.standingTurns,
	}
	if t.Standing {
		w.Reason = reasonStanding
		if !hasType(wakers, "chat.message") && hasType(wakers, alertRaisedType) {
			w.Reason = reasonStandingAlert
		}
		w.Summary, w.SummaryThrough, err = a.tasks.StandingSummary(ctx)
		if err != nil {
			log.Printf("factoryapp: planner: standing summary: %v", err)
		}
	}
	return a.planner.HandleTask(ctx, w)
}

// reasonAlert / reasonStandingAlert: the wake reason of alert.raised.
const (
	reasonAlert         = "planner: new operator alert(s) were raised; triage them"
	reasonStandingAlert = "planner: new alert(s) were raised (alert.raised rows below); no one wrote in the chat. Triage them, do not just relay them"
)

func hasType(envs []wire.MessageEnvelope, typ string) bool {
	for _, e := range envs {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// reasonStanding is the wake reason of the standing Planner chat.
const reasonStanding = "planner: the operator wrote in the standing Planner chat and needs a reply"

// closedByOperatorReason is the wake reason of an operator close (spec
// §9.5.2): stop the task's work, do not reopen it.
const closedByOperatorReason = "planner: the operator closed this task (see the task.closed_by_operator message for the reason). " +
	"Cancel its running, pending and held jobs with cancel_job (leave terminal ones alone), " +
	"do not reopen the task, and do not message the operator unless a job could not be cancelled"

// Wake reasons, one per trigger kind. A mixed batch joins the reasons of
// every kind present (chat first) so no trigger goes uncovered.
const (
	reasonChat   = "planner: new chat from the operator on this task needs a reply"
	reasonReport = "planner: foreman reports arrived on this task; tell the operator what they need and act on them"
	reasonJob    = "planner: a job on this task reached a terminal state (done/failed/dead/cancelled): tell the operator the outcome in plain words. " +
		"Code-change jobs own their work through to the merge: a done job normally queued its own merge (merge_requests in the notify.job payload), " +
		"and the factory tracks that request, dispatches fix jobs when it fails, and sends you a notify.merge with the outcome - " +
		"so do NOT call request_merge for it and do NOT dispatch a merge or fix follow-up; just say the merge is queued. " +
		"Act only where coordination is needed: the job failed, it asks for a decision, or it finished code work without queuing a merge (then call request_merge yourself, never a prose job)"
	reasonMerge = "planner: a job-owned merge request on this task changed state (notify.merge; its text says what happened). " +
		"landed: tell the operator the work is merged, deployed and smoke-tested, naming the running commit, and close the task if nothing else is open; " +
		"if the note says NOT DEPLOYED the task is not done - say so plainly and file the job that adds the deploy script + smoke check. " +
		"escalated, dependency_failed or dependency_stalled: the automatic fix path is exhausted or branches of different jobs need sequencing - " +
		"this is your coordination job: decide the next step (a fix or rebase job filed on THIS task, a re-request, or the operator's decision), do it, and tell the operator. " +
		"Merge events never get a task of their own: work them here, on the task that owns the branch (it may have been reopened for this)"
)

// wakeReason names why a task session wakes, from the batch's envelope types.
func wakeReason(wakers []wire.MessageEnvelope) string {
	var chat, job, merge, report, closed, alert bool
	for _, env := range wakers {
		switch env.Type {
		case taskClosedByOperatorType:
			closed = true
		case "chat.message":
			chat = true
		case alertRaisedType:
			alert = true
		case notifyJobType:
			job = true
		case notifyMergeType:
			merge = true
		default:
			report = true
		}
	}
	var parts []string
	if chat {
		parts = append(parts, reasonChat)
	}
	if alert {
		parts = append(parts, reasonAlert)
	}
	if closed {
		parts = append(parts, closedByOperatorReason)
	}
	if job {
		parts = append(parts, reasonJob)
	}
	if merge {
		parts = append(parts, reasonMerge)
	}
	if report || len(parts) == 0 {
		parts = append(parts, reasonReport)
	}
	return strings.Join(parts, " ALSO: ")
}

// markSelfCancel records that the planner is about to cancel jobID itself
// (cancel_job); the returned func withdraws the mark when the cancel fails.
func (a *App) markSelfCancel(jobID string) func() {
	a.selfCancels.Store(jobID, struct{}{})
	return func() { a.selfCancels.Delete(jobID) }
}

// plannerFailed is the dispatcher's failure path: the brain.reply error
// note, posted INTO the task, so the operator is never left waiting on a
// silent planner (spec §9.5.2). Should the threaded post fail, the note
// goes out unthreaded.
func (a *App) plannerFailed(ctx context.Context, taskID string, err error) {
	log.Printf("factoryapp: planner: task %s: %v", taskID, err)
	text := "The planner could not run: " + err.Error() + ". Check the factory service logs (journalctl -u factory)."
	// P0 alert (S5 glue): the failure note is chat, which the operator may
	// not be watching; the alert is the loud, never-expiring channel.
	defer a.raiseSystemAlertForTask(ctx, alertPriorityPlannerFailed,
		"Planner session failed", err.Error(), "factory:planner", a.knownTask(ctx, taskID))
	data, _ := json.Marshal(map[string]any{"text": text})
	if _, perr := a.postToTask(ctx, taskID, wire.MessageEnvelope{
		Type:    "brain.reply",
		From:    "factory",
		Payload: data,
	}); perr != nil {
		log.Printf("factoryapp: planner: failure note into %s: %v", taskID, perr)
		a.notifyOperator(ctx, "brain.reply", map[string]any{"text": text, "task_id": taskID})
	}
}

// notifyOperator posts one envelope into the operator inbox (spec §9.5).
// Failures are logged, never propagated - notifications must not take down
// the caller (onTerminal fires inside job transitions).
func (a *App) notifyOperator(ctx context.Context, typ string, payload any) {
	a.notifyOperatorThreaded(ctx, "", "", typ, payload)
}

// notifyOperatorThreaded is notifyOperator filed under task threadID ("" =
// unthreaded) and carrying jobID on the envelope; a threaded post also
// publishes task.changed (I5).
func (a *App) notifyOperatorThreaded(ctx context.Context, threadID, jobID, typ string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("factoryapp: notify operator: %v", err)
		return
	}
	env := wire.MessageEnvelope{
		Type:     typ,
		From:     "factory",
		Payload:  data,
		JobID:    jobID,
		ThreadID: threadID,
	}
	if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, env); err != nil {
		log.Printf("factoryapp: notify operator: %v", err)
		return
	}
	a.publishTaskChanged(ctx, threadID)
}

// jobThread resolves jobID to its task for relay stamping (spec §9.5.2);
// "" when unattributed or on a lookup error (logged).
func (a *App) jobThread(ctx context.Context, jobID string) string {
	if jobID == "" {
		return ""
	}
	taskID, ok, err := a.resolveJobTask(ctx, jobID)
	if err != nil {
		log.Printf("factoryapp: tasks: resolve job %s: %v", jobID, err)
		return ""
	}
	if !ok {
		return ""
	}
	return taskID
}

// notifyOperatorJob posts a notify.job envelope when a job reaches a
// terminal state (spec §9.5: bring completions and failures to the
// operator's attention). Called from onTerminal.
func (a *App) notifyOperatorJob(ctx context.Context, jobID string) {
	job, ok, err := a.jobs.Get(ctx, jobID)
	if err != nil || !ok {
		return
	}
	// A cancel the planner made itself (cancel_job) is tagged so it does
	// not wake the planner; the mark is consumed only by a cancel/supersede
	// terminal, never by a racing natural completion.
	self := false
	if job.State == "cancelled" || job.State == "superseded" {
		_, self = a.selfCancels.LoadAndDelete(job.ID)
	}
	payload := map[string]any{
		"self_initiated": self,
		"job_id":         job.ID,
		"state":          job.State,
		"foreman_id":     job.ForemanID,
		"last_error":     job.LastError,
		"over_budget":    job.OverBudget,
		"summary":        lastJobSummary(job.Results),
		"created_at":     job.CreatedAt,
		"updated_at":     job.UpdatedAt,
	}
	// Merges the job queued for its own work (spec §6.11): the planner
	// must not queue or chase them again.
	if reqs := a.jobMergeSummaries(ctx, job.ID); len(reqs) > 0 {
		payload["merge_requests"] = reqs
	}
	// A merge job's outcome reaches the owning task as notify.merge
	// (mergeowner.go); its own notify.job is a record only, so one merge
	// never wakes the planner twice or opens a task.
	if a.isMergeQueueJob(ctx, job.ID) {
		payload["merge_job"] = true
	}
	a.notifyOperatorThreaded(ctx, a.jobThread(ctx, job.ID), job.ID, notifyJobType, payload)
}

// lastJobSummary extracts the last result summary, mirroring the MCP
// list_jobs summary (mcpfactory.lastSummary).
func lastJobSummary(results json.RawMessage) string {
	var entries []struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(results, &entries); err != nil || len(entries) == 0 {
		return ""
	}
	return entries[len(entries)-1].Summary
}

// handleChat serves POST /v1/chat {text, task_id?} (spec §9.5, §9.5.2):
// the conversational UI's send path. Without task_id it opens a new task
// (title = the first 60 characters of the text; the planner may retitle);
// with one it posts into that task, reopening it when closed. An unknown
// task_id is 404. The message is stored as a chat.message envelope in the
// operator inbox, threaded onto the task (streaming to open browsers via
// the SSE hub), and the planner loop wakes on it. 201 returns the stored
// envelope plus "task_id" (every field the old response had is kept).
// Unauthenticated like the rest of the UI surface.
func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text   string `json:"text"`
		TaskID string `json:"task_id"`
		Client string `json:"client"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	a.postChat(w, r, in.Text, in.TaskID, in.Client)
}

// postChat is the shared body of POST /v1/chat and POST /v1/tasks.
func (a *App) postChat(w http.ResponseWriter, r *http.Request, text, taskID, client string) {
	ctx := r.Context()
	if strings.TrimSpace(text) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "text: required")
		return
	}
	// A Claude token or login code is consumed by the re-login flow
	// (spec §13.3) and never stored in a thread or shown to the planner.
	if taskID != "" {
		if _, err := a.tasks.GetTask(ctx, taskID); errors.Is(err, tasks.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown_task", "unknown task "+taskID)
			return
		}
	}
	if taskID != "" && isResetCommand(text) {
		note, err := a.resetTaskContext(r, taskID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, chatResponse{MessageEnvelope: note, TaskID: taskID})
		return
	}
	if posted, tid, ok := a.interceptClaudeChat(ctx, text, taskID); ok {
		writeJSON(w, http.StatusCreated, chatResponse{MessageEnvelope: posted, TaskID: tid})
		return
	}
	if taskID != "" {
		t, err := a.tasks.GetTask(ctx, taskID)
		if errors.Is(err, tasks.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown_task", "unknown task "+taskID)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if t.Status == tasks.StatusClosed {
			if _, err := a.reopenTask(ctx, t.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
		}
	} else {
		t, err := a.tasks.CreateTask(ctx, taskTitle(text), tasks.DefaultPriority)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		taskID = t.ID
	}
	body := map[string]any{"text": text}
	if c := normalizeClient(client); c != "" {
		body["client"] = c
	}
	data, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	posted, err := a.postToTask(ctx, taskID, wire.MessageEnvelope{
		Type:    "chat.message",
		From:    auth.HumanOperatorIdentity,
		Payload: data,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Location", "/v1/inboxes/planner:main/messages/"+posted.ID)
	writeJSON(w, http.StatusCreated, chatResponse{MessageEnvelope: posted, TaskID: taskID})
}

// chatResponse is the 201 body of POST /v1/chat: the stored envelope (as
// before) plus the task it was filed under.
type chatResponse struct {
	wire.MessageEnvelope
	TaskID string `json:"task_id"`
}

// knownTask returns taskID if the task store knows it, else "" (an alert
// linked to an unknown task is rejected, and the failure alert must not be).
func (a *App) knownTask(ctx context.Context, taskID string) string {
	if taskID == "" || a.tasks == nil {
		return ""
	}
	if _, err := a.tasks.GetTask(ctx, taskID); err != nil {
		return ""
	}
	return taskID
}

// Client hints sent with chat messages (spec §9.5.2). Anything else is
// dropped, so the stored payload and the planner prompt only ever see these.
const (
	clientMobile  = "mobile"
	clientDesktop = "desktop"
)

// normalizeClient returns the canonical client hint, or "" when absent or
// unknown (an unknown hint is ignored, never an error).
func normalizeClient(c string) string {
	switch c = strings.ToLower(strings.TrimSpace(c)); c {
	case clientMobile, clientDesktop:
		return c
	}
	return ""
}

// chatClient is the client hint of a chat.message envelope ("" otherwise).
func chatClient(env wire.MessageEnvelope) string {
	if env.Type != "chat.message" {
		return ""
	}
	var p struct {
		Client string `json:"client"`
	}
	if json.Unmarshal(env.Payload, &p) != nil {
		return ""
	}
	return normalizeClient(p.Client)
}

// wakeClient is the device the operator is on, from the newest chat.message
// among the wake's triggers, else the newest one in the thread (a job or
// merge wake still goes to the operator's last known device). "" = unknown.
func wakeClient(wakers, thread []wire.MessageEnvelope) string {
	for _, list := range [][]wire.MessageEnvelope{wakers, thread} {
		for i := len(list) - 1; i >= 0; i-- {
			if list[i].Type != "chat.message" {
				continue
			}
			if c := chatClient(list[i]); c != "" {
				return c
			}
			if list[i].From == auth.HumanOperatorIdentity {
				return "" // newest operator message carries no hint
			}
		}
	}
	return ""
}
