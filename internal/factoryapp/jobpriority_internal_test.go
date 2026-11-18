package factoryapp

import (
	"context"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
)

func newRedApp(t *testing.T) (*App, *gateClock) {
	t.Helper()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	t.Cleanup(func() { _ = a.Close() })
	setUsage(a, clk, 99) // RED
	return a, clk
}

func fileHeldJob(t *testing.T, a *App, taskID string) string {
	t.Helper()
	_, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "work", "task_id": taskID, "priority": 5})
	if out["state"] != jobs.StateHeld {
		t.Fatalf("job not held in RED: %v", out)
	}
	return out["job_id"].(string)
}

func TestSetJobPriorityReleasesHeldJob(t *testing.T) {
	ctx := context.Background()
	a, _ := newRedApp(t)
	task, _ := a.tasks.CreateTask(ctx, "normal", 2)
	id := fileHeldJob(t, a, task.ID)

	old, ok, err := a.jobs.SetPriority(ctx, id, 0)
	if err != nil || !ok || old != 5 {
		t.Fatalf("SetPriority: old=%d ok=%v err=%v", old, ok, err)
	}
	a.jobPriorityChanged(ctx, id, old, 0, "operator escalation")
	if j := mustJob(t, a, id); j.State == jobs.StateHeld {
		t.Fatalf("job still held after bump to P0: %+v", j)
	}
	// The task linkage is intact and the event is on the task thread.
	if got, ok, _ := a.tasks.TaskForJob(ctx, id); !ok || got != task.ID {
		t.Fatalf("task link lost: %q %v", got, ok)
	}
}

func TestSetJobPriorityPersistsWhenNotHeld(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	_, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "work", "priority": 5})
	id := out["job_id"].(string)
	if out["state"] == jobs.StateHeld {
		t.Fatalf("unexpectedly held: %v", out)
	}
	for _, p := range []int{3, 0} {
		if _, ok, err := a.jobs.SetPriority(ctx, id, p); err != nil || !ok {
			t.Fatalf("SetPriority(%d): ok=%v err=%v", p, ok, err)
		}
		o, set, _ := a.jobs.PriorityOverride(ctx, id)
		if !set || o != p {
			t.Fatalf("override = %d,%v want %d", o, set, p)
		}
	}
	if j := mustJob(t, a, id); j.Spec.Priority != 1 {
		t.Fatalf("spec priority = %d, want 1 (P0 clamps)", j.Spec.Priority)
	}
	if _, ok, _ := a.jobs.SetPriority(ctx, id, 10); ok {
		t.Fatal("priority 10 accepted")
	}
	if _, err := a.jobs.Cancel(ctx, id, jobs.StateCancelled, "x"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.jobs.SetPriority(ctx, id, 2); ok {
		t.Fatal("terminal job priority changed")
	}
}

func TestTaskPriorityChangeReleasesHeldJobs(t *testing.T) {
	ctx := context.Background()
	a, _ := newRedApp(t)
	task, _ := a.tasks.CreateTask(ctx, "normal", 2)
	id := fileHeldJob(t, a, task.ID)
	zero := 0
	if _, err := a.tasks.UpdateTask(ctx, task.ID, tasks.TaskUpdate{Priority: &zero}); err != nil {
		t.Fatal(err)
	}
	a.taskPriorityChanged(ctx, task.ID)
	if j := mustJob(t, a, id); j.State == jobs.StateHeld {
		t.Fatalf("job still held after task bump to P0: %+v", j)
	}
}
