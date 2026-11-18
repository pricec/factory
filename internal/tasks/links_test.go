package tasks_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/tasks"
)

// TestLinksMigration: a database from before the link columns gets them
// (null), the migration is idempotent, and links work afterwards.
func TestLinksMigration(t *testing.T) {
	ctx := context.Background()
	svc, err := inbox.Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	db := svc.DB()
	if _, err := db.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY, title TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
		priority INTEGER NOT NULL DEFAULT 2, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		closed_at TEXT, close_reason TEXT NOT NULL DEFAULT '');
		INSERT INTO tasks (id, title, created_at, updated_at)
		VALUES ('task_old', 'legacy', '2026-11-01T00:00:00.000Z', '2026-11-01T00:00:00.000Z');`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		st, err := tasks.New(ctx, db)
		if err != nil {
			t.Fatalf("New #%d: %v", i, err)
		}
		old, err := st.GetTask(ctx, "task_old")
		if err != nil || old.FollowUpOf != "" || old.ParentTaskID != "" {
			t.Fatalf("New #%d: legacy = %+v, %v", i, old, err)
		}
		fu, err := st.CreateTaskLinked(ctx, "next", 2, tasks.TaskLinks{FollowUpOf: "task_old"})
		if err != nil || fu.FollowUpOf != "task_old" {
			t.Fatalf("New #%d: follow-up = %+v, %v", i, fu, err)
		}
	}
}

func TestLinkValidation(t *testing.T) {
	ctx := context.Background()
	_, st, _ := newStore(t)
	real, err := st.CreateTask(ctx, "real", 2)
	if err != nil {
		t.Fatal(err)
	}
	for name, l := range map[string]tasks.TaskLinks{
		"unknown follow_up_of":   {FollowUpOf: "task_nope"},
		"unknown parent":         {ParentTaskID: "task_nope"},
		"valid + unknown parent": {FollowUpOf: real.ID, ParentTaskID: "task_nope"},
		"standing follow_up_of":  {FollowUpOf: tasks.StandingID},
		"standing parent":        {ParentTaskID: tasks.StandingID},
	} {
		if _, err := st.CreateTaskLinked(ctx, "x", 2, l); !errors.Is(err, tasks.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	// Nothing was created by the failures.
	all, _ := st.ListTasks(ctx, tasks.TaskFilter{})
	if len(all) != 1 {
		t.Errorf("tasks after failed creates = %d, want 1", len(all))
	}
}

func TestCreateLinkedAndFilter(t *testing.T) {
	ctx := context.Background()
	_, st, clk := newStore(t)
	root, _ := st.CreateTask(ctx, "survey", 2)
	clk.Add(1000000)
	child1, err := st.CreateTaskLinked(ctx, "child1", 2, tasks.TaskLinks{ParentTaskID: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	clk.Add(1000000)
	child2, _ := st.CreateTaskLinked(ctx, "child2", 2, tasks.TaskLinks{ParentTaskID: root.ID})
	clk.Add(1000000)
	fu, err := st.CreateTaskLinked(ctx, "project", 2, tasks.TaskLinks{FollowUpOf: root.ID, ParentTaskID: child1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if fu.FollowUpOf != root.ID || fu.ParentTaskID != child1.ID {
		t.Errorf("created = %+v", fu)
	}

	got, _ := st.GetTask(ctx, root.ID)
	if !reflect.DeepEqual(got.ChildTaskIDs, []string{child1.ID, child2.ID}) || !reflect.DeepEqual(got.FollowUpTaskIDs, []string{fu.ID}) {
		t.Errorf("root reverse links = %v / %v", got.ChildTaskIDs, got.FollowUpTaskIDs)
	}
	if lone, _ := st.GetTask(ctx, child2.ID); lone.ChildTaskIDs == nil || lone.FollowUpTaskIDs == nil || len(lone.ChildTaskIDs)+len(lone.FollowUpTaskIDs) != 0 {
		t.Errorf("leaf reverse links should be empty non-nil: %+v", lone)
	}

	ids := func(f tasks.TaskFilter) []string {
		l, err := st.ListTasks(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range l {
			out = append(out, x.ID)
		}
		return out
	}
	if got := ids(tasks.TaskFilter{ParentTaskID: root.ID}); len(got) != 2 {
		t.Errorf("children of root = %v", got)
	}
	if got := ids(tasks.TaskFilter{ParentTaskID: child1.ID}); !reflect.DeepEqual(got, []string{fu.ID}) {
		t.Errorf("children of child1 = %v", got)
	}
	if got := ids(tasks.TaskFilter{FollowUpOf: root.ID}); !reflect.DeepEqual(got, []string{fu.ID}) {
		t.Errorf("follow-ups of root = %v", got)
	}
	if got := ids(tasks.TaskFilter{ParentTaskID: "task_nope"}); len(got) != 0 {
		t.Errorf("children of unknown = %v", got)
	}
}
