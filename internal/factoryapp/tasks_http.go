package factoryapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// Task HTTP constants (spec §9.5.2, I3).
const (
	defaultTaskListLimit = 200
	maxTaskListLimit     = 1000
	// defaultThreadLimit / maxThreadLimit bound GET /v1/tasks/{id}'s
	// message list (the last N of the thread, oldest first).
	defaultThreadLimit = 500
	maxThreadLimit     = 5000
)

// registerTaskRoutes mounts the task surface (spec §9.5.2, I3). Same
// auth as POST /v1/chat: unauthenticated, like the rest of the UI surface
// (§7.5).
func (a *App) registerTaskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tasks", a.handleListTasks)
	mux.HandleFunc("POST /v1/tasks", a.handleCreateTask)
	mux.HandleFunc("GET /v1/tasks/{id}", a.handleGetTask)
	mux.HandleFunc("PATCH /v1/tasks/{id}", a.handleRenameTask)
	mux.HandleFunc("POST /v1/tasks/{id}/close", a.handleCloseTask)
}

// taskTitleLimit bounds an operator rename, in characters (runes). It
// matches the 200 limit on alert titles (spec §9.5.3); the 60-character
// taskTitleMax only shortens titles DERIVED from free text and is too
// tight for a hand-written name. The planner's update_task has no limit
// and is deliberately left unchanged.
const taskTitleLimit = 200

