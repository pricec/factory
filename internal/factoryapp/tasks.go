package factoryapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// Task semantics constants (spec §9.5.2, inbox redesign S2).
const (
	// taskTitleMax bounds a fallback task title: the first 60 characters
	// of the opening message (the planner may retitle with update_task).
	taskTitleMax = 60
	// taskPreviewMax bounds Task.preview (I3): the last operator-visible
	// message, at most 120 characters.
	taskPreviewMax = 120
	// maxJobLineage bounds the ParentJobID walk when attributing a job to
	// a task (spawn_child is capped at one level today; the bound only
	// guards against a corrupt cycle).
	maxJobLineage = 8
	// taskChangedType is the synthetic, NON-persisted SSE envelope type
	// published on the planner:main stream when a task changes (I5).
	taskChangedType = "task.changed"
)

// taskView is the I3 Task JSON: the stored task plus the preview.
type taskView struct {
	tasks.Task
	Preview string `json:"preview"`
}

// taskTitle derives a fallback task title from free text: whitespace runs
// collapse to one space, the result is trimmed and cut to taskTitleMax
// runes. Empty input yields "".
func taskTitle(text string) string {
	title := strings.Join(strings.FieldsFunc(text, unicode.IsSpace), " ")
	if r := []rune(title); len(r) > taskTitleMax {
		title = strings.TrimSpace(string(r[:taskTitleMax]))
	}
	return title
}

// payloadText extracts the human-readable text of an envelope payload:
// the first non-empty string field among text, title, summary, message,
// error; otherwise the compact raw payload.
func payloadText(payload json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err == nil {
		for _, k := range []string{"text", "title", "summary", "message", "error"} {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	var s string
	if err := json.Unmarshal(payload, &s); err == nil {
		// An object payload some clients deliver as a JSON string
		// ("{\"text\":...}"): title from the inner object, not the raw JSON
		// (deploy-18 follow-up). One level only; plain strings are unchanged.
		if inner := strings.TrimSpace(s); strings.HasPrefix(inner, "{") && json.Valid([]byte(inner)) {
			var m map[string]any
			if json.Unmarshal([]byte(inner), &m) == nil {
				for _, k := range []string{"text", "title", "summary", "message", "error"} {
					if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
						return v
					}
				}
			}
		}
		return s
	}
	return string(payload)
}

// envelopeTaskTitle titles a task opened for an envelope that could not be
// attributed to an existing one (spec §9.5.2): the payload's text, else
// "<type> from <sender> (<job>)".
func envelopeTaskTitle(env wire.MessageEnvelope) string {
	if t := taskTitle(payloadText(env.Payload)); t != "" && t != "null" && t != "{}" {
		return t
	}
	t := env.Type + " from " + env.From
	if env.JobID != "" {
		t += " (" + env.JobID + ")"
	}
	return taskTitle(t)
}

// openTaskFor creates a new open task titled for env (spec §9.5.2: an
// unattributable report becomes its own task, never lost) and publishes
// task.changed.
func (a *App) openTaskFor(ctx context.Context, env wire.MessageEnvelope) (tasks.Task, error) {
	t, err := a.tasks.CreateTask(ctx, envelopeTaskTitle(env), tasks.DefaultPriority)
	if err != nil {
		return tasks.Task{}, err
	}
	if env.JobID != "" {
		if err := a.tasks.LinkJob(ctx, t.ID, env.JobID); err != nil {
			log.Printf("factoryapp: tasks: link %s to new task %s: %v", env.JobID, t.ID, err)
		}
	}
	return t, nil
}

// resolveJobTask attributes jobID to a task (spec §9.5.2): a direct
// task_jobs link, else the nearest ancestor's via the job spec's
// ParentJobID (spawn_child children, registered on the factory through
// RegisterSpawned). A child resolved through its parent is linked to the
// task so later lookups are direct and the reaper sees it. ok is false
// when nothing in the lineage is linked.
func (a *App) resolveJobTask(ctx context.Context, jobID string) (string, bool, error) {
	var walked []string
	cur := jobID
	for i := 0; cur != "" && i < maxJobLineage; i++ {
		taskID, ok, err := a.tasks.TaskForJob(ctx, cur)
		if err != nil {
			return "", false, err
		}
		if ok {
			for _, j := range walked {
				if err := a.tasks.LinkJob(ctx, taskID, j); err != nil {
					log.Printf("factoryapp: tasks: link child %s to %s: %v", j, taskID, err)
				}
			}
			return taskID, true, nil
		}
		walked = append(walked, cur)
		job, found, err := a.jobs.Get(ctx, cur)
		if err != nil {
			return "", false, err
		}
		if !found {
			break
		}
		cur = job.Spec.ParentJobID
	}
	return "", false, nil
}

// ensureTaskForWaker returns the open task a planner-waking envelope
// belongs to (spec §9.5.2), creating or reopening as needed so that no
// waker is ever dropped:
//   - a thread naming an existing task: that task, reopened if closed;
//   - no thread, or a thread naming no task: a job-attributed task when
//     the envelope's job resolves, else a NEW task titled from the
//     payload; the stored row is re-threaded onto it.
func (a *App) ensureTaskForWaker(ctx context.Context, env wire.MessageEnvelope) (string, error) {
	if env.ThreadID != "" {
		t, err := a.tasks.GetTask(ctx, env.ThreadID)
		switch {
		case err == nil:
			if t.Status == tasks.StatusClosed {
				if _, err := a.reopenTask(ctx, t.ID); err != nil {
					log.Printf("factoryapp: tasks: reopen %s: %v", t.ID, err)
				}
			}
			return t.ID, nil
		case !errors.Is(err, tasks.ErrNotFound):
			return "", err
		}
		log.Printf("factoryapp: planner: %s %s names unknown task %q; re-threading", env.Type, env.ID, env.ThreadID)
	}
	taskID := ""
	if env.JobID != "" {
		if id, ok, err := a.resolveJobTask(ctx, env.JobID); err == nil && ok {
			taskID = id
		}
	}
	if taskID == "" {
		t, err := a.openTaskFor(ctx, env)
		if err != nil {
			return "", err
		}
		taskID = t.ID
	}
	if err := a.inbox.SetThread(ctx, env.ID, taskID); err != nil {
		log.Printf("factoryapp: planner: thread %s onto %s: %v", env.ID, taskID, err)
	}
	return taskID, nil
}

// reopenTask reopens a closed task (posting into a closed task reopens
// it, spec §9.5.2) and publishes task.changed.
func (a *App) reopenTask(ctx context.Context, id string) (tasks.Task, error) {
	open := tasks.StatusOpen
	t, err := a.tasks.UpdateTask(ctx, id, tasks.TaskUpdate{Status: &open})
	if err != nil {
		return tasks.Task{}, err
	}
	a.publishTaskChanged(ctx, id)
	return t, nil
}

// taskView builds the I3 Task JSON for one task.
func (a *App) taskView(ctx context.Context, t tasks.Task) taskView {
	return taskView{Task: t, Preview: a.taskPreview(ctx, t.ID)}
}

// taskPreview returns the last operator-visible message of the task's
// thread (chat.message or brain.reply), whitespace-collapsed and cut to
// taskPreviewMax runes; "" when there is none.
func (a *App) taskPreview(ctx context.Context, taskID string) string {
	var payload string
	err := a.inbox.DB().QueryRowContext(ctx,
		`SELECT payload FROM messages WHERE thread_id = ? AND type IN (?, ?)
		 ORDER BY seq DESC LIMIT 1`,
		taskID, tasks.TypeChatMessage, tasks.TypeBrainReply).Scan(&payload)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("factoryapp: tasks: preview %s: %v", taskID, err)
		}
		return ""
	}
	text := strings.Join(strings.FieldsFunc(payloadText(json.RawMessage(payload)), unicode.IsSpace), " ")
	if r := []rune(text); len(r) > taskPreviewMax {
		text = string(r[:taskPreviewMax])
	}
	return text
}

