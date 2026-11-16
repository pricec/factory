package factoryapp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pricec/factory/internal/tasks"
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
}

// handleListTasks serves GET /v1/tasks?status=open|closed|all&q=&limit=
// -> {"tasks":[Task]}: open first, then priority ASC, then
// last_activity_at DESC. awaiting=operator|factory keeps only tasks awaiting
// that party (operator results come in triage order: priority ASC, oldest
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
	filter := tasks.TaskFilter{Status: status, Query: q.Get("q"), Limit: limit}
	if awaiting != "" {
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
		if awaiting == "" || v.Awaiting == awaiting {
			out = append(out, v)
		}
	}
	if awaiting == tasks.AwaitingOperator {
		out = orderWaitingViews(out)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
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
