package mcpfactory_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/mcpfactory"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

func (e *env) mkTask(title string) tasks.Task {
	e.t.Helper()
	tk, err := e.tasks.CreateTask(context.Background(), title, tasks.DefaultPriority)
	if err != nil {
		e.t.Fatalf("CreateTask: %v", err)
	}
	return tk
}

func (e *env) changedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.changed...)
}

// TestNotifyOperatorTask: notify_operator files the brain.reply under
// task_id, else the woken task (?task_id= on the session URL), else a new
// task titled from the text; unknown tasks are tool errors.
func TestNotifyOperatorTask(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	woken := e.mkTask("woken")
	other := e.mkTask("other")

	lastReply := func() wire.MessageEnvelope {
		t.Helper()
		envs, err := e.svc.Recent(ctx, auth.PlannerMainInbox, 1)
		if err != nil || len(envs) != 1 {
			t.Fatalf("Recent: %v %v", envs, err)
		}
		return envs[0]
	}

	// Woken task default.
	e.wokenTask = woken.ID
	sc := structured(t, resultOf(t, e.callTool("notify_operator", map[string]any{"text": "on it"})))
	if sc["task_id"] != woken.ID || lastReply().ThreadID != woken.ID {
		t.Errorf("woken default: %v / %+v", sc, lastReply())
	}
	// Explicit task_id wins over the woken task.
	sc = structured(t, resultOf(t, e.callTool("notify_operator", map[string]any{"text": "fyi", "task_id": other.ID})))
	if sc["task_id"] != other.ID || lastReply().ThreadID != other.ID {
		t.Errorf("explicit: %v", sc)
	}
	// Unknown explicit / unknown woken: tool errors, nothing posted.
	before := lastReply().ID
	if msg := toolError(t, "notify_operator", e.callTool("notify_operator", map[string]any{"text": "x", "task_id": "task_nope"})); !strings.Contains(msg, "unknown task") {
		t.Errorf("unknown task error = %q", msg)
	}
	e.wokenTask = "task_ghost"
	if msg := toolError(t, "notify_operator", e.callTool("notify_operator", map[string]any{"text": "x"})); !strings.Contains(msg, "task_ghost") {
		t.Errorf("unknown woken error = %q", msg)
	}
	if lastReply().ID != before {
		t.Error("a failed notify_operator posted anyway")
	}
	// No task at all: a new task titled from the text.
	e.wokenTask = ""
	sc = structured(t, resultOf(t, e.callTool("notify_operator", map[string]any{"text": "  unsolicited\nheads-up about the nightly backups  "})))
	newID, _ := sc["task_id"].(string)
	if newID == "" || newID == woken.ID || newID == other.ID {
		t.Fatalf("new task id = %v", sc["task_id"])
	}
	tk, err := e.tasks.GetTask(ctx, newID)
	if err != nil || tk.Title != "unsolicited heads-up about the nightly backups" {
		t.Errorf("new task = %+v (%v)", tk, err)
	}
	if got := e.changedIDs(); strings.Join(got, ",") != strings.Join([]string{woken.ID, other.ID, newID}, ",") {
		t.Errorf("TaskChanged = %v", got)
	}
}

