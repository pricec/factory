package factoryapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

const (
	consumerLimit = 50
	consumerWait  = 5 * time.Second
)

// consumeLoop drains the factory:main inbox via the library API (no HTTP).
// Foreman tokens are post-only on factory:main; results forwarded by the
// foreman (§3.4) are byte-identical job.result payloads.
func (a *App) consumeLoop(ctx context.Context) {
	seenTypes := map[string]bool{}
	for {
		envs, err := a.inbox.Get(ctx, FactoryMainInbox, consumerLimit, consumerWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("factoryapp: consumer: get: %v", err)
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
		ids := make([]string, 0, len(envs))
		for _, env := range envs {
			a.handleEnvelope(ctx, env, seenTypes)
			ids = append(ids, env.ID)
		}
		if err := a.inbox.Ack(ctx, FactoryMainInbox, ids); err != nil {
			log.Printf("factoryapp: consumer: ack: %v", err)
		}
	}
}

// handleEnvelope routes one factory:main envelope (spec §3.2).
func (a *App) handleEnvelope(ctx context.Context, env wire.MessageEnvelope, seenTypes map[string]bool) {
	switch env.Type {
	case "job.result":
		var res wire.JobResult
		if err := json.Unmarshal(env.Payload, &res); err != nil {
			log.Printf("factoryapp: consumer: result payload for %s: %v", env.JobID, err)
			return
		}
		if res.JobID == "" {
			res.JobID = env.JobID
		}
		if a.requeueLimited(ctx, res) {
			return // usage-limit rejection: requeued held, no failure, no retry (S5)
		}
		if err := a.jobs.ApplyResult(ctx, res); err != nil {
			log.Printf("factoryapp: consumer: apply result %s: %v", res.JobID, err)
			return
		}
		job, ok, err := a.jobs.Get(ctx, res.JobID)
		if err != nil || !ok {
			return
		}
		// Merge jobs never retry (spec §6.11): a re-run could merge or
		// deploy twice. A failed merge job goes straight to dead, which
		// closes its request as needs_attention.
		if job.State == jobs.StateFailed && a.isMergeJob(ctx, job.ID) {
			if err := a.jobs.MarkDead(ctx, job.ID); err != nil {
				log.Printf("factoryapp: consumer: dead-letter merge job %s: %v", job.ID, err)
			}
			return
		}
		// Arm the retry only when the job actually failed and the result
		// carried an error kind (spec §6.4).
		if job.State == jobs.StateFailed && res.Error != nil {
			retryKind := string(res.Error.Kind)
			if r := res.Error.Reason; r == wire.ReasonBudget || r == wire.ReasonLoop {
				// Re-running would just spend/loop again: unknown kind =
				// zero retries = dead-letter immediately (§6.4).
				retryKind = r
			}
			if err := a.jobs.MarkFailedForRetry(ctx, job.ID, retryKind); err != nil {
				log.Printf("factoryapp: consumer: retry scheduling for %s: %v", job.ID, err)
			}
		}
	case wire.TypeJobUsage, wire.TypeUsageRecord:
		var ev wire.UsageEvent
		if err := json.Unmarshal(env.Payload, &ev); err != nil {
			log.Printf("factoryapp: consumer: usage payload for %s: %v", env.JobID, err)
			return
		}
		if ev.JobID == "" {
			ev.JobID = env.JobID
		}
		if env.Type == wire.TypeJobUsage {
			ev.Kind = wire.UsageKindWorker
		}
		if err := a.recordUsage(ctx, ev); err != nil {
			log.Printf("factoryapp: consumer: record usage: %v", err)
		}
	case "job.event":
		var p struct {
			Event string        `json:"event"`
			Spec  *wire.JobSpec `json:"spec,omitempty"`
		}
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			log.Printf("factoryapp: consumer: event payload for %s: %v", env.JobID, err)
			return
		}
		// Spawn visibility (spec §6.2, 2026-11-15 spawn-visibility
		// amendment): an accepted event carrying the JobSpec registers a
		// job the factory never created itself - a foreman-local spawn
		// (the §9.3 spawn_worker seam), which otherwise existed only in
		// the foreman's local store. Registration is insert-if-absent, so
		// re-delivered accepted events and factory-created jobs are
		// untouched no-ops: never a duplicate, never a clobber.
		if p.Event == "accepted" && p.Spec != nil {
			switch {
			case env.JobID != "" && p.Spec.ID != env.JobID:
				log.Printf("factoryapp: consumer: accepted event for %s carries spec id %s; registration skipped",
					env.JobID, p.Spec.ID)
			default:
				if _, err := a.jobs.RegisterSpawned(ctx, *p.Spec, env.From); err != nil {
					log.Printf("factoryapp: consumer: register spawned job %s: %v", env.JobID, err)
				}
			}
		}
		if err := a.jobs.ApplyEvent(ctx, env.JobID, p.Event); err != nil {
			log.Printf("factoryapp: consumer: apply event %q for %s: %v", p.Event, env.JobID, err)
		}
	case foremanReportType, brainFailedType:
		// Foreman-originated reports for the operator (spec §3.2, §9.5):
		// relay verbatim into planner:main, where the planner loop wakes
		// on them. This is the path a foreman's post_to_factory alone
		// reaches the operator on - no direct /v1/chat post needed.
		a.relayToPlanner(ctx, env)
	case AlertRaiseType:
		a.handleForemanAlert(ctx, env)
	default:
		// Nothing handles this type. Acking it silently is the parking
		// failure mode (an envelope nobody reads, see spec §9.5): the
		// operator is told instead, once per envelope, without a wake.
		if !seenTypes[env.Type] {
			seenTypes[env.Type] = true
			log.Printf("factoryapp: consumer: no handler for envelope type %q (job_id %q); relayed as notify.unhandled", env.Type, env.JobID)
		}
		a.notifyUnhandled(ctx, env)
	}
}

