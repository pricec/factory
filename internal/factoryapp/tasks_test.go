package factoryapp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/factoryapp"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// chatResp is the POST /v1/chat 201 body: the stored envelope plus task_id.
type chatResp struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	From     string          `json:"from"`
	TS       string          `json:"ts"`
	ThreadID string          `json:"thread_id"`
	TaskID   string          `json:"task_id"`
	Payload  json.RawMessage `json:"payload"`
}

type taskJSON struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	Priority       int      `json:"priority"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	ClosedAt       *string  `json:"closed_at"`
	CloseReason    string   `json:"close_reason"`
	JobIDs         []string `json:"job_ids"`
	LastActivityAt string   `json:"last_activity_at"`
	Preview        *string  `json:"preview"`
}

type threadJSON struct {
	Task     taskJSON `json:"task"`
	Messages []struct {
		ID       string          `json:"id"`
		Type     string          `json:"type"`
		From     string          `json:"from"`
		JobID    string          `json:"job_id"`
		TS       string          `json:"ts"`
		Payload  json.RawMessage `json:"payload"`
		Internal bool            `json:"internal"`
	} `json:"messages"`
}

// wakeLog records planner sessions (task id from the session context, the
// prompt) and lets a test block them.
type wakeLog struct {
	mu      sync.Mutex
	tasks   []string
	prompts []string
	block   chan struct{}
	running int
	maxRun  int
}

func (w *wakeLog) exec(ctx context.Context, prompt string) (string, error) {
	w.mu.Lock()
	w.tasks = append(w.tasks, factoryapp.PlannerTaskFromContext(ctx))
	w.prompts = append(w.prompts, prompt)
	w.running++
	if w.running > w.maxRun {
		w.maxRun = w.running
	}
	block := w.block
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running--
		w.mu.Unlock()
	}()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	return "ok", nil
}

func (w *wakeLog) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.tasks)
}

func (w *wakeLog) snapshot() ([]string, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.tasks...), append([]string(nil), w.prompts...)
}

func (e *testEnv) chat(text, taskID string) chatResp {
	e.t.Helper()
	body := map[string]any{"text": text}
	if taskID != "" {
		body["task_id"] = taskID
	}
	var out chatResp
	if status := e.do("POST", "/v1/chat", "", body, &out); status != http.StatusCreated {
		e.t.Fatalf("chat status = %d", status)
	}
	return out
}

func (e *testEnv) thread(taskID string, internal bool) threadJSON {
	e.t.Helper()
	path := "/v1/tasks/" + taskID
	if internal {
		path += "?internal=1"
	}
	var out threadJSON
	if status := e.do("GET", path, "", nil, &out); status != http.StatusOK {
		e.t.Fatalf("GET %s = %d", path, status)
	}
	return out
}

// TestChatOpensAndPostsIntoTasks covers I2/I3: chat without task_id opens
// a task (first 60 chars), with task_id posts into it, a closed task
// reopens, unknown ids 404, and the response keeps the envelope fields.
func TestChatOpensAndPostsIntoTasks(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{}
	e.a.SetPlannerExec(w.exec)

	text := "please   look into the\nflaky deploy pipeline and tell me what is going on with it today"
	first := e.chat(text, "")
	if first.ID == "" || first.Type != "chat.message" || first.From != "human:operator" || first.TS == "" {
		t.Fatalf("chat response lost envelope fields: %+v", first)
	}
	if !strings.HasPrefix(first.TaskID, "task_") || first.ThreadID != first.TaskID {
		t.Fatalf("task_id = %q thread_id = %q", first.TaskID, first.ThreadID)
	}
	th := e.thread(first.TaskID, false)
	wantTitle := "please look into the flaky deploy pipeline and tell me what"
	if th.Task.Title != wantTitle || th.Task.Status != "open" || th.Task.Priority != 2 {
		t.Errorf("task = %+v, want title %q", th.Task, wantTitle)
	}
	if th.Task.Preview == nil || *th.Task.Preview != "please look into the flaky deploy pipeline and tell me what is going on with it today" {
		t.Errorf("preview = %v", th.Task.Preview)
	}
	if th.Task.JobIDs == nil {
		t.Error("job_ids must be [] not null")
	}

	// Into the same task.
	second := e.chat("any update?", first.TaskID)
	if second.TaskID != first.TaskID {
		t.Fatalf("second chat filed under %q", second.TaskID)
	}
	if got := e.thread(first.TaskID, false); len(got.Messages) != 2 || *got.Task.Preview != "any update?" {
		t.Errorf("thread = %+v", got)
	}

	// Closed task reopens.
	closed := tasks.StatusClosed
	reason := "done"
	if _, err := e.a.Tasks().UpdateTask(e.ctx, first.TaskID, tasks.TaskUpdate{Status: &closed, CloseReason: &reason}); err != nil {
		t.Fatal(err)
	}
	e.chat("one more thing", first.TaskID)
	if got := e.thread(first.TaskID, false); got.Task.Status != "open" || got.Task.ClosedAt != nil || got.Task.CloseReason != "" {
		t.Errorf("closed task not reopened: %+v", got.Task)
	}

	// Errors.
	var errResp map[string]any
	if status := e.do("POST", "/v1/chat", "", map[string]any{"text": "x", "task_id": "task_nope"}, &errResp); status != http.StatusNotFound {
		t.Errorf("unknown task_id status = %d, want 404", status)
	}
	if status := e.do("POST", "/v1/chat", "", map[string]any{"text": "  "}, &errResp); status != http.StatusBadRequest {
		t.Errorf("blank text status = %d, want 400", status)
	}

	// POST /v1/tasks is the alias without task_id.
	var alias chatResp
	if status := e.do("POST", "/v1/tasks", "", map[string]any{"text": "new box"}, &alias); status != http.StatusCreated {
		t.Fatalf("POST /v1/tasks = %d", status)
	}
	if alias.TaskID == "" || alias.TaskID == first.TaskID || alias.Type != "chat.message" {
		t.Errorf("alias response = %+v", alias)
	}
	if status := e.do("POST", "/v1/tasks", "", "{", &errResp); status != http.StatusBadRequest {
		t.Errorf("alias bad json = %d", status)
	}
	if status := e.do("POST", "/v1/tasks", "", map[string]any{"task_id": first.TaskID}, &errResp); status != http.StatusBadRequest {
		t.Errorf("alias without text = %d", status)
	}

	// Every chat woke the planner on its own task.
	waitFor(t, "wakes", 5*time.Second, func() bool { return w.count() >= 3 })
}

// TestTaskListAndThreadEndpoints covers I3 GET: ranking, filters,
// validation, internal filtering, 404.
func TestTaskListAndThreadEndpoints(t *testing.T) {
	e := newEnv(t)
	e.a.SetPlannerExec((&wakeLog{}).exec)

	a := e.chat("alpha task", "")
	b := e.chat("bravo task", "")
	c := e.chat("charlie task", "")
	urgent := 0
	if _, err := e.a.Tasks().UpdateTask(e.ctx, b.TaskID, tasks.TaskUpdate{Priority: &urgent}); err != nil {
		t.Fatal(err)
	}
	closed := tasks.StatusClosed
	if _, err := e.a.Tasks().UpdateTask(e.ctx, c.TaskID, tasks.TaskUpdate{Status: &closed}); err != nil {
		t.Fatal(err)
	}

	list := func(q string) []taskJSON {
		t.Helper()
		var out struct {
			Tasks []taskJSON `json:"tasks"`
		}
		if status := e.do("GET", "/v1/tasks"+q, "", nil, &out); status != http.StatusOK {
			t.Fatalf("GET /v1/tasks%s = %d", q, status)
		}
		return out.Tasks
	}
	ids := func(ts []taskJSON) string {
		var s []string
		for _, tk := range ts {
			s = append(s, tk.ID)
		}
		return strings.Join(s, ",")
	}
	if got, want := ids(list("")), strings.Join([]string{b.TaskID, a.TaskID, c.TaskID}, ","); got != want {
		t.Errorf("default (all) order = %s, want %s", got, want)
	}
	if got, want := ids(list("?status=all")), strings.Join([]string{b.TaskID, a.TaskID, c.TaskID}, ","); got != want {
		t.Errorf("all order = %s, want %s", got, want)
	}
	if got, want := ids(list("?status=open")), strings.Join([]string{b.TaskID, a.TaskID}, ","); got != want {
		t.Errorf("open = %s, want %s", got, want)
	}
	if got := ids(list("?status=closed")); got != c.TaskID {
		t.Errorf("closed = %s", got)
	}
	if got := ids(list("?limit=1")); got != b.TaskID {
		t.Errorf("limit=1 = %s", got)
	}
	if got := ids(list("?q=alpha")); got != a.TaskID {
		t.Errorf("q=alpha = %s", got)
	}
	if got := ids(list("?q=" + a.TaskID[:12])); !strings.Contains(got, a.TaskID) {
		t.Errorf("q=id prefix = %s", got)
	}
	if got := list("?q=" + time.Now().UTC().Format("2006-01-02")); len(got) != 3 {
		t.Errorf("q=date = %d tasks", len(got))
	}
	for _, bad := range []string{"?status=bogus", "?limit=0", "?limit=x"} {
		var errResp map[string]any
		if status := e.do("GET", "/v1/tasks"+bad, "", nil, &errResp); status != http.StatusBadRequest {
			t.Errorf("GET /v1/tasks%s = %d, want 400", bad, status)
		}
	}
	// A huge limit clamps rather than failing.
	if got := list("?limit=999999"); len(got) != 3 {
		t.Errorf("clamped limit = %d tasks", len(got))
	}

	// Internal rows: hidden by default, shown with internal=1.
	if _, err := e.a.Inbox().Post(e.ctx, "planner:main", wire.MessageEnvelope{
		Type: "notify.job", From: "factory", JobID: "job_x", ThreadID: a.TaskID,
		Payload: json.RawMessage(`{"job_id":"job_x","state":"done"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.thread(a.TaskID, false); len(got.Messages) != 1 || got.Messages[0].Internal {
		t.Errorf("default thread = %+v", got.Messages)
	}
	full := e.thread(a.TaskID, true)
	if len(full.Messages) != 2 || full.Messages[0].Type != "chat.message" || !full.Messages[1].Internal ||
		full.Messages[1].JobID != "job_x" || full.Messages[1].From != "factory" {
		t.Errorf("internal thread = %+v", full.Messages)
	}
	// The preview ignores internal rows.
	if *full.Task.Preview != "alpha task" {
		t.Errorf("preview = %q", *full.Task.Preview)
	}
	var errResp map[string]any
	if status := e.do("GET", "/v1/tasks/task_nope", "", nil, &errResp); status != http.StatusNotFound {
		t.Errorf("unknown task = %d, want 404", status)
	}
	if status := e.do("GET", "/v1/tasks/"+a.TaskID+"?limit=-1", "", nil, &errResp); status != http.StatusBadRequest {
		t.Errorf("bad thread limit = %d, want 400", status)
	}
	var lim threadJSON
	if status := e.do("GET", "/v1/tasks/"+a.TaskID+"?internal=1&limit=1", "", nil, &lim); status != http.StatusOK || len(lim.Messages) != 1 || lim.Messages[0].Type != "notify.job" {
		t.Errorf("thread limit=1 = %d %+v (want the newest row)", status, lim.Messages)
	}
}

