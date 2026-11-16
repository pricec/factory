package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/factoryapp"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// Inbox redesign S5: the four live-acceptance steps (a-d) plus the three
// glue behaviors (planner failure -> P0 alert, unattributed report -> P2
// alert, alert reply wakes the planner through thread_id), over the real
// factory + foreman stack. Spec §9.5.4.

type e2eTask struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type e2eMsg struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	From     string          `json:"from"`
	JobID    string          `json:"job_id"`
	Internal bool            `json:"internal"`
	Payload  json.RawMessage `json:"payload"`
}

type e2eAlert struct {
	ID        string  `json:"id"`
	Priority  int     `json:"priority"`
	Title     string  `json:"title"`
	Source    string  `json:"source"`
	State     string  `json:"state"`
	TaskID    *string `json:"task_id"`
	ExpiresAt *string `json:"expires_at"`
}

// plannerStub is the scripted planner session runner: it records every
// (task, prompt) it was woken with and delegates to fn.
type plannerStub struct {
	mu    sync.Mutex
	fn    func(ctx context.Context, task, prompt string) (string, error)
	calls []stubCall
}

type stubCall struct{ task, prompt string }

func (p *plannerStub) set(fn func(ctx context.Context, task, prompt string) (string, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fn = fn
}

func (p *plannerStub) run(ctx context.Context, prompt string) (string, error) {
	task := factoryapp.PlannerTaskFromContext(ctx)
	p.mu.Lock()
	p.calls = append(p.calls, stubCall{task, prompt})
	fn := p.fn
	p.mu.Unlock()
	if fn == nil {
		return "", errors.New("no stub")
	}
	return fn(ctx, task, prompt)
}

func (p *plannerStub) callsFor(task string) []stubCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []stubCall
	for _, c := range p.calls {
		if c.task == task {
			out = append(out, c)
		}
	}
	return out
}

func (s *stack) thread(t *testing.T, taskID string, internal bool) []e2eMsg {
	t.Helper()
	path := "/v1/tasks/" + taskID
	if internal {
		path += "?internal=1"
	}
	var out struct {
		Task     e2eTask  `json:"task"`
		Messages []e2eMsg `json:"messages"`
	}
	if st := s.do("GET", path, "", nil, &out); st != http.StatusOK {
		t.Fatalf("GET %s = %d", path, st)
	}
	return out.Messages
}

func (s *stack) openAlerts(t *testing.T) []e2eAlert {
	t.Helper()
	var out struct {
		Alerts []e2eAlert `json:"alerts"`
	}
	if st := s.do("GET", "/v1/alerts", "", nil, &out); st != http.StatusOK {
		t.Fatalf("GET /v1/alerts = %d", st)
	}
	return out.Alerts
}

func hasType(msgs []e2eMsg, typ string) bool {
	for _, m := range msgs {
		if m.Type == typ {
			return true
		}
	}
	return false
}

func findAlert(as []e2eAlert, titleSub string) *e2eAlert {
	for i := range as {
		if strings.Contains(as[i].Title, titleSub) {
			return &as[i]
		}
	}
	return nil
}