// Envelope types the consumer relays into planner:main (spec §3.2).
const (
	foremanReportType = "foreman.report"
	brainFailedType   = "brain.failed"
	// taskClosedByOperatorType is posted into planner:main, threaded on
	// the task, when the operator closes a task (POST /v1/tasks/{id}/close,
	// spec §9.5.2). It wakes the planner so it can cancel the task's
	// running/held jobs; it never reopens the task. Planner closes
	// (update_task) post nothing, so there is no wake loop.
	taskClosedByOperatorType = "task.closed_by_operator"
)

// relayToPlanner copies one factory:main envelope (type, job id, sender,
// payload) into the operator inbox, stamped with its task (spec §9.5.2
// relay stamping). Resolution order: (a) the envelope job's task through
// task_jobs, following ParentJobID for spawned children; (b) else the
// payload's task_id, only when it names an existing OPEN task (a closed
// task is treated as not found - never silently reopened by an automated
// report); (c) else a NEW task. If (a) and (b) both resolve but differ,
// the job's task wins and the conflict is logged. A report that cannot be attributed
// opens a NEW task titled from its payload, so it is never lost; the
// planner loop wakes on it there. The relayed row stays internal (hidden
// from the operator's default view). The store assigns a fresh id and ts.
// Failures log; the source envelope is still acked, so a persistent
// planner-inbox failure drops only the copy, with the log line as the
// trace. If task resolution fails the copy is relayed unthreaded and the
// planner loop attributes it (ensureTaskForWaker).
func (a *App) relayToPlanner(ctx context.Context, env wire.MessageEnvelope) {
	relay := wire.MessageEnvelope{
		Type:    env.Type,
		JobID:   env.JobID,
		From:    env.From,
		Payload: env.Payload,
	}
	relay.ThreadID = a.jobThread(ctx, env.JobID)
	hint := a.payloadTaskHint(ctx, env, relay.ThreadID)
	if relay.ThreadID == "" {
		relay.ThreadID = hint.taskID
	}
	if relay.ThreadID == "" {
		t, err := a.openTaskFor(ctx, env)
		if err != nil {
			log.Printf("factoryapp: consumer: open task for unattributed %s from %s: %v", env.Type, env.From, err)
		} else {
			relay.ThreadID = t.ID
			log.Printf("factoryapp: consumer: unattributed %s from %s (job %q) opened task %s", env.Type, env.From, env.JobID, t.ID)
			// S5 glue: also a P2 alert linked to the new task.
			a.raiseSystemAlertForTask(ctx, alertPriorityUnattributed,
				"Unattributed "+env.Type+": "+t.Title,
				fmt.Sprintf("%s from %s (job %q) is not linked to any task; opened task %s.%s", env.Type, env.From, env.JobID, t.ID, hint.note),
				"factory:relay", t.ID)
		}
	}
	if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, relay); err != nil {
		log.Printf("factoryapp: consumer: relay %s from %s to planner: %v", env.Type, env.From, err)
		return
	}
	a.publishTaskChanged(ctx, relay.ThreadID)
}

// notifyUnhandled raises a P2 alert (spec §9.5.3) about a factory:main envelope no
// handler consumes (no chat row, no planner wake). The payload names
// the envelope so the operator can see what was posted and by whom.
func (a *App) notifyUnhandled(ctx context.Context, env wire.MessageEnvelope) {
	body := fmt.Sprintf("envelope %s of type %q from %s (job %q): no handler on factory:main; "+
		"the envelope was acked after this notice", env.ID, env.Type, env.From, env.JobID)
	a.raiseSystemAlert(ctx, alertPriorityUnhandled, "Unhandled envelope type "+env.Type, body, "factory:consumer")
}

