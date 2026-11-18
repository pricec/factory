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
	"github.com/pricec/factory/internal/mcpfactory"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// Alerts (inbox redesign S3, spec §9.5.3): the factory's own producers,
// the planner's raise_alert tool and foremen's alert.raise envelopes all
// create rows in the S1 alerts table through raiseAlert. Expiry is lazy
// (every read) plus a ticker; both go through tasks.Store.ExpireAlerts.

const (
	// AlertRaiseType is the foreman->factory envelope type (spec §9.5.3).
	AlertRaiseType = "alert.raise"
	// alertChangedType is the synthetic, NON-persisted SSE envelope type
	// published on planner:main whenever an alert is created or changes
	// state (contract I5). Payload = the Alert JSON.
	alertChangedType = "alert.changed"

	// Default alert priorities of the factory's own producers.
	alertPriorityPending       = 1 // notify.pending
	alertPriorityUnhandled     = 2 // notify.unhandled
	alertPriorityPlannerFailed = 0 // planner-session failure (S5 glue)
	alertPriorityUnattributed  = 2 // unattributed foreman.report / brain.failed (S5 glue)

	// Default TTLs (operator decision 5): P0/P1 never expire.
	alertTTLP2    = 24 * time.Hour
	alertTTLOther = 4 * time.Hour

	maxAlertTitle = 200
	maxAlertBody  = 8 << 10
	maxAlertBody2 = 16 << 10 // request body cap for ack/reply
)

// alertExpiryTick is how often the expiry ticker sweeps (a var so tests
// can shorten it).
var alertExpiryTick = time.Minute

// alertDefaultTTL returns the default TTL for a priority; zero = never.
func alertDefaultTTL(priority int) time.Duration {
	switch {
	case priority <= 1:
		return 0
	case priority == 2:
		return alertTTLP2
	default:
		return alertTTLOther
	}
}

// alertExpiry resolves the expires_at for a new alert: no_expiry wins
// over everything, ttl_s overrides the priority default (P0 included).
func alertExpiry(now time.Time, priority int, ttlS int, noExpiry bool) *time.Time {
	if noExpiry {
		return nil
	}
	ttl := alertDefaultTTL(priority)
	if ttlS > 0 {
		ttl = time.Duration(ttlS) * time.Second
	}
	if ttl <= 0 {
		return nil
	}
	t := now.Add(ttl).UTC()
	return &t
}

// raiseAlert validates and stores one alert, then publishes alert.changed.
// Errors wrap tasks.ErrInvalid for caller mistakes.
func (a *App) raiseAlert(ctx context.Context, in mcpfactory.RaiseAlertInput, source string) (tasks.Alert, error) {
	in.Title = strings.TrimSpace(in.Title)
	switch {
	case in.Priority < 0:
		return tasks.Alert{}, fmt.Errorf("%w: priority must be >= 0", tasks.ErrInvalid)
	case in.Title == "":
		return tasks.Alert{}, fmt.Errorf("%w: title: required", tasks.ErrInvalid)
	case len(in.Title) > maxAlertTitle:
		return tasks.Alert{}, fmt.Errorf("%w: title: max %d bytes", tasks.ErrInvalid, maxAlertTitle)
	case len(in.Body) > maxAlertBody:
		return tasks.Alert{}, fmt.Errorf("%w: body: max %d bytes", tasks.ErrInvalid, maxAlertBody)
	case in.TTLS < 0:
		return tasks.Alert{}, fmt.Errorf("%w: ttl_s must be > 0", tasks.ErrInvalid)
	case in.TTLS > 0 && in.NoExpiry:
		return tasks.Alert{}, fmt.Errorf("%w: ttl_s and no_expiry are mutually exclusive", tasks.ErrInvalid)
	}
	if in.TaskID != "" {
		if _, err := a.tasks.GetTask(ctx, in.TaskID); err != nil {
			if errors.Is(err, tasks.ErrNotFound) {
				return tasks.Alert{}, fmt.Errorf("%w: task_id %q: unknown task", tasks.ErrInvalid, in.TaskID)
			}
			return tasks.Alert{}, err
		}
	}
	alert, err := a.tasks.CreateAlert(ctx, tasks.NewAlert{
		Priority:  in.Priority,
		Title:     in.Title,
		Body:      in.Body,
		Source:    source,
		ExpiresAt: alertExpiry(a.tasks.Now(), in.Priority, in.TTLS, in.NoExpiry),
		TaskID:    in.TaskID,
	})
	if err != nil {
		return tasks.Alert{}, err
	}
	a.publishAlertChanged(alert)
	return alert, nil
}

