package factoryapp_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// taskWithFrom is taskJSON plus the phone-UI preview_from field.
type taskWithFrom struct {
	taskJSON
	PreviewFrom string `json:"preview_from"`
}

// TestCloseTaskEndpoint: POST /v1/tasks/{id}/close closes a task with a
// default or given reason, is idempotent, validates input, 404s unknown
// ids, and a reply reopens the task as usual.
func TestCloseTaskEndpoint(t *testing.T) {
	e := newEnv(t)
	e.a.SetPlannerExec((&wakeLog{}).exec)
	a := e.chat("close me by default", "")
	b := e.chat("close me with a reason", "")

	tests := []struct {
		name       string
		id         string
		body       any
		wantStatus int
		wantReason string
	}{
		{"default reason, empty body", a.TaskID, nil, http.StatusOK, "operator"},
		{"already closed keeps reason", a.TaskID, map[string]string{"reason": "other"}, http.StatusOK, "operator"},
		{"given reason trimmed", b.TaskID, map[string]string{"reason": "  done on phone "}, http.StatusOK, "done on phone"},
		{"unknown task", "task_nope", nil, http.StatusNotFound, ""},
		{"bad json", b.TaskID, "{", http.StatusBadRequest, ""},
		{"reason too long", b.TaskID, map[string]string{"reason": strings.Repeat("x", 201)}, http.StatusBadRequest, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out struct {
				Task taskJSON `json:"task"`
			}
			var dst any
			if tc.wantStatus == http.StatusOK {
				dst = &out
			}
			if got := e.do("POST", "/v1/tasks/"+tc.id+"/close", "", tc.body, dst); got != tc.wantStatus {
				t.Fatalf("status = %d, want %d", got, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			if out.Task.Status != "closed" || out.Task.CloseReason != tc.wantReason || out.Task.ClosedAt == nil {
				t.Fatalf("task = %+v, want closed with reason %q", out.Task, tc.wantReason)
			}
		})
	}

	// Replying reopens an operator-closed task.
	e.chat("actually, one more thing", a.TaskID)
	if th := e.thread(a.TaskID, false); th.Task.Status != "open" {
		t.Fatalf("after reply status = %q, want open", th.Task.Status)
	}
}

// TestTaskPreviewFrom: preview_from names who wrote the preview message.
func TestTaskPreviewFrom(t *testing.T) {
	e := newEnv(t)
	fail := make(chan struct{})
	e.a.SetPlannerExec(func(ctx context.Context, prompt string) (string, error) {
		<-fail
		return "", context.DeadlineExceeded
	})
	c := e.chat("hello planner", "")
	get := func() taskWithFrom {
		var out struct {
			Task taskWithFrom `json:"task"`
		}
		if s := e.do("GET", "/v1/tasks/"+c.TaskID, "", nil, &out); s != http.StatusOK {
			t.Fatalf("GET task = %d", s)
		}
		return out.Task
	}
	if got := get().PreviewFrom; got != "operator" {
		t.Fatalf("preview_from after operator chat = %q, want operator", got)
	}
	// A planner failure note is a brain.reply -> "planner".
	close(fail)
	waitFor(t, "planner preview", 5*time.Second, func() bool { return get().PreviewFrom == "planner" })

	var list struct {
		Tasks []taskWithFrom `json:"tasks"`
	}
	e.do("GET", "/v1/tasks", "", nil, &list)
	if len(list.Tasks) != 1 || list.Tasks[0].PreviewFrom != "planner" {
		t.Fatalf("list = %+v", list.Tasks)
	}
}