// TestPlannerWakesPerTaskInParallel: chats on two tasks wake two sessions
// at once, each bound to its own task with that task's thread, header
// and the priority criteria in the prompt - not the global history.
func TestPlannerWakesPerTaskInParallel(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{block: make(chan struct{})}
	e.a.SetPlannerExec(w.exec)
	if err := e.a.Tasks().SetPriorityCriteria(e.ctx, "customer outages beat everything"); err != nil {
		t.Fatal(err)
	}

	a := e.chat("task A says hello", "")
	b := e.chat("task B says hi", "")
	waitFor(t, "two parallel sessions", 5*time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.running == 2
	})
	close(w.block)
	gotTasks, prompts := w.snapshot()
	sort.Strings(gotTasks)
	want := []string{a.TaskID, b.TaskID}
	sort.Strings(want)
	if strings.Join(gotTasks, ",") != strings.Join(want, ",") {
		t.Fatalf("session tasks = %v, want %v", gotTasks, want)
	}
	for i, p := range prompts {
		var self, other string
		if strings.Contains(p, "id: "+a.TaskID) {
			self, other = "task A says hello", "task B says hi"
		} else {
			self, other = "task B says hi", "task A says hello"
		}
		for _, s := range []string{"factory planner", self, "customer outages beat everything", "priority: 2", "[new]"} {
			if !strings.Contains(p, s) {
				t.Errorf("prompt %d missing %q", i, s)
			}
		}
		if strings.Contains(p, other) {
			t.Errorf("prompt %d leaks the other task's thread (%q)", i, other)
		}
	}
}

