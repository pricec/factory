package factoryapp_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const jobWakeReason = "reached a terminal state"

// finishJob drives a paired job to done through the foreman wire path.
func (e *testEnv) finishJob(bearer, jobID string) {
	e.t.Helper()
	post := func(typ string, payload any) {
		if status := e.do("POST", "/v1/inboxes/factory:main/messages", bearer, map[string]any{
			"type": typ, "job_id": jobID, "from": "foreman:alpha", "payload": payload,
		}, nil); status != http.StatusCreated {
			e.t.Fatalf("post %s = %d", typ, status)
		}
	}
	post("job.event", map[string]any{"event": "started"})
	post("job.result", map[string]any{
		"job_id": jobID, "status": "done", "summary": "built",
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "cost_usd": 0.001},
	})
}

func (e *testEnv) createJob(taskID string) string {
	e.t.Helper()
	var created struct {
		JobID string `json:"job_id"`
	}
	body := map[string]any{"task": "work"}
	if taskID != "" {
		body["task_id"] = taskID
	}
	if status := e.do("POST", "/v1/jobs", "", body, &created); status != http.StatusCreated {
		e.t.Fatalf("create job = %d", status)
	}
	return created.JobID
}

// TestTerminalJobWakes: a terminal notify.job threaded to a task wakes the
// planner there with the job reason; an unthreaded one does not.
func TestTerminalJobWakes(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{}
	e.a.SetPlannerExec(w.exec)
	bearer := e.pair("alpha", nil)

	c := e.chat("build", "")
	waitFor(t, "chat wake", 5*time.Second, func() bool { return w.count() == 1 })
	e.finishJob(bearer, e.createJob(c.TaskID))
	waitFor(t, "job wake", 5*time.Second, func() bool { return w.count() == 2 })
	tasksSeen, prompts := w.snapshot()
	if tasksSeen[1] != c.TaskID || !strings.Contains(prompts[1], jobWakeReason) || strings.Contains(prompts[1], "needs a reply") {
		t.Errorf("job wake: task %s prompt %q", tasksSeen[1], snippetOf(prompts[1], 300))
	}

	// Unthreaded (job not linked to any task): informational, no new task.
	e.finishJob(bearer, e.createJob(""))
	time.Sleep(400 * time.Millisecond)
	if n := w.count(); n != 2 {
		t.Errorf("unthreaded notify.job woke the planner: %d sessions", n)
	}
}

// TestChatAndJobCoalesce: a chat and a terminal notify.job on the same
// task, in one batch, run ONE session whose reason covers both.
func TestChatAndJobCoalesce(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{block: make(chan struct{})}
	e.a.SetPlannerExec(w.exec)
	bearer := e.pair("alpha", nil)

	c := e.chat("first", "")
	waitFor(t, "first session", 5*time.Second, func() bool { return w.count() == 1 })
	e.chat("second", c.TaskID)
	e.finishJob(bearer, e.createJob(c.TaskID))
	time.Sleep(400 * time.Millisecond)
	close(w.block)
	waitFor(t, "follow-up", 5*time.Second, func() bool { return w.count() == 2 })
	time.Sleep(400 * time.Millisecond)
	_, prompts := w.snapshot()
	if len(prompts) != 2 || !strings.Contains(prompts[1], "needs a reply") || !strings.Contains(prompts[1], jobWakeReason) {
		t.Errorf("sessions=%d; coalesced reason must cover chat and job: %q", len(prompts), snippetOf(prompts[len(prompts)-1], 400))
	}
}

// TestSelfCancelDoesNotWake: cancel_job through the planner MCP surface
// posts a tagged notify.job that does not wake; the same cancel through
// the HTTP API (operator) does.
func TestSelfCancelDoesNotWake(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{}
	e.a.SetPlannerExec(w.exec)

	c := e.chat("cancel things", "")
	waitFor(t, "chat wake", 5*time.Second, func() bool { return w.count() == 1 })

	self := e.createJob(c.TaskID)
	_, out := e.rpc("tools/call", map[string]any{"name": "cancel_job", "arguments": map[string]any{"job_id": self, "reason": "dup"}})
	if r, _ := out["result"].(map[string]any); r["isError"] == true {
		t.Fatalf("cancel_job failed: %v", out)
	}
	waitFor(t, "tagged notify.job", 5*time.Second, func() bool {
		for _, m := range e.thread(c.TaskID, true).Messages {
			if m.Type == "notify.job" && m.JobID == self && strings.Contains(string(m.Payload), `"self_initiated":true`) {
				return true
			}
		}
		return false
	})
	time.Sleep(400 * time.Millisecond)
	if n := w.count(); n != 1 {
		t.Fatalf("self-cancel woke the planner: %d sessions", n)
	}

	other := e.createJob(c.TaskID)
	if status := e.do("POST", "/v1/jobs/"+other+"/cancel", "", map[string]any{"reason": "operator"}, nil); status != http.StatusOK {
		t.Fatalf("cancel = %d", status)
	}
	waitFor(t, "operator cancel wakes", 5*time.Second, func() bool { return w.count() == 2 })
}