// RaisePlannerAlert is the planner MCP's raise_alert backend (source "planner").
func (a *App) RaisePlannerAlert(ctx context.Context, in mcpfactory.RaiseAlertInput) (mcpfactory.RaisedAlert, error) {
	alert, err := a.raiseAlert(ctx, in, "planner")
	if err != nil {
		return mcpfactory.RaisedAlert{}, err
	}
	return mcpfactory.RaisedAlert{ID: alert.ID, State: alert.State, ExpiresAt: alert.ExpiresAt}, nil
}

// raiseSystemAlert is the factory-internal producer path: failures log,
// never propagate (like notifyOperator).
func (a *App) raiseSystemAlert(ctx context.Context, priority int, title, body, source string) {
	a.raiseSystemAlertForTask(ctx, priority, title, body, source, "")
}

// raiseSystemAlertForTask is raiseSystemAlert linked to taskID ("" = none).
func (a *App) raiseSystemAlertForTask(ctx context.Context, priority int, title, body, source, taskID string) {
	if _, err := a.raiseAlert(ctx, mcpfactory.RaiseAlertInput{Priority: priority, Title: title, Body: body, TaskID: taskID}, source); err != nil {
		log.Printf("factoryapp: raise alert %q: %v", title, err)
	}
}

// publishAlertChanged fans a synthetic alert.changed envelope out to the
// planner:main SSE streams (not persisted; contract I5).
func (a *App) publishAlertChanged(alert tasks.Alert) {
	data, err := json.Marshal(alert)
	if err != nil {
		return
	}
	a.hub.Publish(auth.PlannerMainInbox, wire.MessageEnvelope{
		Type:    alertChangedType,
		From:    "factory",
		Payload: data,
		TS:      a.tasks.Now().UTC().Format(time.RFC3339),
	})
}

// handleForemanAlert turns a foreman's alert.raise envelope into an alert
// with source "foreman:<id>" (env.From is the foreman's inbox id,
// "foreman:<id>"). Malformed payloads log and raise a P2 alert about it so
// the alert is never silently lost.
func (a *App) handleForemanAlert(ctx context.Context, env wire.MessageEnvelope) {
	var in mcpfactory.RaiseAlertInput
	if err := json.Unmarshal(env.Payload, &in); err != nil {
		log.Printf("factoryapp: consumer: alert.raise from %s: bad payload: %v", env.From, err)
		a.raiseSystemAlert(ctx, alertPriorityUnhandled, "Malformed alert.raise from "+env.From,
			"payload could not be parsed: "+err.Error(), "factory:consumer")
		return
	}
	source := env.From
	if !strings.HasPrefix(source, "foreman:") {
		source = "foreman:" + source
	}
	if _, err := a.raiseAlert(ctx, in, source); err != nil {
		log.Printf("factoryapp: consumer: alert.raise from %s: %v", env.From, err)
		a.raiseSystemAlert(ctx, alertPriorityUnhandled, "Rejected alert.raise from "+env.From,
			err.Error(), "factory:consumer")
	}
}

// ExpireAlertsPass expires every open alert past its expires_at and
// publishes alert.changed for each. Exported for tests and the ticker.
func (a *App) ExpireAlertsPass(ctx context.Context) (int, error) {
	expired, err := a.tasks.ExpireAlerts(ctx, a.tasks.Now())
	for _, al := range expired {
		a.publishAlertChanged(al)
	}
	return len(expired), err
}

func (a *App) alertExpiryLoop(ctx context.Context) {
	ticker := time.NewTicker(alertExpiryTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := a.ExpireAlertsPass(ctx); err != nil {
				log.Printf("factoryapp: alert expiry: %v", err)
			}
		}
	}
}