// TestPlannerSameTaskSerializedAndCoalesced: chats arriving while a task's
// session runs never start a second concurrent session; they coalesce
// into one follow-up.
func TestPlannerSameTaskSerializedAndCoalesced(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{block: make(chan struct{})}
	e.a.SetPlannerExec(w.exec)

	first := e.chat("first", "")
	waitFor(t, "first session", 5*time.Second, func() bool { return w.count() == 1 })
	e.chat("second", first.TaskID)
	e.chat("third", first.TaskID)
	// Let the planner loop hand both to the dispatcher.
	time.Sleep(300 * time.Millisecond)
	if n := w.count(); n != 1 {
		t.Fatalf("sessions while the first runs = %d, want 1", n)
	}
	close(w.block)
	waitFor(t, "follow-up session", 5*time.Second, func() bool { return w.count() == 2 })
	time.Sleep(300 * time.Millisecond)
	tasksSeen, prompts := w.snapshot()
	if len(tasksSeen) != 2 || tasksSeen[1] != first.TaskID {
		t.Fatalf("sessions = %v, want 2 on %s", tasksSeen, first.TaskID)
	}
	if !strings.Contains(prompts[1], "second") || !strings.Contains(prompts[1], "third") {
		t.Errorf("follow-up prompt lacks the coalesced chats")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxRun != 1 {
		t.Errorf("max concurrent sessions on one task = %d", w.maxRun)
	}
}

// TestPlannerFailureNoteIntoTask: a failed session posts the brain.reply
// error note INTO the woken task (visible), never silently.
func TestPlannerFailureNoteIntoTask(t *testing.T) {
	e := newEnv(t)
	e.a.SetPlannerExec(func(ctx context.Context, prompt string) (string, error) {
		return "", context.DeadlineExceeded
	})
	c := e.chat("hello?", "")
	waitFor(t, "failure note in task", 5*time.Second, func() bool {
		th := e.thread(c.TaskID, false)
		for _, m := range th.Messages {
			if m.Type == "brain.reply" && strings.Contains(string(m.Payload), "could not run") {
				return true
			}
		}
		return false
	})
}

// TestRelayStampingAndWakes covers I2 relay stamping end to end: a job
// created with task_id (HTTP) files its foreman.report and notify.job
// under the task (internal rows) and wakes the planner there; a spawned
// child's report follows ParentJobID; an unattributed report opens a new
// task and wakes on it.
func TestRelayStampingAndWakes(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{}
	e.a.SetPlannerExec(w.exec)
	bearer := e.pair("alpha", nil)

	c := e.chat("build me a thing", "")
	waitFor(t, "chat wake", 5*time.Second, func() bool { return w.count() == 1 })

	var created struct {
		JobID  string `json:"job_id"`
		TaskID string `json:"task_id"`
	}
	if status := e.do("POST", "/v1/jobs", "", map[string]any{"task": "build", "task_id": c.TaskID}, &created); status != http.StatusCreated {
		t.Fatalf("create status = %d", status)
	}
	if created.TaskID != c.TaskID {
		t.Errorf("create response task_id = %q", created.TaskID)
	}
	var errResp map[string]any
	if status := e.do("POST", "/v1/jobs", "", map[string]any{"task": "x", "task_id": "task_nope"}, &errResp); status != http.StatusNotFound {
		t.Errorf("create with unknown task = %d, want 404", status)
	}
	if th := e.thread(c.TaskID, false); len(th.Task.JobIDs) != 1 || th.Task.JobIDs[0] != created.JobID {
		t.Errorf("task job_ids = %v", th.Task.JobIDs)
	}
	// GET /v1/jobs/{id} echoes the task link (deploy-18 follow-up).
	var got map[string]any
	if status := e.do("GET", "/v1/jobs/"+created.JobID, "", nil, &got); status != http.StatusOK {
		t.Fatalf("get job status = %d", status)
	}
	if got["task_id"] != c.TaskID || got["id"] != created.JobID {
		t.Errorf("GET job = %v, want task_id %q plus the job record", got, c.TaskID)
	}
	var unlinked struct {
		JobID string `json:"job_id"`
	}
	if status := e.do("POST", "/v1/jobs", "", map[string]any{"task": "no task"}, &unlinked); status != http.StatusCreated {
		t.Fatalf("create unlinked status = %d", status)
	}
	got = nil
	if status := e.do("GET", "/v1/jobs/"+unlinked.JobID, "", nil, &got); status != http.StatusOK {
		t.Fatalf("get unlinked job status = %d", status)
	}
	if _, has := got["task_id"]; has {
		t.Errorf("unlinked job carries task_id: %v", got)
	}
	if status := e.do("GET", "/v1/jobs/job_nope", "", nil, &errResp); status != http.StatusNotFound {
		t.Errorf("unknown job = %d, want 404", status)
	}

	post := func(typ, jobID string, payload any) {
		t.Helper()
		if status := e.do("POST", "/v1/inboxes/factory:main/messages", bearer, map[string]any{
			"type": typ, "job_id": jobID, "from": "foreman:alpha", "payload": payload,
		}, nil); status != http.StatusCreated {
			t.Fatalf("post %s = %d", typ, status)
		}
	}

	// Attributed report.
	post("foreman.report", created.JobID, map[string]any{"text": "build is green"})
	waitFor(t, "report wake on the task", 5*time.Second, func() bool { return w.count() == 2 })
	tasksSeen, prompts := w.snapshot()
	if tasksSeen[1] != c.TaskID || !strings.Contains(prompts[1], "build is green") || !strings.Contains(prompts[1], "foreman reports") {
		t.Errorf("report wake: task %s prompt %q", tasksSeen[1], prompts[1])
	}
	if th := e.thread(c.TaskID, false); len(th.Messages) != 1 {
		t.Errorf("report leaked into the default view: %+v", th.Messages)
	}
	full := e.thread(c.TaskID, true)
	if last := full.Messages[len(full.Messages)-1]; last.Type != "foreman.report" || !last.Internal || last.JobID != created.JobID {
		t.Errorf("internal thread tail = %+v", last)
	}

	// Spawned child (spawn_child -> RegisterSpawned) resolves via parent.
	child := wire.NewJobSpec("child", "worker")
	child.ParentJobID = created.JobID
	post("job.event", child.ID, map[string]any{"event": "accepted", "spec": child})
	waitFor(t, "child registered", 5*time.Second, func() bool {
		_, ok, _ := e.a.Jobs().Get(e.ctx, child.ID)
		return ok
	})
	post("brain.failed", child.ID, map[string]any{"envelope_id": "msg_1", "envelope_type": "x", "error": "child brain died"})
	waitFor(t, "child failure wake on the parent's task", 5*time.Second, func() bool { return w.count() == 3 })
	tasksSeen, _ = w.snapshot()
	if tasksSeen[2] != c.TaskID {
		t.Errorf("child report woke task %s, want %s", tasksSeen[2], c.TaskID)
	}
	if th := e.thread(c.TaskID, false); len(th.Task.JobIDs) != 2 {
		t.Errorf("child not linked: %v", th.Task.JobIDs)
	}

	// Unattributed report: a new task, titled from the payload, woken.
	post("foreman.report", "", map[string]any{"text": "disk on server is 95% full"})
	waitFor(t, "unattributed wake", 5*time.Second, func() bool { return w.count() == 4 })
	tasksSeen, _ = w.snapshot()
	newTask := tasksSeen[3]
	if newTask == c.TaskID || newTask == "" {
		t.Fatalf("unattributed report woke %q", newTask)
	}
	if th := e.thread(newTask, true); th.Task.Title != "disk on server is 95% full" || len(th.Messages) != 1 || th.Messages[0].Type != "foreman.report" {
		t.Errorf("unattributed task = %+v / %+v", th.Task, th.Messages)
	}

	// notify.job on terminal: stamped onto the task, internal, no wake.
	post("job.event", created.JobID, map[string]any{"event": "started"})
	post("job.result", created.JobID, map[string]any{
		"job_id": created.JobID, "status": "done", "summary": "built",
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "cost_usd": 0.001},
	})
	waitFor(t, "notify.job in the task thread", 5*time.Second, func() bool {
		for _, m := range e.thread(c.TaskID, true).Messages {
			if m.Type == "notify.job" && m.JobID == created.JobID && strings.Contains(string(m.Payload), created.JobID) {
				return m.Internal
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	if n := w.count(); n != 4 {
		t.Errorf("notify.job woke the planner: %d sessions", n)
	}
}

// TestUnthreadedWakerGetsATask: a chat.message posted straight into
// planner:main without a thread (legacy clients) is filed under a new
// task and wakes the planner there.
func TestUnthreadedWakerGetsATask(t *testing.T) {
	e := newEnv(t)
	w := &wakeLog{}
	e.a.SetPlannerExec(w.exec)
	posted, err := e.a.Inbox().Post(e.ctx, "planner:main", wire.MessageEnvelope{
		Type: "chat.message", From: "human:operator", Payload: json.RawMessage(`{"text":"legacy path"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// And one naming a task that does not exist.
	if _, err := e.a.Inbox().Post(e.ctx, "planner:main", wire.MessageEnvelope{
		Type: "chat.message", From: "human:operator", ThreadID: "task_bogus", Payload: json.RawMessage(`{"text":"bogus thread"}`),
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "wakes", 5*time.Second, func() bool { return w.count() == 2 })
	tasksSeen, _ := w.snapshot()
	if tasksSeen[0] == "" || tasksSeen[1] == "" || tasksSeen[0] == tasksSeen[1] {
		t.Fatalf("session tasks = %v", tasksSeen)
	}
	titles := map[string]string{}
	for _, id := range tasksSeen {
		titles[e.thread(id, false).Task.Title] = id
	}
	if titles["legacy path"] == "" || titles["bogus thread"] == "" {
		t.Fatalf("woken task titles = %v", titles)
	}
	env, _, _ := e.a.Inbox().GetMessage(e.ctx, posted.ID)
	if env.ThreadID != titles["legacy path"] {
		t.Errorf("legacy message re-threaded to %q, want %q", env.ThreadID, titles["legacy path"])
	}
}

// TestTaskChangedSSE: the planner:main stream carries synthetic
// task.changed envelopes (payload = Task) alongside the threaded message
// (I5).
func TestTaskChangedSSE(t *testing.T) {
	e := newEnv(t)
	e.a.SetPlannerExec((&wakeLog{}).exec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/v1/inboxes/planner:main/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := make(chan wire.MessageEnvelope, 16)
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
	c := e.chat("stream me", "")
	var sawMsg, sawTask bool
	for !(sawMsg && sawTask) {
		select {
		case env := <-got:
			switch env.Type {
			case "chat.message":
				sawMsg = env.ThreadID == c.TaskID
			case "task.changed":
				var tk taskJSON
				if err := json.Unmarshal(env.Payload, &tk); err != nil || tk.ID != c.TaskID || tk.Preview == nil {
					t.Fatalf("task.changed payload = %s (%v)", env.Payload, err)
				}
				sawTask = env.ThreadID == c.TaskID
			}
		case <-ctx.Done():
			t.Fatalf("stream: chat.message=%v task.changed=%v", sawMsg, sawTask)
		}
	}
	// Non-persisted: task.changed never lands in the inbox.
	var envs []wire.MessageEnvelope
	e.do("GET", "/v1/inboxes/planner:main/recent?limit=50", "", nil, &envs)
	for _, env := range envs {
		if env.Type == "task.changed" {
			t.Fatal("task.changed was persisted")
		}
	}
}
