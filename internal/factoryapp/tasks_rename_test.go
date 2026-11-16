package factoryapp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

type renameResp struct {
	Task taskJSON `json:"task"`
}

// TestRenameTask: PATCH /v1/tasks/{id} trims and stores the title, returns
// the task, and a re-read agrees.
func TestRenameTask(t *testing.T) {
	e := newEnv(t)
	c := e.chat("original ask", "")
	var out renameResp
	if st := e.do("PATCH", "/v1/tasks/"+c.TaskID, "", map[string]string{"title": "  Better name \n"}, &out); st != http.StatusOK {
		t.Fatalf("status = %d, want 200", st)
	}
	if out.Task.ID != c.TaskID || out.Task.Title != "Better name" || out.Task.Preview == nil {
		t.Errorf("response task = %+v", out.Task)
	}
	if got := e.thread(c.TaskID, false).Task.Title; got != "Better name" {
		t.Errorf("stored title = %q", got)
	}
	// Exactly the limit (in characters, not bytes) is accepted.
	max := strings.Repeat("é", 200)
	if st := e.do("PATCH", "/v1/tasks/"+c.TaskID, "", map[string]string{"title": max}, &out); st != http.StatusOK || out.Task.Title != max {
		t.Errorf("200-char title: status %d title len %d", st, len([]rune(out.Task.Title)))
	}
}

// TestRenameTaskRejects: empty / blank / missing / over-long titles and bad
// JSON are 400 and leave the title alone.
func TestRenameTaskRejects(t *testing.T) {
	e := newEnv(t)
	c := e.chat("keep me", "")
	before := e.thread(c.TaskID, false).Task.Title
	for name, body := range map[string]any{
		"empty":    map[string]string{"title": ""},
		"blank":    map[string]string{"title": " \t\n "},
		"missing":  map[string]string{},
		"too long": map[string]string{"title": strings.Repeat("x", 201)},
		"bad json": "{nope",
	} {
		if st := e.do("PATCH", "/v1/tasks/"+c.TaskID, "", body, &map[string]any{}); st != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, st)
		}
	}
	if got := e.thread(c.TaskID, false).Task.Title; got != before {
		t.Errorf("title changed to %q after rejected renames", got)
	}
}

// TestRenameTaskUnknown: unknown id -> 404 unknown_task.
func TestRenameTaskUnknown(t *testing.T) {
	e := newEnv(t)
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if st := e.do("PATCH", "/v1/tasks/task_nope", "", map[string]string{"title": "x"}, &out); st != http.StatusNotFound || out.Error.Code != "unknown_task" {
		t.Errorf("status = %d code = %q, want 404 unknown_task", st, out.Error.Code)
	}
}

// TestRenameClosedTask: a closed task can be renamed and stays closed.
func TestRenameClosedTask(t *testing.T) {
	e := newEnv(t)
	c := e.chat("old stuff", "")
	closed, reason := tasks.StatusClosed, "done"
	if _, err := e.a.Tasks().UpdateTask(e.ctx, c.TaskID, tasks.TaskUpdate{Status: &closed, CloseReason: &reason}); err != nil {
		t.Fatal(err)
	}
	var out renameResp
	if st := e.do("PATCH", "/v1/tasks/"+c.TaskID, "", map[string]string{"title": "Archived name"}, &out); st != http.StatusOK {
		t.Fatalf("status = %d, want 200", st)
	}
	if out.Task.Title != "Archived name" || out.Task.Status != "closed" || out.Task.CloseReason != "done" {
		t.Errorf("task = %+v, want renamed and still closed", out.Task)
	}
}

// TestRenameTaskNoBearer: the operator UI surface is unauthenticated
// (spec §7.5, same as POST /v1/chat); the browser has no token to send. A
// bearer, valid or not, is ignored. This pins that contract: if the
// surface ever gains auth, this test and the UI must change together.
func TestRenameTaskNoBearer(t *testing.T) {
	e := newEnv(t)
	c := e.chat("x", "")
	for _, bearer := range []string{"", "garbage"} {
		if st := e.do("PATCH", "/v1/tasks/"+c.TaskID, bearer, map[string]string{"title": "n" + bearer}, &renameResp{}); st != http.StatusOK {
			t.Errorf("bearer %q: status = %d, want 200", bearer, st)
		}
	}
}

// TestRenameTaskSSE: a rename publishes a task.changed envelope carrying
// the new title on the planner:main stream.
func TestRenameTaskSSE(t *testing.T) {
	e := newEnv(t)
	e.a.SetPlannerExec((&wakeLog{}).exec)
	c := e.chat("stream rename", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/v1/inboxes/planner:main/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan wire.MessageEnvelope, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var env wire.MessageEnvelope
				if json.Unmarshal([]byte(data), &env) == nil {
					got <- env
				}
			}
		}
	}()
	// The stream is live once the headers arrived; rename and wait for the
	// event carrying the new title (earlier task.changed from chat wakes may
	// precede it).
	if st := e.do("PATCH", "/v1/tasks/"+c.TaskID, "", map[string]string{"title": "Renamed live"}, &renameResp{}); st != http.StatusOK {
		t.Fatalf("status = %d", st)
	}
	for {
		select {
		case env := <-got:
			if env.Type != "task.changed" || env.ThreadID != c.TaskID {
				continue
			}
			var tk taskJSON
			if err := json.Unmarshal(env.Payload, &tk); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if tk.Title == "Renamed live" {
				return
			}
		case <-ctx.Done():
			t.Fatal("no task.changed with the new title on the stream")
		}
	}
}

// TestRenameRacesPlanner: last write wins between the operator rename and a
// planner-style UpdateTask retitle.
func TestRenameRacesPlanner(t *testing.T) {
	e := newEnv(t)
	c := e.chat("race", "")
	planner := "planner title"
	if _, err := e.a.Tasks().UpdateTask(e.ctx, c.TaskID, tasks.TaskUpdate{Title: &planner}); err != nil {
		t.Fatal(err)
	}
	e.do("PATCH", "/v1/tasks/"+c.TaskID, "", map[string]string{"title": "operator title"}, &renameResp{})
	if got := e.thread(c.TaskID, false).Task.Title; got != "operator title" {
		t.Errorf("title = %q, want the later (operator) write", got)
	}
}
