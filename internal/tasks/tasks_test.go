package tasks_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// fakeClock is an injectable, steppable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newStore(t *testing.T) (*inbox.Service, *tasks.Store, *fakeClock) {
	t.Helper()
	svc, err := inbox.Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	st, err := tasks.New(context.Background(), svc.DB())
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	st.Now = clk.Now
	return svc, st, clk
}

func ptr[T any](v T) *T { return &v }

func TestIDs(t *testing.T) {
	a, b := tasks.NewTaskID(), tasks.NewTaskID()
	if !strings.HasPrefix(a, "task_") || len(a) != len("task_")+26 || a == b {
		t.Errorf("task ids %q %q", a, b)
	}
	if al := tasks.NewAlertID(); !strings.HasPrefix(al, "alert_") || len(al) != len("alert_")+26 {
		t.Errorf("alert id %q", al)
	}
}

func TestIsInternal(t *testing.T) {
	for typ, want := range map[string]bool{
		"chat.message":     false,
		"brain.reply":      false,
		"foreman.report":   true,
		"brain.failed":     true,
		"notify.job":       true,
		"notify.pending":   true,
		"notify.unhandled": true,
		"job.result":       true,
		"job.assign":       true,
		"alert.raise":      true,
		"":                 true,
		"Chat.Message":     true,
	} {
		if got := tasks.IsInternal(typ); got != want {
			t.Errorf("IsInternal(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestCreateGetTask(t *testing.T) {
	ctx := context.Background()
	_, st, clk := newStore(t)
	tk, err := st.CreateTask(ctx, "Fix the build", tasks.DefaultPriority)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != tasks.StatusOpen || tk.Priority != 2 || tk.Title != "Fix the build" ||
		!tk.CreatedAt.Equal(clk.Now()) || !tk.UpdatedAt.Equal(clk.Now()) || tk.ClosedAt != nil ||
		tk.CloseReason != "" || tk.JobIDs == nil || len(tk.JobIDs) != 0 || !tk.LastActivityAt.Equal(clk.Now()) {
		t.Fatalf("created = %+v", tk)
	}
	got, err := st.GetTask(ctx, tk.ID)
	if err != nil || !reflect.DeepEqual(got, tk) {
		t.Fatalf("get = %+v %v, want %+v", got, err, tk)
	}
	if _, err := st.GetTask(ctx, "task_nope"); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	for _, title := range []string{"", "   "} {
		if _, err := st.CreateTask(ctx, title, 1); !errors.Is(err, tasks.ErrInvalid) {
			t.Errorf("title %q: err %v", title, err)
		}
	}
}

func TestUpdateTask(t *testing.T) {
	ctx := context.Background()
	_, st, clk := newStore(t)
	tk, _ := st.CreateTask(ctx, "a", 2)

	clk.Add(time.Minute)
	u, err := st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{Title: ptr("b"), Priority: ptr(0)})
	if err != nil || u.Title != "b" || u.Priority != 0 || !u.UpdatedAt.Equal(clk.Now()) || u.Status != "open" {
		t.Fatalf("retitle: %+v %v", u, err)
	}

	clk.Add(time.Minute)
	closedAt := clk.Now()
	u, err = st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{Status: ptr("closed"), CloseReason: ptr("done")})
	if err != nil || u.Status != "closed" || u.ClosedAt == nil || !u.ClosedAt.Equal(closedAt) || u.CloseReason != "done" {
		t.Fatalf("close: %+v %v", u, err)
	}
	// Closing again keeps the original closed_at.
	clk.Add(time.Minute)
	u, err = st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{Status: ptr("closed"), CloseReason: ptr("idle-24h")})
	if err != nil || !u.ClosedAt.Equal(closedAt) || u.CloseReason != "idle-24h" {
		t.Fatalf("re-close: %+v %v", u, err)
	}
	// Reopen clears closed_at and close_reason.
	clk.Add(time.Minute)
	u, err = st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{Status: ptr("open")})
	if err != nil || u.Status != "open" || u.ClosedAt != nil || u.CloseReason != "" || !u.UpdatedAt.Equal(clk.Now()) {
		t.Fatalf("reopen: %+v %v", u, err)
	}

	cases := []struct {
		name string
		id   string
		u    tasks.TaskUpdate
		want error
	}{
		{"unknown", "task_nope", tasks.TaskUpdate{Title: ptr("x")}, tasks.ErrNotFound},
		{"bad status", tk.ID, tasks.TaskUpdate{Status: ptr("done")}, tasks.ErrInvalid},
		{"empty title", tk.ID, tasks.TaskUpdate{Title: ptr(" ")}, tasks.ErrInvalid},
	}
	for _, c := range cases {
		if _, err := st.UpdateTask(ctx, c.id, c.u); !errors.Is(err, c.want) {
			t.Errorf("%s: err %v, want %v", c.name, err, c.want)
		}
	}
	// Empty update just bumps updated_at.
	clk.Add(time.Minute)
	if u, err := st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{}); err != nil || !u.UpdatedAt.Equal(clk.Now()) {
		t.Errorf("noop update: %+v %v", u, err)
	}
	clk.Add(time.Minute)
	if err := st.TouchTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.GetTask(ctx, tk.ID); !g.UpdatedAt.Equal(clk.Now()) {
		t.Errorf("touch: updated_at %v", g.UpdatedAt)
	}
	if err := st.TouchTask(ctx, "task_nope"); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("touch unknown: %v", err)
	}
}