// publishTaskChanged publishes a synthetic, non-persisted task.changed
// envelope (payload = the I3 Task) on the planner:main SSE stream (I5).
// Best effort: a lookup failure only logs.
func (a *App) publishTaskChanged(ctx context.Context, taskID string) {
	if a.hub == nil || taskID == "" {
		return
	}
	t, err := a.tasks.GetTask(ctx, taskID)
	if err != nil {
		log.Printf("factoryapp: tasks: publish %s: %v", taskID, err)
		return
	}
	data, err := json.Marshal(a.taskView(ctx, t))
	if err != nil {
		log.Printf("factoryapp: tasks: publish %s: %v", taskID, err)
		return
	}
	a.hub.Publish(auth.PlannerMainInbox, wire.MessageEnvelope{
		ID:       wire.NewMessageID(),
		Type:     taskChangedType,
		From:     "factory",
		TS:       time.Now().UTC().Format(time.RFC3339),
		ThreadID: taskID,
		Payload:  data,
	})
}

// postToTask posts env into planner:main threaded onto taskID, then
// publishes task.changed. The message itself streams via the inbox hook.
func (a *App) postToTask(ctx context.Context, taskID string, env wire.MessageEnvelope) (wire.MessageEnvelope, error) {
	env.ThreadID = taskID
	posted, err := a.inbox.Post(ctx, auth.PlannerMainInbox, env)
	if err != nil {
		return posted, fmt.Errorf("post to task %s: %w", taskID, err)
	}
	a.publishTaskChanged(ctx, taskID)
	return posted, nil
}

// initTasks wires the S2 task machinery (spec §9.5.2): the per-task wake
// dispatcher (unlimited by default; SetWakeLimiter installs a cap) and the
// idle reaper.
func (a *App) initTasks() {
	a.wakes = &wakeDispatcher{
		limiter: unlimitedWakes{},
		run:     a.runTaskWake,
		fail:    a.plannerFailed,
		persist: a.persistInflight,
	}
	a.reaper = &taskReaper{
		tasks:   a.tasks,
		jobs:    a.jobs,
		busy:    a.wakes.Busy,
		changed: a.publishTaskChanged,
	}
}

// SetWakeLimiter installs the planner wake concurrency limiter (spec
// §9.5.2 seam; nil restores the unlimited v1 default). Call before Run.
func (a *App) SetWakeLimiter(l WakeLimiter) {
	if l == nil {
		l = unlimitedWakes{}
	}
	a.wakes.mu.Lock()
	a.wakes.limiter = l
	a.wakes.mu.Unlock()
}

// Tasks is the integration/test accessor to the task store.
func (a *App) Tasks() *tasks.Store { return a.tasks }