// notifyDispatched posts the job.request envelope for one dispatched job.
func (a *App) notifyDispatched(ctx context.Context, job jobs.Job) error {
	specJSON, err := json.Marshal(job.Spec)
	if err != nil {
		return fmt.Errorf("marshal spec: %w", err)
	}
	env := wire.MessageEnvelope{
		Type:    "job.request",
		JobID:   job.ID,
		From:    FactoryMainInbox,
		Payload: specJSON,
	}
	if err := env.Validate(); err != nil {
		return fmt.Errorf("envelope: %w", err)
	}
	// A retry re-dispatches the same job id: free the previous attempt's
	// job.result dedupe slot in the main inbox, or this attempt's result is
	// rejected as a duplicate and the job is later reaped by reconcile.
	if _, err := a.inbox.ReleaseResult(ctx, FactoryMainInbox, job.ID); err != nil {
		return fmt.Errorf("release prior result: %w", err)
	}
	target := registry.InboxID(job.ForemanID)
	if _, err := a.inbox.Post(ctx, target, env); err != nil {
		return fmt.Errorf("post to %s: %w", target, err)
	}
	return nil
}

// notifyCancelled posts a job.cancel envelope to the foreman that owns a
// job cancelled/superseded while dispatched or running, so it kills the
// worker (spec §3.2). The foreman inbox is durable: if the foreman is
// offline the envelope waits and is delivered on reconnect, so no
// separate offline path exists. Failures are logged; the job is already
// terminal and any late result is ignored.
func (a *App) notifyCancelled(job jobs.Job) {
	if job.ForemanID == "" {
		return
	}
	payload, _ := json.Marshal(map[string]string{"job_id": job.ID, "state": job.State})
	env := wire.MessageEnvelope{
		Type:    wire.TypeJobCancel,
		JobID:   job.ID,
		From:    FactoryMainInbox,
		Payload: payload,
	}
	if err := env.Validate(); err != nil {
		log.Printf("factoryapp: job.cancel for %s: %v", job.ID, err)
		return
	}
	target := registry.InboxID(job.ForemanID)
	if _, err := a.inbox.Post(context.Background(), target, env); err != nil {
		log.Printf("factoryapp: job.cancel for %s -> %s: %v", job.ID, target, err)
		return
	}
	log.Printf("factoryapp: job.cancel for %s (%s) -> %s", job.ID, job.State, target)
}

// taskHint is the outcome of reading payload.task_id for relay routing.
type taskHint struct {
	taskID string // the payload task to thread into; "" when unusable or unneeded
	note   string // extra alert-body text explaining an ignored task_id
}

// payloadTaskID reads payload.task_id from a foreman.report-style payload,
// accepting the structured object and the legacy double-encoded JSON string
// (the deploy-21 shape). It returns "" for anything else.
func payloadTaskID(payload json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		var s string
		if json.Unmarshal(payload, &s) != nil {
			return ""
		}
		inner := strings.TrimSpace(s)
		if !strings.HasPrefix(inner, "{") || json.Unmarshal([]byte(inner), &m) != nil {
			return ""
		}
	}
	id, _ := m["task_id"].(string)
	return strings.TrimSpace(id)
}

// payloadTaskHint implements step (b) of the relay resolution order.
// jobTask is step (a)'s result: when set it wins, and a differing payload
// task_id is only logged. A payload task_id that is unknown, closed or
// unreadable yields no taskID (fall through to a new task) and a note for
// the unattributed alert.
func (a *App) payloadTaskHint(ctx context.Context, env wire.MessageEnvelope, jobTask string) taskHint {
	id := payloadTaskID(env.Payload)
	if id == "" {
		return taskHint{}
	}
	t, err := a.tasks.GetTask(ctx, id)
	if err != nil || t.Status != tasks.StatusOpen {
		reason := "does not exist"
		switch {
		case err != nil && !errors.Is(err, tasks.ErrNotFound):
			reason = "could not be read"
			log.Printf("factoryapp: consumer: %s payload task_id %q lookup: %v", env.Type, id, err)
		case err == nil:
			reason = "is closed (reports never reopen a task)"
		}
		log.Printf("factoryapp: consumer: %s from %s: payload task_id %q %s; ignored", env.Type, env.From, id, reason)
		return taskHint{note: fmt.Sprintf(" Its payload named task %s, which %s.", id, reason)}
	}
	if jobTask != "" {
		if jobTask != id {
			log.Printf("factoryapp: consumer: %s from %s: job %q resolves to task %s but payload task_id is %s; keeping the job's task", env.Type, env.From, env.JobID, jobTask, id)
		}
		return taskHint{}
	}
	return taskHint{taskID: id}
}
