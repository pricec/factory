// Package routing implements the foreman's message routing rules
// (spec §3.3): job.request -> spawn_worker, job.result -> forward to
// parent byte-identical (the hot path; zero LLM calls), job.event ->
// drop, job.question -> brain with a 60s SLA, brain.wake and anything
// unmatched -> brain. The rule table is Go code in v1 - no config file.
package routing

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// Action is a handler action from the v1 rule table (spec §3.3).
type Action string

const (
	ActionForward     Action = "forward"
	ActionDrop        Action = "drop"
	ActionBrain       Action = "brain"
	ActionSpawnWorker Action = "spawn_worker"
)

// QuestionTimeout is the default brain SLA for job.question envelopes
// (spec §3.3, §11).
const QuestionTimeout = 60 * time.Second

// Brain decides what to do with messages that need LLM judgment
// (spec §3.3). #18 provides the real one; RecordBrain here is the stub.
type Brain interface {
	Handle(ctx context.Context, env wire.MessageEnvelope) error
}

// Poster delivers an envelope to the parent inbox (spec §3.3: forward).
// The integration wires the HTTP client + foreman bearer.
type Poster interface {
	PostToParent(ctx context.Context, env wire.MessageEnvelope) error
}

// Spawner enqueues a JobSpec to the worker harness queue (spec §3.3).
// The integration wires it.
type Spawner interface {
	Spawn(ctx context.Context, spec wire.JobSpec) error
}

// Router applies the v1 rule table (spec §3.3) to envelopes read from
// the foreman's inbox. Handle never blocks the loop: question SLAs are
// bounded, and brain/poster/spawn errors are logged, not returned.
type Router struct {
	Brain   Brain
	Poster  Poster
	Spawner Spawner
	// QuestionTimeout bounds the brain SLA for job.question envelopes;
	// zero means the 60s default (spec §3.3).
	QuestionTimeout time.Duration
}

// route returns the action for an envelope type (spec §3.3).
func route(msgType string) Action {
	switch msgType {
	case "job.request":
		return ActionSpawnWorker
	case "job.result":
		return ActionForward
	case "job.event":
		return ActionDrop
	case "job.question", "brain.wake":
		return ActionBrain
	default:
		return ActionBrain // anything unmatched -> brain
	}
}

// DelegatesToBrain reports whether the v1 rule table (spec §3.3) hands
// msgType to the brain: job.question, brain.wake, and anything unmatched.
// The foreman integration uses it to split its consume loop: these
// envelopes go to the background brain queue (spec §3.3, 2026-11-15
// amendment) so the hot paths never wait on an LLM session, while
// everything else routes inline. The rule table stays the single source
// of truth - this must never disagree with route().
func DelegatesToBrain(msgType string) bool {
	return route(msgType) == ActionBrain
}

func (r *Router) questionSla() time.Duration {
	if r.QuestionTimeout > 0 {
		return r.QuestionTimeout
	}
	return QuestionTimeout
}

// Handle routes env per the v1 rule table. It returns an error only on
// infra failure of the forward (hot) path; every other path logs and
// returns nil so the inbox loop keeps going.
func (r *Router) Handle(ctx context.Context, env wire.MessageEnvelope) error {
	switch route(env.Type) {
	case ActionForward:
		// Byte-identical payload: marshal the same struct, drop and
		// alter nothing (spec §3.4). Never calls the brain.
		if err := r.Poster.PostToParent(ctx, env); err != nil {
			return err
		}
	case ActionSpawnWorker:
		var spec wire.JobSpec
		if err := json.Unmarshal(env.Payload, &spec); err != nil {
			log.Printf("routing: %s %s: invalid JobSpec payload: %v", env.Type, env.ID, err)
			return nil
		}
		if err := r.Spawner.Spawn(ctx, spec); err != nil {
			log.Printf("routing: %s %s: spawn failed: %v", env.Type, env.ID, err)
		}
	case ActionDrop:
		log.Printf("routing: dropping %s %s (state flows through the jobs events path)", env.Type, env.ID)
	case ActionBrain:
		if env.Type == "job.question" {
			// Bounded SLA; on timeout or error, log and continue -
			// the default answer is ask_parent's behavior on the
			// worker side (#17), not the router's.
			qctx, cancel := context.WithTimeout(ctx, r.questionSla())
			defer cancel()
			if err := r.Brain.Handle(qctx, env); err != nil {
				log.Printf("routing: %s %s: brain: %v", env.Type, env.ID, err)
			}
			return nil
		}
		if err := r.Brain.Handle(ctx, env); err != nil {
			log.Printf("routing: %s %s: brain: %v", env.Type, env.ID, err)
		}
	}
	return nil
}