// handleRenameTask serves PATCH /v1/tasks/{id} {"title"} -> 200 {"task":Task}
// (spec §9.5.2). The title is trimmed; empty -> 400, over taskTitleLimit
// characters -> 400, unknown id -> 404. Closed tasks may be renamed (a
// rename is bookkeeping, not a reopen; status is untouched). On success a
// task.changed envelope is published so every open tab updates live. Last
// write wins against a concurrent planner update_task. Unauthenticated like
// the rest of the operator UI surface (§7.5).
func (a *App) handleRenameTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title *string `json:"title"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if in.Title == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "title: required")
		return
	}
	title := strings.TrimSpace(*in.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "title: must not be empty")
		return
	}
	if n := utf8.RuneCountInString(title); n > taskTitleLimit {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("title: too long (%d characters, max %d)", n, taskTitleLimit))
		return
	}
	ctx := r.Context()
	t, err := a.tasks.UpdateTask(ctx, r.PathValue("id"), tasks.TaskUpdate{Title: &title})
	if errors.Is(err, tasks.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown_task", "unknown task "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	a.publishTaskChanged(ctx, t.ID)
	writeJSON(w, http.StatusOK, map[string]any{"task": a.taskView(ctx, t)})
}

// operatorCloseReason is the default close_reason of an operator close.
const operatorCloseReason = "operator"

// handleCloseTask serves POST /v1/tasks/{id}/close {reason?} -> 200
// {task}: the operator closes a task from the UI (the phone inbox's quick
// action). reason defaults to "operator" (at most 200 runes). The close
// clears needs_operator and posts task.closed_by_operator {reason} into
// the task, which wakes the planner so it can cancel the task's jobs; the
// wake does not reopen the task. Closing a closed task is a no-op 200 that
// keeps the original reason and posts nothing. Unknown id -> 404
// unknown_task. Publishes task.changed. Replying reopens the task as usual.
func (a *App) handleCloseTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
			return
		}
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = operatorCloseReason
	}
	if utf8.RuneCountInString(reason) > 200 {
		writeError(w, http.StatusBadRequest, "bad_request", "reason: at most 200 characters")
		return
	}
	id := r.PathValue("id")
	t, err := a.tasks.GetTask(ctx, id)
	if errors.Is(err, tasks.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown_task", "unknown task "+id)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if t.Status != tasks.StatusClosed {
		closed := tasks.StatusClosed
		t, err = a.tasks.UpdateTask(ctx, id, tasks.TaskUpdate{Status: &closed, CloseReason: &reason})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		// Wake the planner on the task (same path as operator chat). The
		// close itself already happened; a failed post only logs.
		data, _ := json.Marshal(map[string]any{"reason": reason, "text": "The operator closed this task: " + reason})
		if _, err := a.postToTask(ctx, id, wire.MessageEnvelope{
			Type:    taskClosedByOperatorType,
			From:    auth.HumanOperatorIdentity,
			Payload: data,
		}); err != nil {
			log.Printf("factoryapp: tasks: notify planner of close %s: %v", id, err)
			a.publishTaskChanged(ctx, id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": a.taskView(ctx, t)})
}

// handleListTasks serves GET /v1/tasks?status=open|closed|all&q=&limit=
// -> {"tasks":[Task]}: open first, then priority ASC, then
// last_activity_at DESC. awaiting=operator|factory keeps only tasks awaiting
// that party (legacy heuristic; the UIs use needs_operator=true|false, the
// planner's explicit flag, triage order priority ASC then oldest activity;
// operator results come in triage order: priority ASC, oldest
// waiting first). status defaults to all; q matches a title
// substring, an id prefix or a YYYY-MM-DD date prefix.
func (a *App) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	switch status {
	case "", "all":
		status = ""
	case tasks.StatusOpen, tasks.StatusClosed:
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "status: want open, closed or all")
		return
	}
	limit, ok := intParam(w, q.Get("limit"), defaultTaskListLimit, maxTaskListLimit)
	if !ok {
		return
	}
	awaiting := q.Get("awaiting")
	switch awaiting {
	case "", tasks.AwaitingOperator, tasks.AwaitingFactory:
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "awaiting: want operator or factory")
		return
	}
	needs := q.Get("needs_operator")
	switch needs {
	case "", "true", "false":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "needs_operator: want true or false")
		return
	}
	filter := tasks.TaskFilter{Status: status, Query: q.Get("q"), Limit: limit}
	if awaiting != "" || needs != "" {
		filter.Limit = 0 // filter first, then cap
	}
	list, err := a.tasks.ListTasks(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]taskView, 0, len(list))
	for _, t := range list {
		v := a.taskView(r.Context(), t)
		if awaiting != "" && v.Awaiting != awaiting {
			continue
		}
		if needs != "" && (v.NeedsOperator && v.Status == tasks.StatusOpen) != (needs == "true") {
			continue
		}
		out = append(out, v)
	}
	if needs == "true" {
		out = orderFlaggedViews(out)
	} else if awaiting == tasks.AwaitingOperator {
		out = orderWaitingViews(out)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
}

// orderFlaggedViews sorts needs_operator views into triage order:
// priority ASC, then oldest last_activity_at first (the flag has no
// timestamp of its own; the planner's update_task bumps activity).
func orderFlaggedViews(views []taskView) []taskView {
	items := make([]tasks.WaitingItem, len(views))
	byID := make(map[string]taskView, len(views))
	for i, v := range views {
		items[i] = tasks.WaitingItem{ID: v.ID, Priority: v.Priority, Since: v.LastActivityAt}
		byID[v.ID] = v
	}
	tasks.OrderWaiting(items)
	out := make([]taskView, len(items))
	for i, it := range items {
		out[i] = byID[it.ID]
	}
	return out
}

// orderWaitingViews sorts awaiting-operator views into triage order
// (tasks.OrderWaiting): priority ASC, oldest waiting first.
func orderWaitingViews(views []taskView) []taskView {
	items := make([]tasks.WaitingItem, len(views))
	byID := make(map[string]taskView, len(views))
	for i, v := range views {
		since, _ := time.Parse(time.RFC3339Nano, v.AwaitingSince)
		items[i] = tasks.WaitingItem{ID: v.ID, Priority: v.Priority, Since: since}
		byID[v.ID] = v
	}
	tasks.OrderWaiting(items)
	out := make([]taskView, len(items))
	for i, it := range items {
		out[i] = byID[it.ID]
	}
	return out
}

// taskMessage is one thread row of GET /v1/tasks/{id} (I3).
type taskMessage struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	From     string          `json:"from"`
	JobID    string          `json:"job_id"`
	TS       string          `json:"ts"`
	Payload  json.RawMessage `json:"payload"`
	Internal bool            `json:"internal"`
}

// handleGetTask serves GET /v1/tasks/{id}?internal=1&limit= ->
// {"task":Task,"messages":[...]}: the thread oldest first (the last limit
// rows). Internal rows (everything but chat.message and brain.reply) are
// EXCLUDED unless internal=1 (or true). Unknown id -> 404.
func (a *App) handleGetTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := a.tasks.GetTask(ctx, r.PathValue("id"))
	if errors.Is(err, tasks.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown_task", "unknown task "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	q := r.URL.Query()
	limit, ok := intParam(w, q.Get("limit"), defaultThreadLimit, maxThreadLimit)
	if !ok {
		return
	}
	withInternal := q.Get("internal") == "1" || q.Get("internal") == "true"
	thread, err := a.inbox.ThreadMessages(ctx, t.ID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	msgs := make([]taskMessage, 0, len(thread))
	for _, env := range thread {
		internal := tasks.IsInternal(env.Type)
		if internal && !withInternal {
			continue
		}
		payload := env.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("null")
		}
		msgs = append(msgs, taskMessage{
			ID: env.ID, Type: env.Type, From: env.From, JobID: env.JobID,
			TS: env.TS, Payload: payload, Internal: internal,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": a.taskView(ctx, t), "messages": msgs})
}

// handleCreateTask serves POST /v1/tasks {text}: the "new task" box, an
// alias of POST /v1/chat without task_id (same 201 body).
func (a *App) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	a.postChat(w, r, in.Text, "")
}

// intParam parses an optional positive integer query parameter, clamped
// to max; it writes a 400 and returns ok=false on garbage.
func intParam(w http.ResponseWriter, raw string, def, max int) (int, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "bad_request", "limit: want a positive integer")
		return 0, false
	}
	if n > max {
		n = max
	}
	return n, true
}