func TestInboxRedesignE2E(t *testing.T) {
	s := boot(t)
	waitFor(t, "foreman online", 5*time.Second, func() bool {
		var foremen []jobs.Foreman
		_ = s.do("GET", "/v1/foremen", "", nil, &foremen)
		for _, f := range foremen {
			if f.ID == foremanInbox && f.Online {
				return true
			}
		}
		return false
	})
	stub := &plannerStub{}
	s.factory.SetPlannerExec(stub.run)

	var taskID, jobID string

	// (a) operator chat opens a task; (b) the planner's reply lands in it.
	t.Run("A_ChatOpensTask_B_PlannerReplyInThread", func(t *testing.T) {
		stub.set(func(_ context.Context, task, _ string) (string, error) {
			s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "notify_operator", map[string]any{
				"text": "on it", "task_id": task,
			})
			return "ok", nil
		})
		var resp struct {
			ID     string `json:"id"`
			TaskID string `json:"task_id"`
		}
		if st := s.do("POST", "/v1/chat", "", map[string]any{"text": "e2e inbox: count the widgets"}, &resp); st != http.StatusCreated {
			t.Fatalf("POST /v1/chat = %d, want 201", st)
		}
		if !strings.HasPrefix(resp.TaskID, "task_") || resp.ID == "" {
			t.Fatalf("chat response = %+v", resp)
		}
		taskID = resp.TaskID

		var list struct {
			Tasks []e2eTask `json:"tasks"`
		}
		s.do("GET", "/v1/tasks?status=open", "", nil, &list)
		found := false
		for _, tk := range list.Tasks {
			found = found || (tk.ID == taskID && tk.Title == "e2e inbox: count the widgets")
		}
		if !found {
			t.Fatalf("task %s not in open list: %+v", taskID, list.Tasks)
		}

		waitFor(t, "planner reply in thread", 5*time.Second, func() bool {
			return hasType(s.thread(t, taskID, false), "brain.reply")
		})
		if c := stub.callsFor(taskID); len(c) == 0 || !strings.Contains(c[0].prompt, "count the widgets") {
			t.Fatalf("planner not woken on the task with its thread: %+v", c)
		}
	})

	// (c) a job attached to the task: its foreman.report is filed under the
	// task and hidden by default, shown with internal=1, and wakes the
	// planner on that task.
	t.Run("C_JobReportFiledUnderTaskHidden", func(t *testing.T) {
		sc := s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "create_job", map[string]any{
			"task": "e2e inbox job", "task_id": taskID,
		})
		jobID, _ = sc["job_id"].(string)
		if !strings.HasPrefix(jobID, "job_") {
			t.Fatalf("create_job = %v", sc)
		}
		waitFor(t, "job done", 8*time.Second, func() bool {
			var j jobs.Job
			_ = s.do("GET", "/v1/jobs/"+jobID, "", nil, &j)
			return j.State == jobs.StateDone
		})

		before := len(stub.callsFor(taskID))
		bearer := s.pairSynthetic(t, "charlie")
		st := s.req(s.factorySrv.URL, "POST", "/v1/inboxes/factory:main/messages", bearer, wire.MessageEnvelope{
			Type: "foreman.report", JobID: jobID, From: "foreman:charlie",
			Payload: mustJSON(t, map[string]any{"text": "e2e report for the job"}),
		}, nil)
		if st != http.StatusCreated {
			t.Fatalf("post foreman.report = %d", st)
		}
		waitFor(t, "report filed under task (internal view)", 5*time.Second, func() bool {
			return hasType(s.thread(t, taskID, true), "foreman.report")
		})
		if hasType(s.thread(t, taskID, false), "foreman.report") {
			t.Fatal("foreman.report visible in the default (non-internal) view")
		}
		for _, m := range s.thread(t, taskID, true) {
			if m.Type == "foreman.report" && (!m.Internal || m.JobID != jobID) {
				t.Errorf("report row = %+v, want internal with job %s", m, jobID)
			}
		}
		waitFor(t, "planner woken on the task by the report", 5*time.Second, func() bool {
			return len(stub.callsFor(taskID)) > before
		})
		if n := len(s.openAlerts(t)); n != 0 {
			t.Errorf("attributed report raised %d alerts, want 0", n)
		}
	})

	// (d) a foreman raise_alert shows in GET /v1/alerts and acks.
	t.Run("D_ForemanRaiseAlertAcks", func(t *testing.T) {
		s.callTool(s.brainMCP(), s.foreman.BrainToken(), "raise_alert", map[string]any{
			"priority": 1, "title": "e2e foreman alert", "body": "disk is 95% full",
		})
		var a *e2eAlert
		waitFor(t, "alert listed", 5*time.Second, func() bool {
			a = findAlert(s.openAlerts(t), "e2e foreman alert")
			return a != nil
		})
		if a.Priority != 1 || a.State != "open" || a.ExpiresAt != nil || !strings.HasPrefix(a.Source, "foreman:") {
			t.Fatalf("alert = %+v, want open P1 never-expiring from a foreman", a)
		}
		var ack struct {
			Alert e2eAlert `json:"alert"`
		}
		if st := s.do("POST", "/v1/alerts/"+a.ID+"/ack", "", nil, &ack); st != http.StatusOK || ack.Alert.State != "acked" {
			t.Fatalf("ack = %d %+v", st, ack)
		}
		if findAlert(s.openAlerts(t), "e2e foreman alert") != nil {
			t.Fatal("acked alert still listed")
		}
		if st := s.do("POST", "/v1/alerts/"+a.ID+"/ack", "", nil, nil); st != http.StatusConflict {
			t.Errorf("second ack = %d, want 409", st)
		}
	})

	// Glue: an alert reply creates a task and wakes the planner on it.
	t.Run("Glue_AlertReplyWakesPlannerViaThread", func(t *testing.T) {
		stub.set(func(context.Context, string, string) (string, error) { return "ok", nil })
		s.callTool(s.brainMCP(), s.foreman.BrainToken(), "raise_alert", map[string]any{
			"priority": 2, "title": "e2e replyable alert",
		})
		var a *e2eAlert
		waitFor(t, "alert listed", 5*time.Second, func() bool {
			a = findAlert(s.openAlerts(t), "e2e replyable alert")
			return a != nil
		})
		var rep struct {
			Alert  e2eAlert `json:"alert"`
			TaskID string   `json:"task_id"`
		}
		if st := s.do("POST", "/v1/alerts/"+a.ID+"/reply", "", map[string]any{"text": "please look at the replyable thing"}, &rep); st != http.StatusCreated {
			t.Fatalf("reply = %d", st)
		}
		if rep.Alert.State != "replied" || !strings.HasPrefix(rep.TaskID, "task_") {
			t.Fatalf("reply = %+v", rep)
		}
		waitFor(t, "planner woken on the reply's task", 5*time.Second, func() bool {
			return len(stub.callsFor(rep.TaskID)) > 0
		})
		if p := stub.callsFor(rep.TaskID)[0].prompt; !strings.Contains(p, "please look at the replyable thing") || !strings.Contains(p, "e2e replyable alert") {
			t.Errorf("wake prompt lacks the reply/alert seed:\n%s", p)
		}
	})

	// Glue: an unattributed foreman.report opens a task AND raises a P2
	// alert linked to it.
	t.Run("Glue_UnattributedReportAlerts", func(t *testing.T) {
		stub.set(func(context.Context, string, string) (string, error) { return "ok", nil })
		bearer := s.pairSynthetic(t, "delta")
		st := s.req(s.factorySrv.URL, "POST", "/v1/inboxes/factory:main/messages", bearer, wire.MessageEnvelope{
			Type: "foreman.report", JobID: "job_unknown_e2e", From: "foreman:delta",
			Payload: mustJSON(t, map[string]any{"text": "orphan report e2e"}),
		}, nil)
		if st != http.StatusCreated {
			t.Fatalf("post = %d", st)
		}
		var a *e2eAlert
		waitFor(t, "P2 alert", 5*time.Second, func() bool {
			a = findAlert(s.openAlerts(t), "Unattributed foreman.report")
			return a != nil
		})
		if a.Priority != 2 || a.TaskID == nil || !strings.HasPrefix(*a.TaskID, "task_") {
			t.Fatalf("alert = %+v, want P2 linked to a task", a)
		}
		waitFor(t, "planner woken on the new task", 5*time.Second, func() bool {
			return len(stub.callsFor(*a.TaskID)) > 0
		})
	})

	// Glue: a planner session failure raises a P0 alert (plus the note in
	// the task).
	t.Run("Glue_PlannerFailureP0Alert", func(t *testing.T) {
		stub.set(func(context.Context, string, string) (string, error) {
			return "", errors.New("boom e2e planner crash")
		})
		var resp struct {
			TaskID string `json:"task_id"`
		}
		s.do("POST", "/v1/chat", "", map[string]any{"text": "e2e this will fail"}, &resp)
		var a *e2eAlert
		waitFor(t, "P0 alert", 5*time.Second, func() bool {
			a = findAlert(s.openAlerts(t), "Planner session failed")
			return a != nil
		})
		if a.Priority != 0 || a.ExpiresAt != nil || a.TaskID == nil || *a.TaskID != resp.TaskID {
			t.Fatalf("alert = %+v, want never-expiring P0 linked to %s", a, resp.TaskID)
		}
		waitFor(t, "failure note in thread", 5*time.Second, func() bool {
			for _, m := range s.thread(t, resp.TaskID, false) {
				if m.Type == "brain.reply" && strings.Contains(string(m.Payload), "could not run") {
					return true
				}
			}
			return false
		})
	})
}

// pairSynthetic pairs a second, post-only foreman credential so a test can
// drive the factory:main consumer without disturbing the real foreman.
func (s *stack) pairSynthetic(t *testing.T, id string) string {
	t.Helper()
	tok, _, err := s.factory.Auth().IssuePairingToken(s.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	bearer, _, err := s.factory.Auth().PairForeman(s.ctx, tok, id)
	if err != nil {
		t.Fatalf("PairForeman: %v", err)
	}
	return bearer
}
