package factoryapp

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// Alert delivery to the planner (spec §9.5.3): a new alert wakes the
// planner in the standing chat as its own message type, alert.raised,
// distinct from the operator's chat.message. Guards:
//   - coalescing: alerts raised within alertWakeWindow ride in ONE
//     alert.raised envelope (one wake, payload.alerts lists them all);
//   - dedupe: a repeat for the same subject (source + task + title)
//     inside alertWakeDedupe is stored and shown as an alert but does not
//     wake the planner again;
//   - no self-loop: alerts the planner raised itself (source "planner"),
//     and the planner-failure / Claude-login alerts (the planner cannot
//     run to read them), never wake it.
const alertRaisedType = "alert.raised"

var (
	// alertWakeWindow is the coalescing delay (a var so tests can shorten
	// it; <= 0 delivers synchronously).
	alertWakeWindow = 3 * time.Second
	// alertWakeDedupe is how long a subject stays suppressed after it woke
	// the planner once.
	alertWakeDedupe = 30 * time.Minute
)

// alertWakeState is the process-local coalescing buffer and dedupe memory.
type alertWakeState struct {
	mu    sync.Mutex
	buf   []tasks.Alert
	timer *time.Timer
	seen  map[string]time.Time
}

// stop cancels a pending coalescing timer (shutdown).
func (s *alertWakeState) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.buf = nil
}

// alertWakeSkipSource reports whether alerts from source never wake the
// planner (self-raised, or raised because the planner cannot run).
func alertWakeSkipSource(source string) bool {
	switch source {
	case "planner", "factory:planner", claudeAuthSource:
		return true
	}
	return false
}

func alertSubject(al tasks.Alert) string {
	return al.Source + "|" + al.TaskID + "|" + strings.ToLower(strings.TrimSpace(al.Title))
}

// alertSeverity maps a priority to the planner-facing severity word.
func alertSeverity(p int) string {
	switch {
	case p <= 1:
		return "critical"
	case p == 2:
		return "warning"
	}
	return "info"
}

var jobIDRe = regexp.MustCompile(`\bjob_[0-9A-Za-z]+`)

// alertJobIDs extracts the job ids an alert mentions.
func alertJobIDs(al tasks.Alert) []string {
	seen := map[string]bool{}
	ids := []string{}
	for _, id := range jobIDRe.FindAllString(al.Title+" "+al.Body, -1) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// queueAlertWake offers a freshly created alert for delivery to the
// planner; the guards above decide whether it actually wakes it.
func (a *App) queueAlertWake(al tasks.Alert) {
	if a.inbox == nil || alertWakeSkipSource(al.Source) {
		return
	}
	now := a.tasks.Now()
	key := alertSubject(al)
	st := &a.alertWake
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.seen == nil {
		st.seen = map[string]time.Time{}
	}
	if at, ok := st.seen[key]; ok && now.Sub(at) < alertWakeDedupe {
		return
	}
	st.seen[key] = now
	for k, at := range st.seen { // prune
		if now.Sub(at) >= alertWakeDedupe {
			delete(st.seen, k)
		}
	}
	st.buf = append(st.buf, al)
	if alertWakeWindow <= 0 {
		a.flushAlertWakeLocked()
		return
	}
	if st.timer == nil {
		st.timer = time.AfterFunc(alertWakeWindow, func() {
			st.mu.Lock()
			defer st.mu.Unlock()
			a.flushAlertWakeLocked()
		})
	}
}

// flushAlertWake delivers whatever is buffered now (tests, shutdown).
func (a *App) flushAlertWake() {
	a.alertWake.mu.Lock()
	defer a.alertWake.mu.Unlock()
	a.flushAlertWakeLocked()
}

func (a *App) flushAlertWakeLocked() {
	st := &a.alertWake
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	batch := st.buf
	st.buf = nil
	if len(batch) == 0 {
		return
	}
	type item struct {
		ID       string   `json:"alert_id"`
		Severity string   `json:"severity"`
		Priority int      `json:"priority"`
		Source   string   `json:"source"`
		TaskID   string   `json:"task_id,omitempty"`
		JobIDs   []string `json:"job_ids"`
		Title    string   `json:"title"`
		Text     string   `json:"text"`
		Created  string   `json:"created_at"`
	}
	items := make([]item, 0, len(batch))
	critical := false
	for _, al := range batch {
		if al.Priority <= 1 {
			critical = true
		}
		items = append(items, item{
			al.ID, alertSeverity(al.Priority), al.Priority, al.Source, al.TaskID,
			alertJobIDs(al), al.Title, al.Body, al.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	data, err := json.Marshal(map[string]any{"count": len(items), "critical": critical, "alerts": items})
	if err != nil {
		return
	}
	if _, err := a.inbox.Post(context.Background(), auth.PlannerMainInbox, wire.MessageEnvelope{
		Type: alertRaisedType, From: "factory", Payload: data, ThreadID: tasks.StandingID,
	}); err != nil {
		log.Printf("factoryapp: alert wake: %v", err)
	}
}

// alertWakePrompt is the triage section of the standing wake prompt when
// alert.raised rows woke the planner.
const alertWakePrompt = `
=== ALERT TRIAGE (alert.raised rows above woke you; you are NOT answering a chat message) ===
Triage first, notify last:
1. Investigate with the read tools (list_alerts, get_task, list_jobs, read_result, list_foremen, merge queue). Do not guess from the alert text.
2. Fix or re-route it yourself where you can (create_job, cancel_job, request_merge, message_foreman, update_task...). Then ack_alert the alerts you handled or that are noise/duplicates.
3. Only then decide whether the operator needs to hear about it. Tell them (notify_operator) when it needs their decision or action, or you could not fix it. Stay silent about alerts you resolved quietly, or fold them into one brief line.
4. Critical alerts (severity critical: P0/P1) and anything blocking the operator ALWAYS reach them via notify_operator, even when you also fixed it (and even on mobile). Leave those alerts open until the operator has been told.
Never raise_alert about these alerts (no loops). If an alert comes from a job or task, link it by id in your message.
`
