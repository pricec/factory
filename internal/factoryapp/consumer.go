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
			Event string `json:"event"`
		}
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			log.Printf("factoryapp: consumer: event payload for %s: %v", env.JobID, err)
			return
		}
		if err := a.jobs.ApplyEvent(ctx, env.JobID, p.Event); err != nil {
			log.Printf("factoryapp: consumer: apply event %q for %s: %v", p.Event, env.JobID, err)
		}
	case "brain.failed":
		// A foreman's brain session failed on a delegated envelope
		// (spec §3.2, 2026-11-15 amendment): relay it into the operator
		// inbox so the planner wakes, handles the underlying request
		// itself (create_job is the reliable path), and tells the
		// operator - the failure must never die silently.
		if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, env); err != nil {
			log.Printf("factoryapp: consumer: relay brain.failed to planner: %v", err)
		}
	default:
		if !seenTypes[env.Type] {
			seenTypes[env.Type] = true
			log.Printf("factoryapp: consumer: no handler for envelope type %q (job_id %q); acked", env.Type, env.JobID)
		}
	}
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
