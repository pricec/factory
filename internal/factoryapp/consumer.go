package factoryapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/registry"
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
		if err := a.jobs.ApplyResult(ctx, res); err != nil {
			log.Printf("factoryapp: consumer: apply result %s: %v", res.JobID, err)
			return
		}
		job, ok, err := a.jobs.Get(ctx, res.JobID)
		if err != nil || !ok {
			return
		}
		// Arm the retry only when the job actually failed and the result
		// carried an error kind (spec §6.4).
		if job.State == jobs.StateFailed && res.Error != nil {
			if err := a.jobs.MarkFailedForRetry(ctx, job.ID, string(res.Error.Kind)); err != nil {
				log.Printf("factoryapp: consumer: retry scheduling for %s: %v", job.ID, err)
			}
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
)

// relayToPlanner copies one factory:main envelope (type, job id, sender,
// payload) into the operator inbox, stamped with its task (spec §9.5.2
// relay stamping): the job resolves to a task through task_jobs, following
// ParentJobID for spawned children. A report that cannot be attributed
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
				fmt.Sprintf("%s from %s (job %q) is not linked to any task; opened task %s.", env.Type, env.From, env.JobID, t.ID),
				"factory:relay", t.ID)
		}
	}
	if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, relay); err != nil {
		log.Printf("factoryapp: consumer: relay %s from %s to planner: %v", env.Type, env.From, err)
		return
	}
	a.publishTaskChanged(ctx, relay.ThreadID)
}

// notifyUnhandled raises a P2 alert (spec §9.5.2) about a factory:main envelope no
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
	target := registry.InboxID(job.ForemanID)
	if _, err := a.inbox.Post(ctx, target, env); err != nil {
		return fmt.Errorf("post to %s: %w", target, err)
	}
	return nil
}
