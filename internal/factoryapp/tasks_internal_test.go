// Unit tests for the S2 task machinery that need App internals (spec
// §9.5.2): the idle reaper (injectable clock), in-flight wake recovery,
// the per-task wake prompt, titles, and job→task lineage.
package factoryapp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/budgets"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

func newInternalApp(t *testing.T) *App {
	t.Helper()
	a, err := New(Config{
		DataDir:      t.TempDir(),
		Addr:         "127.0.0.1:0",
		AdvertiseURL: "http://factory.test",
		Budget:       budgets.DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestTaskTitle(t *testing.T) {
	long := strings.Repeat("é", 70)
	tests := []struct{ in, want string }{
		{"hello", "hello"},
		{"  multi\n\tline   text  ", "multi line text"},
		{long, strings.Repeat("é", 60)},
		{strings.Repeat("a", 59) + " bcd", strings.Repeat("a", 59)},
		{"   ", ""},
	}
	for _, tc := range tests {
		if got := taskTitle(tc.in); got != tc.want {
			t.Errorf("taskTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEnvelopeTaskTitle(t *testing.T) {
	tests := []struct {
		name string
		env  wire.MessageEnvelope
		want string
	}{
		{"text", wire.MessageEnvelope{Payload: json.RawMessage(`{"text":"deploy finished"}`)}, "deploy finished"},
		{"summary", wire.MessageEnvelope{Payload: json.RawMessage(`{"summary":"s","x":1}`)}, "s"},
		{"error", wire.MessageEnvelope{Payload: json.RawMessage(`{"error":"brain died"}`)}, "brain died"},
		{"json string", wire.MessageEnvelope{Payload: json.RawMessage(`"plain"`)}, "plain"},
		{"raw", wire.MessageEnvelope{Payload: json.RawMessage(`{"n":1}`)}, `{"n":1}`},
		{"stringified object payload", wire.MessageEnvelope{Payload: json.RawMessage(`"{\"text\":\"DEPLOY-18 CLOSED\",\"task_id\":\"task_1\"}"`)}, "DEPLOY-18 CLOSED"},
		{"stringified object without text keeps the string", wire.MessageEnvelope{Payload: json.RawMessage(`"{\"n\":1}"`)}, `{"n":1}`},
		{"string that only looks like an object", wire.MessageEnvelope{Payload: json.RawMessage(`"{not json"`)}, "{not json"},
		{
			"empty payload falls back to type/sender/job",
			wire.MessageEnvelope{Type: "foreman.report", From: "foreman:x", JobID: "job_1", Payload: json.RawMessage(`{}`)},
			"foreman.report from foreman:x (job_1)",
		},
		{
			"null payload, no job",
			wire.MessageEnvelope{Type: "brain.failed", From: "foreman:y", Payload: json.RawMessage(`null`)},
			"brain.failed from foreman:y",
		},
	}
	for _, tc := range tests {
		if got := envelopeTaskTitle(tc.env); got != tc.want {
			t.Errorf("%s: envelopeTaskTitle = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestReaper: an open task idle >= 24h with no live job and no planner
// session closes with close_reason idle-24h; everything else stays.
func TestReaper(t *testing.T) {
	a := newInternalApp(t)
	ctx := context.Background()
	mk := func(title string) tasks.Task {
		t.Helper()
		tk, err := a.tasks.CreateTask(ctx, title, tasks.DefaultPriority)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	idle := mk("idle")
	recent := mk("recent")
	liveJob := mk("live job")
	doneJob := mk("done job")
	busy := mk("busy")
	closed := mk("already closed")
	status := tasks.StatusClosed
	if _, err := a.tasks.UpdateTask(ctx, closed.ID, tasks.TaskUpdate{Status: &status}); err != nil {
		t.Fatal(err)
	}

	// Jobs: one pending (live), one cancelled (terminal); plus an id the
	// jobs store never heard of (counts as terminal).
	live := wire.NewJobSpec("live", "test")
	done := wire.NewJobSpec("done", "test")
	for _, s := range []wire.JobSpec{live, done} {
		if err := a.jobs.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.jobs.Cancel(ctx, done.ID, "cancelled", "test"); err != nil {
		t.Fatal(err)
	}
	for task, job := range map[string]string{liveJob.ID: live.ID, doneJob.ID: done.ID, idle.ID: "job_unknown"} {
		if err := a.tasks.LinkJob(ctx, task, job); err != nil {
			t.Fatal(err)
		}
	}

	var changed []string
	r := &taskReaper{
		tasks:   a.tasks,
		jobs:    a.jobs,
		busy:    func(id string) bool { return id == busy.ID },
		changed: func(_ context.Context, id string) { changed = append(changed, id) },
	}

	// Fake clock 1h ahead: nothing is idle yet.
	base := time.Now()
	r.Now = func() time.Time { return base.Add(time.Hour) }
	if got, err := r.Reap(ctx); err != nil || len(got) != 0 {
		t.Fatalf("Reap at +1h = %v, %v; want nothing", got, err)
	}

	// "recent" gets a thread message strictly after every other task's
	// last activity (message ts has 1s resolution, hence the sleep); the
	// clock is then set so the cutoff falls just before that message.
	time.Sleep(1100 * time.Millisecond)
	if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, wire.MessageEnvelope{
		Type: "chat.message", From: "human:operator", Payload: json.RawMessage(`{"text":"again"}`), ThreadID: recent.ID,
	}); err != nil {
		t.Fatal(err)
	}
	recentTask, _ := a.tasks.GetTask(ctx, recent.ID)
	r.Now = func() time.Time { return recentTask.LastActivityAt.Add(taskIdleClose - time.Millisecond) }
	// At this clock only tasks whose activity is strictly older than the
	// recent one's qualify: all but "recent".
	got, err := r.Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{idle.ID: true, doneJob.ID: true}
	if len(got) != len(want) {
		t.Fatalf("reaped %v, want %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("reaped %s unexpectedly", id)
		}
	}
	if len(changed) != 2 {
		t.Errorf("changed callbacks = %v", changed)
	}
	for id := range want {
		tk, _ := a.tasks.GetTask(ctx, id)
		if tk.Status != tasks.StatusClosed || tk.CloseReason != taskIdleReason {
			t.Errorf("%s: status=%s reason=%q", id, tk.Status, tk.CloseReason)
		}
	}
	for _, id := range []string{recent.ID, liveJob.ID, busy.ID} {
		if tk, _ := a.tasks.GetTask(ctx, id); tk.Status != tasks.StatusOpen {
			t.Errorf("%s (%s) was closed", id, tk.Title)
		}
	}
	if tk, _ := a.tasks.GetTask(ctx, closed.ID); tk.CloseReason == taskIdleReason {
		t.Error("an already-closed task was re-closed by the reaper")
	}

	// Far future: "recent" goes too; the live job and busy task stay.
	r.Now = func() time.Time { return base.Add(72 * time.Hour) }
	got, _ = r.Reap(ctx)
	if len(got) != 1 || got[0] != recent.ID {
		t.Errorf("second pass reaped %v, want [%s]", got, recent.ID)
	}
}

// TestReaperRunTicks: Run reaps on its ticker and stops with ctx.
func TestReaperRunTicks(t *testing.T) {
	a := newInternalApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	tk, err := a.tasks.CreateTask(ctx, "old", 2)
	if err != nil {
		t.Fatal(err)
	}
	r := &taskReaper{
		tasks: a.tasks, jobs: a.jobs, Idle: time.Millisecond,
		Now: func() time.Time { return time.Now().Add(time.Hour) },
	}
	done := make(chan struct{})
	go func() { r.Run(ctx, 5*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := a.tasks.GetTask(context.Background(), tk.ID)
		if got.Status == tasks.StatusClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reaper never closed the task")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

// TestRecoverWakes: wakes persisted as in flight at shutdown re-run on
// the next start, bound to their task; unknown message ids are skipped
// and an all-stale set is cleared.
func TestRecoverWakes(t *testing.T) {
	a := newInternalApp(t)
	ctx := context.Background()
	tk, err := a.tasks.CreateTask(ctx, "interrupted", 2)
	if err != nil {
		t.Fatal(err)
	}
	posted, err := a.inbox.Post(ctx, auth.PlannerMainInbox, wire.MessageEnvelope{
		Type: "chat.message", From: "human:operator", ThreadID: tk.ID,
		Payload: json.RawMessage(`{"text":"are you still there?"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.inbox.Ack(ctx, auth.PlannerMainInbox, []string{posted.ID}); err != nil {
		t.Fatal(err)
	}
	a.persistInflight(map[string][]string{tk.ID: {posted.ID, "msg_gone"}})

	var mu sync.Mutex
	var gotTask, gotPrompt string
	a.SetPlannerExec(func(ctx context.Context, prompt string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		gotTask, gotPrompt = PlannerTaskFromContext(ctx), prompt
		return "ok", nil
	})
	a.recoverWakes(ctx)
	a.wakes.Wait()
	mu.Lock()
	if gotTask != tk.ID || !strings.Contains(gotPrompt, "are you still there?") {
		t.Errorf("recovered wake: task=%q prompt=%q", gotTask, snippet(gotPrompt))
	}
	mu.Unlock()
	if raw, _, _ := a.tasks.GetSetting(ctx, keyPlannerInflight); raw != "{}" {
		t.Errorf("in-flight set after recovery = %q, want {}", raw)
	}

	// All-stale set: cleared without a session.
	a.persistInflight(map[string][]string{tk.ID: {"msg_gone"}})
	a.recoverWakes(ctx)
	if raw, _, _ := a.tasks.GetSetting(ctx, keyPlannerInflight); raw != "{}" {
		t.Errorf("stale in-flight set = %q, want {}", raw)
	}
	// Garbage and absent values are tolerated.
	_ = a.tasks.SetSetting(ctx, keyPlannerInflight, "not json")
	a.recoverWakes(ctx)
}

// TestBuildTaskWakePrompt: header, criteria, thread order, internal/new
// tags, triggers missing from the thread, and the bound-task footer.
func TestBuildTaskWakePrompt(t *testing.T) {
	w := PlannerWake{
		Reason: "planner: new chat from the operator on this task needs a reply",
		Task: tasks.Task{
			ID: "task_X", Title: "Ship it", Status: "open", Priority: 1,
			JobIDs: []string{"job_a", "job_b"},
		},
		Criteria: "security first",
		Thread: []wire.MessageEnvelope{
			{ID: "m1", Type: "chat.message", From: "human:operator", Payload: json.RawMessage(`{"text":"first"}`)},
			{ID: "m2", Type: "notify.job", From: "factory", JobID: "job_a", Payload: json.RawMessage(`{"state":"done"}`)},
			{ID: "m3", Type: "chat.message", From: "human:operator", Payload: json.RawMessage(`{"text":"second"}`)},
		},
		Triggers: []wire.MessageEnvelope{
			{ID: "m3", Type: "chat.message"},
			{ID: "m9", Type: "foreman.report", From: "foreman:s", Payload: json.RawMessage(`{"text":"late"}`)},
		},
	}
	p := buildTaskWakePrompt(w)
	for _, want := range []string{
		"You are the factory planner. planner: new chat",
		"id: task_X", "title: Ship it", "status: open", "priority: 1", "job_ids: job_a, job_b",
		"security first",
		"--- message 2 [internal] ---", "--- message 3 [new] ---",
		"new messages not yet in the thread view", "--- new message 1 [internal] [new] ---", "late",
		"bound to task task_X",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Index(p, "first") > strings.Index(p, "second") {
		t.Error("thread not oldest-first")
	}
	if strings.Contains(p, "--- message 1 [") {
		t.Error("message 1 (visible, not new) carries tags")
	}

	empty := buildTaskWakePrompt(PlannerWake{Reason: "planner: x", Task: tasks.Task{ID: "task_E"}})
	for _, want := range []string{"job_ids: none", "(none set;", "(empty)"} {
		if !strings.Contains(empty, want) {
			t.Errorf("empty prompt missing %q", want)
		}
	}
}

func TestPlannerTaskURLAndDir(t *testing.T) {
	b := &PlannerBrain{MCPURL: "http://127.0.0.1:8080/v1/mcp/factory", WorkDir: "/data/planner/"}
	c := b.forTask("task_1")
	if c.MCPURL != "http://127.0.0.1:8080/v1/mcp/factory?task_id=task_1" {
		t.Errorf("MCPURL = %q", c.MCPURL)
	}
	if c.WorkDir != "/data/planner-tasks/task_1" {
		t.Errorf("WorkDir = %q", c.WorkDir)
	}
	if b.MCPURL != "http://127.0.0.1:8080/v1/mcp/factory" {
		t.Error("forTask mutated the base brain")
	}
	if got := plannerTaskURL("http://h/x?a=1", "task_2"); got != "http://h/x?a=1&task_id=task_2" {
		t.Errorf("plannerTaskURL = %q", got)
	}
	if got := plannerTaskURL("://bad", "task_3"); got != "://bad" {
		t.Errorf("plannerTaskURL(bad) = %q", got)
	}
	if PlannerTaskFromContext(context.Background()) != "" {
		t.Error("PlannerTaskFromContext on a bare ctx")
	}
}

// TestResolveJobTaskLineage: a spawned child resolves through its
// parent's link and is linked itself; unknown and unlinked jobs do not
// resolve.
func TestResolveJobTaskLineage(t *testing.T) {
	a := newInternalApp(t)
	ctx := context.Background()
	tk, _ := a.tasks.CreateTask(ctx, "parent task", 2)
	parent := wire.NewJobSpec("parent", "factory")
	if err := a.jobs.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := a.tasks.LinkJob(ctx, tk.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	child := wire.NewJobSpec("child", "worker")
	child.ParentJobID = parent.ID
	if _, err := a.jobs.RegisterSpawned(ctx, child, "foreman:alpha"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := a.resolveJobTask(ctx, child.ID)
	if err != nil || !ok || got != tk.ID {
		t.Fatalf("resolve child = %q, %v, %v; want %s", got, ok, err, tk.ID)
	}
	ids, _ := a.tasks.JobIDs(ctx, tk.ID)
	if len(ids) != 2 {
		t.Errorf("task jobs = %v, want parent + child", ids)
	}
	orphan := wire.NewJobSpec("orphan", "factory")
	if err := a.jobs.Create(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{orphan.ID, "job_nope", ""} {
		if _, ok, err := a.resolveJobTask(ctx, id); ok || err != nil {
			t.Errorf("resolve %q = ok %v err %v; want unattributed", id, ok, err)
		}
	}
	if a.jobThread(ctx, "") != "" || a.jobThread(ctx, orphan.ID) != "" || a.jobThread(ctx, parent.ID) != tk.ID {
		t.Error("jobThread mismatch")
	}
}

// TestRecoverTerminalJobWake: an in-flight notify.job wake interrupted by
// a restart is re-run on its task with the job reason.
func TestRecoverTerminalJobWake(t *testing.T) {
	a := newInternalApp(t)
	ctx := context.Background()
	tk, err := a.tasks.CreateTask(ctx, "job done", 2)
	if err != nil {
		t.Fatal(err)
	}
	posted, err := a.inbox.Post(ctx, auth.PlannerMainInbox, wire.MessageEnvelope{
		Type: "notify.job", From: "factory", JobID: "job_x", ThreadID: tk.ID,
		Payload: json.RawMessage(`{"job_id":"job_x","state":"done"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.inbox.Ack(ctx, auth.PlannerMainInbox, []string{posted.ID}); err != nil {
		t.Fatal(err)
	}
	a.persistInflight(map[string][]string{tk.ID: {posted.ID}})
	var mu sync.Mutex
	var gotTask, gotPrompt string
	a.SetPlannerExec(func(ctx context.Context, prompt string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		gotTask, gotPrompt = PlannerTaskFromContext(ctx), prompt
		return "ok", nil
	})
	a.recoverWakes(ctx)
	a.wakes.Wait()
	mu.Lock()
	defer mu.Unlock()
	if gotTask != tk.ID || !strings.Contains(gotPrompt, "reached a terminal state") {
		t.Errorf("recovered job wake: task=%q prompt=%q", gotTask, snippet(gotPrompt))
	}
}
