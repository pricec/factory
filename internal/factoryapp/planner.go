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
// chat.message envelopes from the conversational UI and foreman reports
// (foreman.report, brain.failed, relayed from factory:main) wake the LLM
// planner - ONE session per touched task, in parallel across tasks,
// serialized and coalesced within a task (spec §9.5.2, wakeDispatcher);
// everything else (brain.reply, notify.*) is informational and is acked
// without a wake. Envelopes are acked once handed to the dispatcher; the
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
			if wakesPlanner(env.Type) {
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

// wakesPlanner reports whether an operator-inbox envelope type wakes the
// planner (spec §9.5): operator chat, and reports from foremen that need
// the operator's attention.
func wakesPlanner(typ string) bool {
	switch typ {
	case "chat.message", foremanReportType, brainFailedType:
		return true
	}
	return false
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
// acted on.
func (a *App) runTaskWake(ctx context.Context, taskID string, wakers []wire.MessageEnvelope) error {
	t, err := a.tasks.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("load task %s: %w", taskID, err)
	}
	thread, err := a.inbox.ThreadMessages(ctx, taskID, plannerThreadCap)
	if err != nil {
		log.Printf("factoryapp: planner: thread %s: %v", taskID, err)
		thread = nil
	}
	criteria, err := a.tasks.PriorityCriteria(ctx)
	if err != nil {
		log.Printf("factoryapp: planner: priority criteria: %v", err)
	}
	reason := "planner: foreman reports arrived on this task; tell the operator what they need and act on them"
	for _, env := range wakers {
		if env.Type == "chat.message" {
			reason = "planner: new chat from the operator on this task needs a reply"
			break
		}
	}
	return a.planner.HandleTask(ctx, PlannerWake{
		Reason:   reason,
		Task:     t,
		Criteria: criteria,
		Thread:   thread,
		Triggers: wakers,
	})
}

// plannerFailed is the dispatcher's failure path: the brain.reply error
// note, posted INTO the task, so the operator is never left waiting on a
// silent planner (spec §9.5.2). Should the threaded post fail, the note
// goes out unthreaded.
func (a *App) plannerFailed(ctx context.Context, taskID string, err error) {
	log.Printf("factoryapp: planner: task %s: %v", taskID, err)
	text := "The planner could not run: " + err.Error() + ". Check the factory service logs (journalctl -u factory)."
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
	a.notifyOperatorThreaded(ctx, a.jobThread(ctx, job.ID), job.ID, "notify.job", map[string]any{
		"job_id":      job.ID,
		"state":       job.State,
		"foreman_id":  job.ForemanID,
		"last_error":  job.LastError,
		"over_budget": job.OverBudget,
		"summary":     lastJobSummary(job.Results),
		"created_at":  job.CreatedAt,
		"updated_at":  job.UpdatedAt,
	})
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
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	a.postChat(w, r, in.Text, in.TaskID)
}

// postChat is the shared body of POST /v1/chat and POST /v1/tasks.
func (a *App) postChat(w http.ResponseWriter, r *http.Request, text, taskID string) {
	ctx := r.Context()
	if strings.TrimSpace(text) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "text: required")
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
	data, err := json.Marshal(map[string]any{"text": text})
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
