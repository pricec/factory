package factoryapp

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/wire"
)

// Operator conversation constants (spec §9.5).
const (
	// plannerHistoryCap bounds the replayed operator-inbox history per wake
	// (the inbox fetch limit; each payload is llmctx-capped at 2KB, spec
	// §6.6, so the prompt stays bounded).
	plannerHistoryCap = 20
	// plannerLimit / plannerWait bound the planner loop's inbox drain.
	plannerLimit = 20
	plannerWait  = 5 * time.Second
)

// plannerLoop drains the operator inbox (planner:main, spec §9.5):
// chat.message envelopes from the conversational UI wake the LLM planner;
// brain.failed envelopes (relayed by the factory:main consumer, spec
// §3.2, 2026-11-15 amendment) wake it too - a foreman's brain session
// died on a delegation and the planner must handle the request itself
// and tell the operator; everything else (brain.reply, notify.*) is
// informational for the human and is acked without a wake. Acked history
// stays replayable via GET /v1/inboxes/planner:main/recent - acking never
// deletes.
func (a *App) plannerLoop(ctx context.Context) {
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
		var wakes []wire.MessageEnvelope
		for _, env := range envs {
			if env.Type == "chat.message" || env.Type == "brain.failed" {
				wakes = append(wakes, env)
			}
		}
		if len(wakes) > 0 {
			a.wakePlanner(ctx, wakeReason(wakes), wakes)
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

// wakeReason names why the planner is waking: operator chat, or a
// foreman brain failure that needs the planner to take over.
func wakeReason(wakes []wire.MessageEnvelope) string {
	for _, env := range wakes {
		if env.Type == "brain.failed" {
			return "a foreman brain session failed on a message delegated to it - handle the underlying request yourself (create_job is the reliable path) and tell the operator what happened"
		}
	}
	return "planner: new chat from the operator needs a reply"
}

// wakePlanner runs one planner session for the triggering envelopes,
// with the recent operator-inbox history replayed into the prompt. On
// failure it posts a brain.reply error note so the operator is never left
// waiting on a silent brain. After the session it records the planner's
// own token usage in the sessions ledger (spec §6.5 reporting amendment):
// planner sessions are recorded in-process, not via the session.usage
// envelope path foremen use.
func (a *App) wakePlanner(ctx context.Context, reason string, envs []wire.MessageEnvelope) {
	history, err := a.inbox.Recent(ctx, auth.PlannerMainInbox, plannerHistoryCap)
	if err != nil {
		log.Printf("factoryapp: planner: history: %v", err)
		history = nil
	}
	started := a.now()
	if err := a.planner.Handle(ctx, reason, history, envs); err != nil {
		log.Printf("factoryapp: planner: %v", err)
		a.notifyOperator(ctx, "brain.reply", map[string]any{
			"text": "The planner could not run: " + err.Error() + ". Check the factory service logs (journalctl -u factory).",
		})
	}
	// Best-effort even on failure: a session that errored still spent
	// tokens, and a session that never started records none.
	a.recordPlannerUsage(ctx, started.UnixMilli())
}

// notifyOperator posts one envelope into the operator inbox (spec §9.5).
// Failures are logged, never propagated - notifications must not take down
// the caller (onTerminal fires inside job transitions).
func (a *App) notifyOperator(ctx context.Context, typ string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("factoryapp: notify operator: %v", err)
		return
	}
	env := wire.MessageEnvelope{
		Type:    typ,
		From:    "factory",
		Payload: data,
	}
	if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, env); err != nil {
		log.Printf("factoryapp: notify operator: %v", err)
	}
}

// notifyOperatorJob posts a notify.job envelope when a job reaches a
// terminal state (spec §9.5: bring completions and failures to the
// operator's attention). Called from onTerminal.
func (a *App) notifyOperatorJob(ctx context.Context, jobID string) {
	job, ok, err := a.jobs.Get(ctx, jobID)
	if err != nil || !ok {
		return
	}
	a.notifyOperator(ctx, "notify.job", map[string]any{
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

// handleChat serves POST /v1/chat (spec §9.5): the conversational UI's send
// path. It stores the operator's message as a chat.message envelope in the
// operator inbox (so it appears in the chat log immediately, streaming to
// any open browser via the SSE hub) and returns 201. The planner loop picks
// it up from there. Unauthenticated like the rest of the UI surface.
func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if in.Text == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "text: required")
		return
	}
	data, err := json.Marshal(map[string]any{"text": in.Text})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	env := wire.MessageEnvelope{
		Type:    "chat.message",
		From:    auth.HumanOperatorIdentity,
		Payload: data,
	}
	posted, err := a.inbox.Post(r.Context(), auth.PlannerMainInbox, env)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Location", "/v1/inboxes/planner:main/messages/"+posted.ID)
	writeJSON(w, http.StatusCreated, posted)
}
