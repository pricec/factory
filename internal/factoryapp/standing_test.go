package factoryapp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/tasks"
)

func TestStandingChatHTTP(t *testing.T) {
	e := newEnv(t)
	wl := &wakeLog{}
	e.a.SetPlannerExec(wl.exec)

	// Exists from boot, readable by id, flagged standing.
	var got struct {
		Task struct {
			taskJSON
			Standing bool `json:"standing"`
		} `json:"task"`
	}
	if s := e.do("GET", "/v1/tasks/"+tasks.StandingID, "", nil, &got); s != http.StatusOK || !got.Task.Standing || got.Task.Status != "open" {
		t.Fatalf("GET standing = %d %+v", s, got.Task)
	}

	// Cannot be closed or renamed.
	if s := e.do("POST", "/v1/tasks/"+tasks.StandingID+"/close", "", nil, nil); s != http.StatusBadRequest {
		t.Errorf("close standing = %d, want 400", s)
	}
	if s := e.do("PATCH", "/v1/tasks/"+tasks.StandingID, "", map[string]any{"title": "x"}, nil); s != http.StatusBadRequest {
		t.Errorf("rename standing = %d, want 400", s)
	}

	// Chat to it wakes a planner session bound to it, with the standing prompt.
	e.chat("where is task X at?", tasks.StandingID)
	waitFor(t, "standing wake", 5*time.Second, func() bool {
		wl.mu.Lock()
		defer wl.mu.Unlock()
		return len(wl.tasks) == 1
	})
	wl.mu.Lock()
	gotTask, prompt := wl.tasks[0], wl.prompts[0]
	wl.mu.Unlock()
	if gotTask != tasks.StandingID {
		t.Errorf("session bound to %q, want standing", gotTask)
	}
	for _, want := range []string{"STANDING factory chat", "where is task X at?", "create_task"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	// Excluded from every list, Needs-you and awaiting views, even after a
	// planner reply (which would make an ordinary task "awaiting").
	e.chat("real work", "")
	for _, q := range []string{"", "?status=open", "?needs_operator=true", "?awaiting=operator", "?q=Planner"} {
		var list struct {
			Tasks []taskJSON `json:"tasks"`
		}
		if s := e.do("GET", "/v1/tasks"+q, "", nil, &list); s != http.StatusOK {
			t.Fatalf("list %q = %d", q, s)
		}
		for _, x := range list.Tasks {
			if x.ID == tasks.StandingID {
				t.Errorf("list %q contains the standing chat", q)
			}
		}
	}
	var th struct {
		Task taskJSON `json:"task"`
	}
	e.do("GET", "/v1/tasks/"+tasks.StandingID, "", nil, &th)
	if th.Task.Awaiting != "factory" {
		t.Errorf("standing awaiting = %q, want factory", th.Task.Awaiting)
	}
}

// TestStandingChatHTTPWindow: the standing thread is windowed to the last
// 10 turns by default; all=1 returns everything (nothing is deleted).
func TestStandingChatHTTPWindow(t *testing.T) {
	e := newEnv(t)
	e.a.SetPlannerExec((&wakeLog{}).exec)
	for i := 0; i < 15; i++ {
		e.chat(fmt.Sprintf("msg %d", i), tasks.StandingID)
	}
	var got struct {
		Messages []json.RawMessage `json:"messages"`
		Window   *struct{ Turns, Older int }
	}
	if s := e.do("GET", "/v1/tasks/"+tasks.StandingID, "", nil, &got); s != http.StatusOK {
		t.Fatalf("GET = %d", s)
	}
	if len(got.Messages) != 10 || got.Window == nil || got.Window.Turns != 10 || got.Window.Older != 5 {
		t.Fatalf("default: %d messages, window %+v; want 10, {10 5}", len(got.Messages), got.Window)
	}
	got.Messages, got.Window = nil, nil
	if s := e.do("GET", "/v1/tasks/"+tasks.StandingID+"?all=1", "", nil, &got); s != http.StatusOK {
		t.Fatalf("GET all = %d", s)
	}
	if len(got.Messages) != 15 || got.Window != nil {
		t.Fatalf("all=1: %d messages, window %+v; want 15, none", len(got.Messages), got.Window)
	}
}