func TestTaskJobs(t *testing.T) {
	ctx := context.Background()
	_, st, _ := newStore(t)
	a, _ := st.CreateTask(ctx, "a", 2)
	b, _ := st.CreateTask(ctx, "b", 2)
	for _, j := range []string{"job_2", "job_1", "job_2"} { // dup is a no-op
		if err := st.LinkJob(ctx, a.ID, j); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.LinkJob(ctx, b.ID, "job_1"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkJob(ctx, "task_nope", "job_9"); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("link unknown task: %v", err)
	}
	if err := st.LinkJob(ctx, a.ID, ""); !errors.Is(err, tasks.ErrInvalid) {
		t.Errorf("link empty job: %v", err)
	}
	if got, _ := st.JobIDs(ctx, a.ID); !reflect.DeepEqual(got, []string{"job_2", "job_1"}) {
		t.Errorf("JobIDs(a) = %v", got)
	}
	if got, err := st.JobIDs(ctx, "task_nope"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("JobIDs(unknown) = %#v %v", got, err)
	}
	if g, _ := st.GetTask(ctx, a.ID); !reflect.DeepEqual(g.JobIDs, []string{"job_2", "job_1"}) {
		t.Errorf("GetTask JobIDs = %v", g.JobIDs)
	}
	if tid, ok, err := st.TaskForJob(ctx, "job_1"); err != nil || !ok || tid != a.ID {
		t.Errorf("TaskForJob(job_1) = %q %v %v, want earliest link %s", tid, ok, err, a.ID)
	}
	if _, ok, err := st.TaskForJob(ctx, "job_none"); err != nil || ok {
		t.Errorf("TaskForJob(unlinked) ok=%v err=%v", ok, err)
	}
	ls, _ := st.ListTasks(ctx, tasks.TaskFilter{})
	for _, tk := range ls {
		want := map[string][]string{a.ID: {"job_2", "job_1"}, b.ID: {"job_1"}}[tk.ID]
		if !reflect.DeepEqual(tk.JobIDs, want) {
			t.Errorf("list %s JobIDs = %v, want %v", tk.ID, tk.JobIDs, want)
		}
	}
}

func TestListTasksRankingAndFilters(t *testing.T) {
	ctx := context.Background()
	svc, st, clk := newStore(t)

	mk := func(title string, prio int) tasks.Task {
		clk.Add(time.Minute)
		tk, err := st.CreateTask(ctx, title, prio)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	oldP1 := mk("old p1", 1)
	newP1 := mk("new p1", 1)
	p0 := mk("urgent 100% sure_thing", 0)
	p3 := mk("low", 3)
	closedP0 := mk("closed zero", 0)
	clk.Add(time.Minute)
	if _, err := st.UpdateTask(ctx, closedP0.ID, tasks.TaskUpdate{Status: ptr("closed")}); err != nil {
		t.Fatal(err)
	}
	// A thread message on oldP1 makes it the most recently active P1.
	clk.Add(time.Hour)
	if _, err := svc.Post(ctx, "planner:main", wire.MessageEnvelope{Type: "chat.message", From: "human:operator", ThreadID: oldP1.ID}); err != nil {
		t.Fatal(err)
	}

	ids := func(ts []tasks.Task) []string {
		var out []string
		for _, tk := range ts {
			out = append(out, tk.ID)
		}
		return out
	}
	all, err := st.ListTasks(ctx, tasks.TaskFilter{Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{p0.ID, oldP1.ID, newP1.ID, p3.ID, closedP0.ID}
	if got := ids(all); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranking = %v, want %v", got, want)
	}
	if all[1].LastActivityAt.Before(all[2].LastActivityAt) || all[1].LastActivityAt.Equal(all[1].UpdatedAt) {
		t.Errorf("oldP1 LastActivityAt %v should come from its message", all[1].LastActivityAt)
	}

	cases := []struct {
		name string
		f    tasks.TaskFilter
		want []string
	}{
		{"open", tasks.TaskFilter{Status: "open"}, []string{p0.ID, oldP1.ID, newP1.ID, p3.ID}},
		{"closed", tasks.TaskFilter{Status: "closed"}, []string{closedP0.ID}},
		{"limit", tasks.TaskFilter{Limit: 2}, []string{p0.ID, oldP1.ID}},
		{"title substr case-insensitive", tasks.TaskFilter{Query: "P1"}, []string{oldP1.ID, newP1.ID}},
		{"like wildcards escaped %", tasks.TaskFilter{Query: "100%"}, []string{p0.ID}},
		{"like wildcards escaped _", tasks.TaskFilter{Query: "d_p"}, nil}, // unescaped _ would match "old p1",
		{"id prefix", tasks.TaskFilter{Query: p3.ID[:len(p3.ID)-3]}, []string{p3.ID}},
		{"id prefix without task_", tasks.TaskFilter{Query: strings.TrimPrefix(p3.ID, "task_")}, []string{p3.ID}},
		{"date prefix", tasks.TaskFilter{Query: "2026-11-16", Status: "closed"}, []string{closedP0.ID}},
		{"date no match", tasks.TaskFilter{Query: "2025-01-01"}, nil},
		{"query + status", tasks.TaskFilter{Query: "zero", Status: "open"}, nil},
	}
	for _, c := range cases {
		got, err := st.ListTasks(ctx, c.f)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if g := ids(got); !reflect.DeepEqual(g, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, g, c.want)
		}
	}
	if _, err := st.ListTasks(ctx, tasks.TaskFilter{Status: "bogus"}); !errors.Is(err, tasks.ErrInvalid) {
		t.Errorf("bad status: %v", err)
	}
}
