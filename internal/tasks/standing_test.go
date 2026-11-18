package tasks_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/tasks"
)

func TestStandingChat(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := newStore(t)
	_ = svc

	// Created automatically, flagged, open; idempotent.
	got, err := st.GetTask(ctx, tasks.StandingID)
	if err != nil || !got.Standing || got.Status != tasks.StatusOpen || got.Title != tasks.StandingTitle {
		t.Fatalf("standing = %+v, %v", got, err)
	}
	if err := st.EnsureStanding(ctx); err != nil {
		t.Fatal(err)
	}

	// Excluded from every listing unless asked for.
	normal, err := st.CreateTask(ctx, "normal", tasks.DefaultPriority)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"", "open", "closed"} {
		list, err := st.ListTasks(ctx, tasks.TaskFilter{Status: status, Query: "Planner"})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range list {
			if x.Standing {
				t.Errorf("status %q: list contains the standing chat", status)
			}
		}
	}
	list, _ := st.ListTasks(ctx, tasks.TaskFilter{})
	if len(list) != 1 || list[0].ID != normal.ID {
		t.Fatalf("list = %+v", list)
	}
	list, _ = st.ListTasks(ctx, tasks.TaskFilter{IncludeStanding: true})
	if len(list) != 2 {
		t.Fatalf("list with standing = %d, want 2", len(list))
	}

	// Cannot be closed, renamed, flagged, or own jobs.
	cases := []struct {
		name string
		u    tasks.TaskUpdate
	}{
		{"close", tasks.TaskUpdate{Status: ptr(tasks.StatusClosed)}},
		{"rename", tasks.TaskUpdate{Title: ptr("x")}},
		{"needs_operator", tasks.TaskUpdate{NeedsOperator: ptr(true)}},
	}
	for _, c := range cases {
		if _, err := st.UpdateTask(ctx, tasks.StandingID, c.u); !errors.Is(err, tasks.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", c.name, err)
		}
	}
	if err := st.LinkJob(ctx, tasks.StandingID, "job_x"); !errors.Is(err, tasks.ErrInvalid) {
		t.Errorf("LinkJob err = %v, want ErrInvalid", err)
	}
	if got, _ := st.GetTask(ctx, tasks.StandingID); got.Status != tasks.StatusOpen {
		t.Errorf("status = %q after refused close", got.Status)
	}
}

func TestStandingSurvivesRestartAndRepairs(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := newStore(t)
	// Simulate corruption: closed and unflagged row.
	if _, err := svc.DB().ExecContext(ctx, `UPDATE tasks SET status='closed', closed_at='2026-01-01T00:00:00.000Z', standing=0 WHERE id=?`, tasks.StandingID); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.New(ctx, svc.DB()); err != nil { // "restart"
		t.Fatal(err)
	}
	got, err := st.GetTask(ctx, tasks.StandingID)
	if err != nil || !got.Standing || got.Status != tasks.StatusOpen || got.ClosedAt != nil {
		t.Fatalf("after re-open = %+v, %v", got, err)
	}
}

func TestStandingSummary(t *testing.T) {
	ctx := context.Background()
	_, st, _ := newStore(t)
	if s, th, err := st.StandingSummary(ctx); err != nil || s != "" || th != "" {
		t.Fatalf("empty = %q %q %v", s, th, err)
	}
	if err := st.SetStandingSummary(ctx, "héllo "+strings.Repeat("é", tasks.StandingSummaryMax), "msg_1"); err != nil {
		t.Fatal(err)
	}
	s, th, _ := st.StandingSummary(ctx)
	if th != "msg_1" || len(s) > tasks.StandingSummaryMax || !strings.HasPrefix(s, "héllo") {
		t.Fatalf("summary len=%d through=%q", len(s), th)
	}
}