// handleListAlerts serves GET /v1/alerts: open alerts only, priority ASC
// then created_at DESC; expiry applied lazily first.
func (a *App) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	if _, err := a.ExpireAlertsPass(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	list, err := a.tasks.ListAlerts(r.Context(), tasks.AlertFilter{States: []string{tasks.AlertOpen}})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if list == nil {
		list = []tasks.Alert{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": list})
}

// handleAckAlert serves POST /v1/alerts/{id}/ack.
func (a *App) handleAckAlert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.ExpireAlertsPass(r.Context()); err != nil {
		log.Printf("factoryapp: alert expiry: %v", err)
	}
	alert, err := a.tasks.TransitionAlert(r.Context(), id, []string{tasks.AlertOpen}, tasks.AlertAcked)
	switch {
	case errors.Is(err, tasks.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "unknown alert "+id)
	case errors.Is(err, tasks.ErrStateConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "already_terminal"})
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	default:
		a.publishAlertChanged(alert)
		writeJSON(w, http.StatusOK, map[string]any{"alert": alert})
	}
}

// handleReplyAlert serves POST /v1/alerts/{id}/reply {text}: marks the
// alert replied (when still open), opens a task seeded with the alert and
// posts the reply as a chat.message on it, which wakes the planner. An
// expired or acked alert still gets a task; its state is left alone.
func (a *App) handleReplyAlert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAlertBody2)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Text) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "text: required")
		return
	}
	ctx := r.Context()
	if _, err := a.ExpireAlertsPass(ctx); err != nil {
		log.Printf("factoryapp: alert expiry: %v", err)
	}
	alert, err := a.tasks.GetAlert(ctx, id)
	if errors.Is(err, tasks.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "unknown alert "+id)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// A reply to the Claude login alert, or any reply carrying a Claude
	// token, belongs to the re-login flow (spec §13.3): it is consumed in
	// the alert's task and never stored or shown to the planner.
	if alert.Source == claudeAuthSource || hasClaudeToken(in.Text) {
		taskID := ""
		if alert.Source == claudeAuthSource {
			taskID = a.knownTask(ctx, alert.TaskID)
		}
		if _, tid, ok := a.interceptClaudeChat(ctx, in.Text, taskID); ok {
			updated, err := a.tasks.TransitionAlert(ctx, id, []string{tasks.AlertOpen}, tasks.AlertReplied)
			if err != nil {
				updated = alert
			}
			a.publishAlertChanged(updated)
			writeJSON(w, http.StatusCreated, map[string]any{"alert": updated, "task_id": tid})
			return
		}
	}
	title := alert.Title
	if len(title) > 60 {
		title = title[:60]
	}
	task, err := a.tasks.CreateTask(ctx, title, alert.Priority)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Seed: the alert itself as the thread's first (operator-visible) row.
	seed, _ := json.Marshal(map[string]any{"text": alertSeedText(alert)})
	if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, wire.MessageEnvelope{
		Type: tasks.TypeBrainReply, From: "factory", Payload: seed, ThreadID: task.ID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// State change after the task exists, so a failure above leaves the
	// alert open for a retry. A conflict (already acked/expired/replied)
	// is fine: only the state is left alone.
	updated, err := a.tasks.TransitionAlert(ctx, id, []string{tasks.AlertOpen}, tasks.AlertReplied)
	if err != nil && !errors.Is(err, tasks.ErrStateConflict) {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if updated, err = a.tasks.SetAlertTask(ctx, id, task.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data, _ := json.Marshal(map[string]any{"text": in.Text})
	if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, wire.MessageEnvelope{
		Type: tasks.TypeChatMessage, From: auth.HumanOperatorIdentity, Payload: data, ThreadID: task.ID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	a.publishAlertChanged(updated)
	writeJSON(w, http.StatusCreated, map[string]any{"alert": updated, "task_id": task.ID})
}

// alertSeedText renders the alert as the first message of its reply task.
func alertSeedText(al tasks.Alert) string {
	s := fmt.Sprintf("[P%d alert from %s] %s", al.Priority, al.Source, al.Title)
	if al.Body != "" {
		s += "\n" + al.Body
	}
	return s
}

// mountAlertRoutes registers the alert HTTP surface (spec §9.5.3). Same
// unauthenticated UI-surface class as /v1/chat (§7.5).
func (a *App) mountAlertRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/alerts", a.handleListAlerts)
	mux.HandleFunc("POST /v1/alerts/{id}/ack", a.handleAckAlert)
	mux.HandleFunc("POST /v1/alerts/{id}/reply", a.handleReplyAlert)
}
