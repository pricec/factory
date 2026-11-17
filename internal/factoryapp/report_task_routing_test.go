package factoryapp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// reportRouting posts a foreman.report and returns the relayed row's
// thread id once it reaches planner:main (spec §9.5.2 resolution order).
func (e *testEnv) reportRouting(bearer, jobID, payload string) string {
	e.t.Helper()
	before := len(operatorEnvelopes(e.t, e, "foreman.report"))
	if st := e.postToFactoryMain(bearer, wire.MessageEnvelope{
		Type: "foreman.report", From: "foreman:alpha", JobID: jobID, Payload: []byte(payload),
	}); st >= 300 {
		e.t.Fatalf("post status = %d", st)
	}
	waitFor(e.t, "relay", 5*time.Second, func() bool {
		return len(operatorEnvelopes(e.t, e, "foreman.report")) == before+1
	})
	envs := operatorEnvelopes(e.t, e, "foreman.report")
	// Recent returns oldest-first; the newest relay is last.
	return envs[len(envs)-1].ThreadID
}

func (e *testEnv) taskCount() int {
	e.t.Helper()
	ts, err := e.a.Tasks().ListTasks(e.ctx, tasks.TaskFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(ts)
}

func (e *testEnv) unattributedAlerts() int {
	n := 0
	for _, al := range e.listAlerts() {
		if strings.HasPrefix(al.Title, "Unattributed ") {
			n++
		}
	}
	return n
}

func TestReportPayloadTaskIDThreads(t *testing.T) {
	for name, payload := range map[string]string{
		"structured":     `{"text":"r","task_id":"%s"}`,
		"double-encoded": `"{\"text\":\"r\",\"task_id\":\"%s\"}"`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			bearer := e.pair("alpha", nil)
			task, err := e.a.Tasks().CreateTask(e.ctx, "existing", 2)
			if err != nil {
				t.Fatal(err)
			}
			n := e.taskCount()
			got := e.reportRouting(bearer, "", strings.Replace(payload, "%s", task.ID, 1))
			if got != task.ID {
				t.Fatalf("thread = %q, want %q", got, task.ID)
			}
			if e.taskCount() != n {
				t.Errorf("task count changed: a new task was opened")
			}
			if c := e.unattributedAlerts(); c != 0 {
				t.Errorf("unattributed alerts = %d, want 0", c)
			}
		})
	}
}

func TestReportUnknownOrClosedTaskIDFallsBack(t *testing.T) {
	e := newEnv(t)
	bearer := e.pair("alpha", nil)
	closedTask, err := e.a.Tasks().CreateTask(e.ctx, "done", 2)
	if err != nil {
		t.Fatal(err)
	}
	closed, reason := tasks.StatusClosed, "done"
	if _, err := e.a.Tasks().UpdateTask(e.ctx, closedTask.ID, tasks.TaskUpdate{Status: &closed, CloseReason: &reason}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"task_nope", closedTask.ID} {
		n := e.taskCount()
		got := e.reportRouting(bearer, "", `{"text":"orphan `+id+`","task_id":"`+id+`"}`)
		if got == "" || got == id {
			t.Fatalf("%s: thread = %q, want a NEW task", id, got)
		}
		if e.taskCount() != n+1 {
			t.Errorf("%s: want exactly one new task", id)
		}
		if c := e.unattributedAlerts(); c != i+1 {
			t.Errorf("%s: unattributed alerts = %d, want %d", id, c, i+1)
		}
		if st, _ := e.a.Tasks().GetTask(e.ctx, closedTask.ID); st.Status != tasks.StatusClosed {
			t.Errorf("closed task was reopened")
		}
	}
}

func TestReportJobTaskWinsOverPayloadTaskID(t *testing.T) {
	e := newEnv(t)
	bearer := e.pair("alpha", nil)
	jobTask, _ := e.a.Tasks().CreateTask(e.ctx, "job task", 2)
	other, _ := e.a.Tasks().CreateTask(e.ctx, "other", 2)
	if err := e.a.Tasks().LinkJob(e.ctx, jobTask.ID, "job_conflict"); err != nil {
		t.Fatal(err)
	}
	got := e.reportRouting(bearer, "job_conflict", `{"text":"r","task_id":"`+other.ID+`"}`)
	if got != jobTask.ID {
		t.Fatalf("thread = %q, want job task %q", got, jobTask.ID)
	}
	if c := e.unattributedAlerts(); c != 0 {
		t.Errorf("unattributed alerts = %d", c)
	}
}
