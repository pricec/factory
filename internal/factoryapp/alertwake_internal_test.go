package factoryapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/mcpfactory"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

func alertWakeRows(t *testing.T, a *App) []wire.MessageEnvelope {
	t.Helper()
	rows, err := a.inbox.ThreadMessages(context.Background(), tasks.StandingID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []wire.MessageEnvelope
	for _, r := range rows {
		if r.Type == alertRaisedType {
			out = append(out, r)
		}
	}
	return out
}

func setWindow(t *testing.T, w time.Duration) {
	t.Helper()
	old := alertWakeWindow
	alertWakeWindow = w
	t.Cleanup(func() { alertWakeWindow = old })
}

func newWakeApp(t *testing.T) (*App, *gateClock) {
	t.Helper()
	clk := &gateClock{t: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	t.Cleanup(func() { _ = a.Close() })
	return a, clk
}

type wakeItem struct {
	AlertID  string   `json:"alert_id"`
	Severity string   `json:"severity"`
	Source   string   `json:"source"`
	TaskID   string   `json:"task_id"`
	JobIDs   []string `json:"job_ids"`
	Title    string   `json:"title"`
	Text     string   `json:"text"`
}

func wakeItems(t *testing.T, env wire.MessageEnvelope) []wakeItem {
	t.Helper()
	var p struct {
		Count  int        `json:"count"`
		Alerts []wakeItem `json:"alerts"`
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Count != len(p.Alerts) {
		t.Fatalf("count %d != %d alerts", p.Count, len(p.Alerts))
	}
	return p.Alerts
}

// TestAlertWakeDelivery: a system alert becomes one alert.raised row in the
// standing chat carrying id, severity, source, task and job ids and text,
// and it wakes the planner (wakesPlanner) as a type distinct from chat.
func TestAlertWakeDelivery(t *testing.T) {
	setWindow(t, 0)
	a, _ := newWakeApp(t)
	ctx := context.Background()
	task, _ := a.tasks.CreateTask(ctx, "t", 1)
	a.raiseSystemAlertForTask(ctx, 1, "Job job_ABC123 failed", "see job_ABC123 and job_XYZ9", "factory:test", task.ID)

	rows := alertWakeRows(t, a)
	if len(rows) != 1 {
		t.Fatalf("alert.raised rows = %d, want 1", len(rows))
	}
	if rows[0].Type == "chat.message" || !wakesPlanner(rows[0]) || rows[0].ThreadID != tasks.StandingID {
		t.Fatalf("bad envelope: %+v", rows[0])
	}
	items := wakeItems(t, rows[0])
	it := items[0]
	open, _ := a.tasks.ListAlerts(ctx, tasks.AlertFilter{States: []string{tasks.AlertOpen}})
	if len(open) != 1 || it.AlertID != open[0].ID {
		t.Fatalf("alert id mismatch: %+v vs %+v", it, open)
	}
	if it.Severity != "critical" || it.Source != "factory:test" || it.TaskID != task.ID ||
		strings.Join(it.JobIDs, ",") != "job_ABC123,job_XYZ9" || !strings.Contains(it.Text, "job_XYZ9") {
		t.Fatalf("item = %+v", it)
	}
}

func TestAlertSeverity(t *testing.T) {
	for p, want := range map[int]string{0: "critical", 1: "critical", 2: "warning", 3: "info"} {
		if got := alertSeverity(p); got != want {
			t.Errorf("P%d = %q, want %q", p, got, want)
		}
	}
}

// TestAlertWakeCoalesce: a burst inside the window becomes ONE wake.
func TestAlertWakeCoalesce(t *testing.T) {
	setWindow(t, time.Hour)
	a, _ := newWakeApp(t)
	ctx := context.Background()
	for _, title := range []string{"a", "b", "c"} {
		a.raiseSystemAlert(ctx, 2, "burst "+title, "", "factory:test")
	}
	if n := len(alertWakeRows(t, a)); n != 0 {
		t.Fatalf("delivered before window: %d", n)
	}
	a.flushAlertWake()
	rows := alertWakeRows(t, a)
	if len(rows) != 1 || len(wakeItems(t, rows[0])) != 3 {
		t.Fatalf("rows = %d, want 1 carrying 3 alerts", len(rows))
	}
	a.flushAlertWake() // idempotent
	if n := len(alertWakeRows(t, a)); n != 1 {
		t.Fatalf("second flush re-delivered: %d", n)
	}
}

func TestAlertWakeTimerFlush(t *testing.T) {
	setWindow(t, 20*time.Millisecond)
	a, _ := newWakeApp(t)
	a.raiseSystemAlert(context.Background(), 2, "timer", "", "factory:test")
	deadline := time.Now().Add(3 * time.Second)
	for len(alertWakeRows(t, a)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timer never delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAlertWakeDedupe: a repeat for the same subject is still an alert but
// does not wake again until the dedupe window passes; other subjects wake.
func TestAlertWakeDedupe(t *testing.T) {
	setWindow(t, 0)
	a, clk := newWakeApp(t)
	ctx := context.Background()
	a.raiseSystemAlert(ctx, 2, "Disk full", "1", "factory:test")
	a.raiseSystemAlert(ctx, 2, "disk full ", "2", "factory:test") // same subject
	if n := len(alertWakeRows(t, a)); n != 1 {
		t.Fatalf("repeat woke: rows = %d", n)
	}
	a.raiseSystemAlert(ctx, 2, "Disk full", "3", "factory:other") // other source
	if n := len(alertWakeRows(t, a)); n != 2 {
		t.Fatalf("other subject: rows = %d, want 2", n)
	}
	if open, _ := a.tasks.ListAlerts(ctx, tasks.AlertFilter{States: []string{tasks.AlertOpen}}); len(open) != 3 {
		t.Fatalf("repeat alert must still be stored: %d", len(open))
	}
	clk.Add(alertWakeDedupe + time.Minute)
	a.raiseSystemAlert(ctx, 2, "Disk full", "4", "factory:test")
	if n := len(alertWakeRows(t, a)); n != 3 {
		t.Fatalf("after window: rows = %d, want 3", n)
	}
}

// TestAlertWakeNoSelfLoop: the planner's own alerts, and alerts raised
// because the planner cannot run, never wake it.
func TestAlertWakeNoSelfLoop(t *testing.T) {
	setWindow(t, 0)
	a, _ := newWakeApp(t)
	ctx := context.Background()
	if _, err := a.RaisePlannerAlert(ctx, mcpfactory.RaiseAlertInput{Priority: 0, Title: "from planner"}); err != nil {
		t.Fatal(err)
	}
	a.plannerFailed(ctx, tasks.StandingID, context.DeadlineExceeded)
	a.raiseSystemAlert(ctx, 0, "login", "", claudeAuthSource)
	if n := len(alertWakeRows(t, a)); n != 0 {
		t.Fatalf("self/unrunnable alerts woke the planner: %d", n)
	}
	if open, _ := a.tasks.ListAlerts(ctx, tasks.AlertFilter{States: []string{tasks.AlertOpen}}); len(open) != 3 {
		t.Fatalf("alerts must still be stored: %d", len(open))
	}
}

// TestAlertWakePrompt: alert wakes carry the triage-first prompt and an
// alert-specific reason; chat wakes are unchanged. Mobile hint still shows.
func TestAlertWakePrompt(t *testing.T) {
	alert := wire.MessageEnvelope{ID: "m1", Type: alertRaisedType, From: "factory", Payload: json.RawMessage(`{"count":1}`)}
	chat := wire.MessageEnvelope{ID: "m2", Type: "chat.message", From: "operator", Payload: json.RawMessage(`{"text":"hi","client":"mobile"}`)}
	task := tasks.Task{ID: tasks.StandingID, Standing: true}
	p := buildStandingWakePrompt(PlannerWake{Task: task, Reason: reasonStandingAlert, Triggers: []wire.MessageEnvelope{alert}})
	for _, want := range []string{"ALERT TRIAGE", "Investigate with the read tools", "ack_alert", "ALWAYS reach them"} {
		if !strings.Contains(p, want) {
			t.Errorf("alert prompt missing %q", want)
		}
	}
	p = buildStandingWakePrompt(PlannerWake{Task: task, Reason: reasonStanding, Triggers: []wire.MessageEnvelope{chat}})
	if strings.Contains(p, "ALERT TRIAGE") {
		t.Error("chat-only wake carries triage section")
	}
	if got := wakeReason([]wire.MessageEnvelope{alert}); !strings.Contains(got, "alert") {
		t.Errorf("wakeReason = %q", got)
	}
}
