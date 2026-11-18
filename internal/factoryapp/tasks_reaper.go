package factoryapp

import (
	"context"
	"log"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
)

// Task reaper constants (spec §9.5.2; operator DECISION 3).
const (
	// taskIdleClose is how long a task must sit idle - no thread message
	// and no task update - before the reaper closes it.
	taskIdleClose = 24 * time.Hour
	// taskReapInterval is the reaper's ticker period.
	taskReapInterval = 10 * time.Minute
	// taskIdleReason is the close_reason the reaper stamps.
	taskIdleReason = "idle-24h"
)

// taskReaper is the auto-close backstop (spec §9.5.2): an open task idle
// for taskIdleClose (last_activity_at = newest thread message or task
// update) with no non-terminal job and no planner session running or
// queued is closed with close_reason "idle-24h". The planner normally
// closes tasks itself; this catches the ones it leaves behind.
type taskReaper struct {
	tasks *tasks.Store
	jobs  *jobs.Store
	// busy reports a task with a planner session running or queued; such
	// a task is never reaped mid-session. nil = never busy.
	busy func(taskID string) bool
	// changed is told about every task the reaper closes (task.changed).
	changed func(ctx context.Context, taskID string)
	// Now is the injectable clock; nil = time.Now.
	Now func() time.Time
	// Idle overrides taskIdleClose when > 0 (tests).
	Idle time.Duration
}

func (r *taskReaper) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run ticks every interval until ctx is done, reaping on each tick.
func (r *taskReaper) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.Reap(ctx); err != nil && ctx.Err() == nil {
				log.Printf("factoryapp: task reaper: %v", err)
			}
		}
	}
}

// Reap runs one pass and returns the ids of the tasks it closed.
func (r *taskReaper) Reap(ctx context.Context) ([]string, error) {
	idle := r.Idle
	if idle <= 0 {
		idle = taskIdleClose
	}
	open, err := r.tasks.ListTasks(ctx, tasks.TaskFilter{Status: tasks.StatusOpen})
	if err != nil {
		return nil, err
	}
	cutoff := r.now().Add(-idle)
	var closed []string
	for _, t := range open {
		if t.Standing {
			continue // the standing chat is never reaped
		}
		if t.LastActivityAt.After(cutoff) {
			continue
		}
		if r.busy != nil && r.busy(t.ID) {
			continue
		}
		live, err := r.hasLiveJob(ctx, t.JobIDs)
		if err != nil {
			log.Printf("factoryapp: task reaper: jobs of %s: %v", t.ID, err)
			continue
		}
		if live {
			continue
		}
		status, reason := tasks.StatusClosed, taskIdleReason
		if _, err := r.tasks.UpdateTask(ctx, t.ID, tasks.TaskUpdate{Status: &status, CloseReason: &reason}); err != nil {
			log.Printf("factoryapp: task reaper: close %s: %v", t.ID, err)
			continue
		}
		log.Printf("factoryapp: task reaper: closed %s (%q) idle since %s", t.ID, t.Title, t.LastActivityAt.UTC().Format(time.RFC3339))
		closed = append(closed, t.ID)
		if r.changed != nil {
			r.changed(ctx, t.ID)
		}
	}
	return closed, nil
}

// hasLiveJob reports whether any of ids is a known, non-terminal job. A
// job id unknown to the store counts as terminal (it can never progress).
func (r *taskReaper) hasLiveJob(ctx context.Context, ids []string) (bool, error) {
	for _, id := range ids {
		job, ok, err := r.jobs.Get(ctx, id)
		if err != nil {
			return false, err
		}
		if ok && !jobs.Terminal(job.State) {
			return true, nil
		}
	}
	return false, nil
}