// TestWokenTaskHeaderDirect: the header itself also binds the session.
func TestWokenTaskHeaderDirect(t *testing.T) {
	e := newEnv(t)
	tk := e.mkTask("hdr")
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"notify_operator","arguments":{"text":"hi"}}}`
	req, _ := http.NewRequest("POST", e.base, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+e.planner)
	req.Header.Set(mcpfactory.WokenTaskHeader, tk.ID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if sc := structured(t, resultOf(t, out)); sc["task_id"] != tk.ID {
		t.Errorf("header-bound notify = %v", sc)
	}
}

// TestCreateJobTask: create_job links the job to task_id / the woken task
// (before dispatch) and reports it; unknown tasks refuse the create.
func TestCreateJobTask(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	woken := e.mkTask("woken")
	other := e.mkTask("other")

	// No task context: no link, no task_id in the result.
	sc := structured(t, resultOf(t, e.callTool("create_job", map[string]any{"task": "plain"})))
	if _, has := sc["task_id"]; has {
		t.Errorf("unbound create_job reported task_id: %v", sc)
	}

	e.wokenTask = woken.ID
	sc = structured(t, resultOf(t, e.callTool("create_job", map[string]any{"task": "default"})))
	if sc["task_id"] != woken.ID {
		t.Errorf("woken create_job = %v", sc)
	}
	if id, ok, _ := e.tasks.TaskForJob(ctx, sc["job_id"].(string)); !ok || id != woken.ID {
		t.Errorf("job not linked to the woken task: %q %v", id, ok)
	}
	sc = structured(t, resultOf(t, e.callTool("create_job", map[string]any{"task": "explicit", "task_id": other.ID})))
	if id, ok, _ := e.tasks.TaskForJob(ctx, sc["job_id"].(string)); !ok || id != other.ID || sc["task_id"] != other.ID {
		t.Errorf("explicit create_job = %v (linked %q)", sc, id)
	}

	jobsBefore, _ := e.store.List(ctx, "", "", 100)
	if msg := toolError(t, "create_job", e.callTool("create_job", map[string]any{"task": "x", "task_id": "task_nope"})); !strings.Contains(msg, "unknown task") {
		t.Errorf("unknown task error = %q", msg)
	}
	if jobsAfter, _ := e.store.List(ctx, "", "", 100); len(jobsAfter) != len(jobsBefore) {
		t.Error("create_job with an unknown task still created a job")
	}
}

// TestTaskTools covers list_tasks, get_task, update_task and
// set_priority_criteria.
func TestTaskTools(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.mkTask("alpha deploy")
	b := e.mkTask("bravo audit")
	c := e.mkTask("charlie cleanup")

	// update_task: retitle + reprioritise.
	sc := structured(t, resultOf(t, e.callTool("update_task", map[string]any{"task_id": b.ID, "title": " Bravo audit v2 ", "priority": 0})))
	task := sc["task"].(map[string]any)
	if task["title"] != "Bravo audit v2" || task["priority"] != float64(0) {
		t.Errorf("update_task = %v", task)
	}
	// Close with the default reason, then with an explicit one.
	sc = structured(t, resultOf(t, e.callTool("update_task", map[string]any{"task_id": c.ID, "status": "closed"})))
	if task := sc["task"].(map[string]any); task["status"] != "closed" || task["close_reason"] != "planner" {
		t.Errorf("close default reason = %v", task)
	}
	sc = structured(t, resultOf(t, e.callTool("update_task", map[string]any{"task_id": c.ID, "reason": "superseded by alpha"})))
	if task := sc["task"].(map[string]any); task["close_reason"] != "superseded by alpha" {
		t.Errorf("reason-only update = %v", task)
	}
	// Reopen ignores reason and clears close_reason.
	sc = structured(t, resultOf(t, e.callTool("update_task", map[string]any{"task_id": c.ID, "status": "open", "reason": "ignored"})))
	if task := sc["task"].(map[string]any); task["status"] != "open" || task["close_reason"] != nil {
		t.Errorf("reopen = %v", task)
	}
	_ = structured(t, resultOf(t, e.callTool("update_task", map[string]any{"task_id": c.ID, "status": "closed", "reason": "done"})))

	// needs_operator: set, kept by an unrelated update, cleared.
	for _, step := range []struct {
		args map[string]any
		want bool
	}{
		{map[string]any{"task_id": a.ID, "needs_operator": true}, true},
		{map[string]any{"task_id": a.ID, "title": "alpha deploy"}, true},
		{map[string]any{"task_id": a.ID, "needs_operator": false}, false},
	} {
		sc := structured(t, resultOf(t, e.callTool("update_task", step.args)))
		if got := sc["task"].(map[string]any)["needs_operator"]; got != step.want {
			t.Errorf("update_task %v: needs_operator = %v, want %v", step.args, got, step.want)
		}
	}

	for name, args := range map[string]map[string]any{
		"missing id":     {"title": "x"},
		"unknown id":     {"task_id": "task_nope", "title": "x"},
		"empty title":    {"task_id": a.ID, "title": "  "},
		"negative prio":  {"task_id": a.ID, "priority": -1},
		"bad status":     {"task_id": a.ID, "status": "archived"},
		"unknown arg":    {"task_id": a.ID, "colour": "red"},
		"needs not bool": {"task_id": a.ID, "needs_operator": "yes"},
	} {
		out := e.callTool("update_task", args)
		if res, _ := out["result"].(map[string]any); res != nil && res["isError"] != true {
			t.Errorf("update_task %s: want an error, got %v", name, out)
		}
	}

	// list_tasks: default open, ranked (priority 0 first).
	sl := structuredSlice(t, resultOf(t, e.callTool("list_tasks", map[string]any{})), "tasks")
	if len(sl) != 2 || sl[0]["id"] != b.ID || sl[1]["id"] != a.ID {
		t.Errorf("list_tasks default = %v", sl)
	}
	if sl := structuredSlice(t, resultOf(t, e.callTool("list_tasks", map[string]any{"status": "all"})), "tasks"); len(sl) != 3 {
		t.Errorf("list_tasks all = %d", len(sl))
	}
	if sl := structuredSlice(t, resultOf(t, e.callTool("list_tasks", map[string]any{"status": "closed"})), "tasks"); len(sl) != 1 || sl[0]["id"] != c.ID {
		t.Errorf("list_tasks closed = %v", sl)
	}
	if sl := structuredSlice(t, resultOf(t, e.callTool("list_tasks", map[string]any{"status": "all", "q": "alpha"})), "tasks"); len(sl) != 1 || sl[0]["id"] != a.ID {
		t.Errorf("list_tasks q = %v", sl)
	}
	if sl := structuredSlice(t, resultOf(t, e.callTool("list_tasks", map[string]any{"status": "all", "limit": 0})), "tasks"); len(sl) != 1 {
		t.Errorf("list_tasks limit 0 (clamped to 1) = %d", len(sl))
	}
	if sl := structuredSlice(t, resultOf(t, e.callTool("list_tasks", map[string]any{"q": "zzz-none"})), "tasks"); len(sl) != 0 {
		t.Errorf("list_tasks no match = %v", sl)
	}
	if msg := toolError(t, "list_tasks", e.callTool("list_tasks", map[string]any{"status": "weird"})); !strings.Contains(msg, "status") {
		t.Errorf("list_tasks bad status = %q", msg)
	}

	// get_task: thread with internal flags, 2KB-capped payloads.
	big := strings.Repeat("x", 3000)
	for _, env := range []wire.MessageEnvelope{
		{Type: "chat.message", From: "human:operator", Payload: json.RawMessage(`{"text":"ship alpha"}`)},
		{Type: "foreman.report", From: "foreman:alpha", JobID: "job_1", Payload: json.RawMessage(`{"text":"` + big + `"}`)},
	} {
		env.ThreadID = a.ID
		if _, err := e.svc.Post(ctx, auth.PlannerMainInbox, env); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.tasks.LinkJob(ctx, a.ID, "job_1"); err != nil {
		t.Fatal(err)
	}
	sc = structured(t, resultOf(t, e.callTool("get_task", map[string]any{"task_id": a.ID})))
	if task := sc["task"].(map[string]any); task["id"] != a.ID || len(task["job_ids"].([]any)) != 1 {
		t.Errorf("get_task task = %v", task)
	}
	msgs := sc["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("get_task messages = %v", msgs)
	}
	m0, m1 := msgs[0].(map[string]any), msgs[1].(map[string]any)
	if m0["internal"] != false || m0["payload"] != `{"text":"ship alpha"}` {
		t.Errorf("message 0 = %v", m0)
	}
	if p := m1["payload"].(string); m1["internal"] != true || m1["job_id"] != "job_1" || len(p) > 2200 || !strings.Contains(p, "truncated") {
		t.Errorf("message 1 internal=%v job=%v len=%d", m1["internal"], m1["job_id"], len(p))
	}
	if msg := toolError(t, "get_task", e.callTool("get_task", map[string]any{"task_id": "task_nope"})); !strings.Contains(msg, "unknown task") {
		t.Errorf("get_task unknown = %q", msg)
	}
	if msg := toolError(t, "get_task", e.callTool("get_task", map[string]any{"task_id": ""})); !strings.Contains(msg, "required") {
		t.Errorf("get_task empty = %q", msg)
	}

	// set_priority_criteria round trip.
	_ = structured(t, resultOf(t, e.callTool("set_priority_criteria", map[string]any{"text": "outages first"})))
	if got, _ := e.tasks.PriorityCriteria(ctx); got != "outages first" {
		t.Errorf("criteria = %q", got)
	}

	// Every update_task success (5 + 3 needs_operator steps) fired TaskChanged.
	if n := len(e.changedIDs()); n != 8 {
		t.Errorf("TaskChanged calls = %d, want 8", n)
	}
}

// TestNoTaskStore: without Config.Tasks the task tools are absent and
// notify_operator posts unthreaded (pre-task behaviour).
func TestNoTaskStore(t *testing.T) {
	e := newEnv(t)
	h, err := mcpfactory.Handler(mcpfactory.Config{Auth: e.authStore, Store: e.store, Inbox: e.svc})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	bare := &env{t: t, svc: e.svc, base: srv.URL, planner: e.planner, wokenTask: "task_ignored"}
	_, out := bare.rpc(bare.planner, "tools/list", nil)
	if tools := resultOf(t, out)["tools"].([]any); len(tools) != 9 { // 7 core + raise_alert (inbox S3) + request_merge (§6.11), always registered
		t.Errorf("tools without a task store = %d, want 9", len(tools))
	}
	sc := structured(t, resultOf(t, bare.callTool("notify_operator", map[string]any{"text": "hi"})))
	if _, has := sc["task_id"]; has {
		t.Errorf("unthreaded notify reported task_id: %v", sc)
	}
	envs, _ := e.svc.Recent(context.Background(), auth.PlannerMainInbox, 1)
	if len(envs) != 1 || envs[0].ThreadID != "" {
		t.Errorf("reply = %+v, want unthreaded", envs)
	}
	sc = structured(t, resultOf(t, bare.callTool("create_job", map[string]any{"task": "x", "task_id": "task_whatever"})))
	if _, has := sc["task_id"]; has {
		t.Errorf("create_job without a task store reported task_id: %v", sc)
	}
}
