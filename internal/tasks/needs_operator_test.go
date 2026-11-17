package tasks_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/tasks"
)

// TestNeedsOperator covers the needs_operator flag (spec §9.5.2): set and
// cleared by UpdateTask, cleared by ClearNeedsOperator and by a close.
func TestNeedsOperator(t *testing.T) {
	ctx := context.Background()
	_, st, _ := newStore(t)
	tk, err := st.CreateTask(ctx, "deploy", tasks.DefaultPriority)
	if err != nil {
		t.Fatal(err)
	}
	if tk.NeedsOperator {
		t.Fatal("a new task must not need the operator")
	}
	get := func() tasks.Task {
		t.Helper()
		got, err := st.GetTask(ctx, tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	steps := []struct {
		name string
		do   func() error
		want bool
	}{
		{"set", func() error {
			_, err := st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{NeedsOperator: ptr(true)})
			return err
		}, true},
		{"retitle keeps it", func() error {
			_, err := st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{Title: ptr("deploy v2")})
			return err
		}, true},
		{"clear explicitly", func() error {
			_, err := st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{NeedsOperator: ptr(false)})
			return err
		}, false},
		{"set again", func() error {
			_, err := st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{NeedsOperator: ptr(true)})
			return err
		}, true},
		{"operator answered", func() error { return st.ClearNeedsOperator(ctx, tk.ID) }, false},
		{"set before close", func() error {
			_, err := st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{NeedsOperator: ptr(true)})
			return err
		}, true},
		{"close clears", func() error {
			_, err := st.UpdateTask(ctx, tk.ID, tasks.TaskUpdate{Status: ptr(tasks.StatusClosed)})
			return err
		}, false},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got := get().NeedsOperator; got != s.want {
			t.Errorf("%s: needs_operator = %v, want %v", s.name, got, s.want)
		}
	}
	list, err := st.ListTasks(ctx, tasks.TaskFilter{})
	if err != nil || len(list) != 1 || list[0].NeedsOperator {
		t.Errorf("ListTasks = %+v, %v", list, err)
	}
	if err := st.ClearNeedsOperator(ctx, "task_nope"); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("ClearNeedsOperator(unknown) = %v, want ErrNotFound", err)
	}
}

// TestNeedsOperatorMigration opens a database created before the column
// existed: New adds it (default false) and stays idempotent.
func TestNeedsOperatorMigration(t *testing.T) {
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
		got, err := st.GetTask(ctx, "task_old")
		// Pass 0: the migrated default. Pass 1: the flag set in pass 0
		// survived the second (no-op) migration.
		if err != nil || got.NeedsOperator != (i == 1) {
			t.Fatalf("New #%d: task_old = %+v, %v", i, got, err)
		}
		if _, err := st.UpdateTask(ctx, "task_old", tasks.TaskUpdate{NeedsOperator: ptr(true)}); err != nil {
			t.Fatal(err)
		}
	}
}
