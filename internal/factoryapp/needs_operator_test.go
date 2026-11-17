package factoryapp_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// taskWithNeeds is taskJSON plus needs_operator.
type taskWithNeeds struct {
	taskJSON
	NeedsOperator bool `json:"needs_operator"`
}

// plannerTool calls one planner MCP tool and fails the test on an error.
func (e *testEnv) plannerTool(name string, args map[string]any) {
	e.t.Helper()
	status, out := e.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if status != http.StatusOK {
		e.t.Fatalf("%s: status %d: %v", name, status, out)
	}
	if res, _ := out["result"].(map[string]any); res == nil || res["isError"] == true {
		e.t.Fatalf("%s: %v", name, out)
	}
}

func (e *testEnv) taskNeeds(id string) (taskWithNeeds, taskWithNeeds) {
	e.t.Helper()
	var one struct {
		Task taskWithNeeds `json:"task"`
	}
	if s := e.do("GET", "/v1/tasks/"+id, "", nil, &one); s != http.StatusOK {
		e.t.Fatalf("GET task = %d", s)
	}
	var list struct {
		Tasks []taskWithNeeds `json:"tasks"`
	}
	if s := e.do("GET", "/v1/tasks?status=all", "", nil, &list); s != http.StatusOK {
		e.t.Fatalf("GET tasks = %d", s)
	}
	for _, t := range list.Tasks {
		if t.ID == id {
			return one.Task, t
		}
	}
	e.t.Fatalf("task %s not in the list", id)
	return one.Task, taskWithNeeds{}
}

// TestOperatorCloseWakesPlanner: POST /v1/tasks/{id}/close posts
// task.closed_by_operator into the task and wakes the planner on it; the
// wake does not reopen the task, and a repeat close posts nothing.
func TestOperatorCloseWakesPlanner(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{}
	e.a.SetPlannerExec(w.exec)
	c := e.chat("deploy the canary", "")
	waitFor(t, "chat wake", 5*time.Second, func() bool { return w.count() == 1 })

	if s := e.do("POST", "/v1/tasks/"+c.TaskID+"/close", "", map[string]string{"reason": "not needed any more"}, nil); s != http.StatusOK {
		t.Fatalf("close = %d", s)
	}
	waitFor(t, "close wake", 5*time.Second, func() bool { return w.count() == 2 })
	tasksWoken, prompts := w.snapshot()
	if tasksWoken[1] != c.TaskID {
		t.Errorf("close woke task %q, want %q", tasksWoken[1], c.TaskID)
	}
	for _, want := range []string{"task.closed_by_operator", "not needed any more", "operator closed this task", "cancel_job"} {
		if !strings.Contains(prompts[1], want) {
			t.Errorf("close wake prompt lacks %q:\n%s", want, prompts[1])
		}
	}

	th := e.thread(c.TaskID, true)
	if th.Task.Status != "closed" || th.Task.CloseReason != "not needed any more" {
		t.Errorf("after the close wake task = %+v, want still closed", th.Task)
	}
	var found bool
	for _, m := range th.Messages {
		if m.Type == "task.closed_by_operator" {
			found = true
			if !m.Internal || !strings.Contains(string(m.Payload), "not needed any more") {
				t.Errorf("close message = %+v", m)
			}
		}
	}
	if !found {
		t.Error("no task.closed_by_operator message in the thread")
	}
	for _, m := range e.thread(c.TaskID, false).Messages {
		if m.Type == "task.closed_by_operator" {
			t.Error("task.closed_by_operator shows in the default (operator) view")
		}
	}

	// Closing a closed task is a no-op: no second wake.
	e.do("POST", "/v1/tasks/"+c.TaskID+"/close", "", nil, nil)
	time.Sleep(300 * time.Millisecond)
	if n := w.count(); n != 2 {
		t.Errorf("repeat close: %d wakes, want 2", n)
	}
}

// TestPlannerCloseDoesNotWakePlanner: update_task(status=closed) from the
// planner posts nothing and wakes nothing (no close/wake loop).
func TestPlannerCloseDoesNotWakePlanner(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{}
	e.a.SetPlannerExec(w.exec)
	c := e.chat("small job", "")
	waitFor(t, "chat wake", 5*time.Second, func() bool { return w.count() == 1 })
	before := len(e.thread(c.TaskID, true).Messages)

	e.plannerTool("update_task", map[string]any{"task_id": c.TaskID, "status": "closed", "reason": "done"})
	time.Sleep(500 * time.Millisecond)
	if n := w.count(); n != 1 {
		t.Errorf("planner close: %d wakes, want 1", n)
	}
	th := e.thread(c.TaskID, true)
	if th.Task.Status != "closed" || len(th.Messages) != before {
		t.Errorf("planner close: task %+v, %d messages (want closed, %d)", th.Task, len(th.Messages), before)
	}
}

// TestNeedsOperatorFlag: the planner sets/clears needs_operator through
// update_task; task JSON and the list carry it; an operator reply or an
// operator close clears it.
func TestNeedsOperatorFlag(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{}
	e.a.SetPlannerExec(w.exec)
	c := e.chat("migrate the db?", "")
	waitFor(t, "chat wake", 5*time.Second, func() bool { return w.count() == 1 })

	check := func(step string, want bool) {
		t.Helper()
		one, listed := e.taskNeeds(c.TaskID)
		if one.NeedsOperator != want || listed.NeedsOperator != want {
			t.Errorf("%s: needs_operator task=%v list=%v, want %v", step, one.NeedsOperator, listed.NeedsOperator, want)
		}
	}
	set := func(v bool) {
		t.Helper()
		e.plannerTool("update_task", map[string]any{"task_id": c.TaskID, "needs_operator": v})
	}
	check("new task", false)
	set(true)
	check("planner set", true)
	set(false)
	check("planner cleared", false)

	set(true)
	e.chat("yes, go ahead", c.TaskID)
	check("operator replied", false)

	set(true)
	if s := e.do("POST", "/v1/tasks/"+c.TaskID+"/close", "", nil, nil); s != http.StatusOK {
		t.Fatalf("close = %d", s)
	}
	check("operator closed", false)
}
