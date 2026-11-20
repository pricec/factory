package factoryapp

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// scheduledJob creates a schedule (notify flag as given) under a new task and
// a job as if the scheduler had fired it; returns the job and task ids.
func scheduledJob(t *testing.T, a *App, notify bool) (jobID, taskID string) {
	t.Helper()
	ctx := context.Background()
	task, err := a.tasks.CreateTask(ctx, "Daily report", tasks.DefaultPriority)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := wire.NewJobSpec("write the daily report", "factory")
	tmpl.Tier, tmpl.TimeoutS = wire.TierDefault, 300
	sch, err := a.jobs.CreateScheduleNotify(ctx, "0 7 * * *", tmpl, task.ID, notify)
	if err != nil {
		t.Fatal(err)
	}
	spec := tmpl
	spec.ID = wire.NewJobID()
	spec.CreatedAt = a.jobs.Now().Format("2006-01-02T15:04:05Z07:00")
	if err := a.jobs.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := a.jobs.DB().ExecContext(ctx,
		`INSERT INTO schedule_runs (schedule_id, run_at, job_id) VALUES (?, ?, ?)`, sch.ID, spec.CreatedAt, spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.tasks.LinkJob(ctx, task.ID, spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.jobs.Dispatch(ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatal(err)
	}
	if err := a.jobs.ApplyEvent(ctx, spec.ID, "started"); err != nil {
		t.Fatal(err)
	}
	return spec.ID, task.ID
}

func standingReplies(t *testing.T, a *App) []wire.MessageEnvelope {
	t.Helper()
	all, err := a.inbox.Recent(context.Background(), auth.PlannerMainInbox, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []wire.MessageEnvelope
	for _, e := range all {
		if e.Type == tasks.TypeBrainReply && e.ThreadID == tasks.StandingID {
			out = append(out, e)
		}
	}
	return out
}

func TestScheduleSummaryPostedToStanding(t *testing.T) {
	ctx := context.Background()

	t.Run("done", func(t *testing.T) {
		a := newInternalApp(t)
		jobID, taskID := scheduledJob(t, a, true)
		if err := a.jobs.ApplyResult(ctx, wire.JobResult{JobID: jobID, Status: "done", Summary: "3 jobs ran, all green"}); err != nil {
			t.Fatal(err)
		}
		got := standingReplies(t, a)
		if len(got) != 1 {
			t.Fatalf("standing replies = %d, want 1", len(got))
		}
		p := string(got[0].Payload)
		for _, want := range []string{"Scheduled run done", "Daily report", taskID, "3 jobs ran, all green", `"spoken"`} {
			if !strings.Contains(p, want) {
				t.Errorf("payload %s lacks %q", p, want)
			}
		}
		if strings.Contains(p, "FAILED") {
			t.Errorf("done run marked failed: %s", p)
		}
	})

	t.Run("failed", func(t *testing.T) {
		a := newInternalApp(t)
		jobID, taskID := scheduledJob(t, a, true)
		if err := a.jobs.ApplyResult(ctx, wire.JobResult{
			JobID: jobID, Status: "failed", Summary: "gave up",
			Error: &wire.JobError{Kind: "validation", Message: "bad input"},
		}); err != nil {
			t.Fatal(err)
		}
		// A failed job retries first: nothing is posted until it is dead.
		if got := standingReplies(t, a); len(got) != 0 {
			t.Fatalf("posted before dead: %d", len(got))
		}
		if err := a.jobs.MarkDead(ctx, jobID); err != nil {
			t.Fatal(err)
		}
		got := standingReplies(t, a)
		if len(got) != 1 {
			t.Fatalf("standing replies = %d, want 1", len(got))
		}
		p := string(got[0].Payload)
		for _, want := range []string{"Scheduled run FAILED", taskID, "gave up"} {
			if !strings.Contains(p, want) {
				t.Errorf("payload %s lacks %q", p, want)
			}
		}
	})

	t.Run("opt out", func(t *testing.T) {
		a := newInternalApp(t)
		jobID, _ := scheduledJob(t, a, false)
		if err := a.jobs.ApplyResult(ctx, wire.JobResult{JobID: jobID, Status: "done", Summary: "quiet"}); err != nil {
			t.Fatal(err)
		}
		if got := standingReplies(t, a); len(got) != 0 {
			t.Fatalf("opted-out schedule posted %d", len(got))
		}
	})

	t.Run("unscheduled job", func(t *testing.T) {
		a := newInternalApp(t)
		spec := wire.NewJobSpec("one off", "factory")
		spec.Tier, spec.TimeoutS = wire.TierDefault, 300
		spec.CreatedAt = a.jobs.Now().Format("2006-01-02T15:04:05Z07:00")
		if err := a.jobs.Create(ctx, spec); err != nil {
			t.Fatal(err)
		}
		_ = a.jobs.Dispatch(ctx, spec.ID, "foreman:alpha")
		_ = a.jobs.ApplyEvent(ctx, spec.ID, "started")
		if err := a.jobs.ApplyResult(ctx, wire.JobResult{JobID: spec.ID, Status: "done", Summary: "x ok"}); err != nil {
			t.Fatal(err)
		}
		if got := standingReplies(t, a); len(got) != 0 {
			t.Fatalf("non-scheduled job posted %d", len(got))
		}
	})
}

func TestScheduleNotifyToggle(t *testing.T) {
	a := newInternalApp(t)
	ctx := context.Background()
	tmpl := wire.NewJobSpec("t", "factory")
	tmpl.Tier, tmpl.TimeoutS = wire.TierDefault, 300
	sch, err := a.jobs.CreateScheduleForTask(ctx, "0 7 * * *", tmpl, "")
	if err != nil || !sch.NotifyStanding {
		t.Fatalf("default NotifyStanding = %v, err %v; want true", sch.NotifyStanding, err)
	}
	if err := a.jobs.SetScheduleNotifyStanding(ctx, sch.ID, false); err != nil {
		t.Fatal(err)
	}
	all, _ := a.jobs.Schedules(ctx)
	if len(all) != 1 || all[0].NotifyStanding {
		t.Fatalf("after disable: %+v", all)
	}
	if err := a.jobs.SetScheduleNotifyStanding(ctx, "nope", true); err == nil {
		t.Fatal("unknown id: want error")
	}
	_ = jobs.ErrScheduleNotFound
}

func TestScheduleSummaryTextBounds(t *testing.T) {
	long := strings.Repeat("word ", 400)
	text, spoken := scheduleSummaryText("Daily report", "task_1", "job_1", true, "dead", long, "boom")
	if !strings.HasPrefix(text, "Scheduled run FAILED") || !strings.Contains(text, "task_1") {
		t.Errorf("text = %q", text)
	}
	if n := utf8.RuneCountInString(spoken); n > scheduleSpokenChars {
		t.Errorf("spoken %d runes > %d", n, scheduleSpokenChars)
	}
	if strings.Contains(spoken, "task_1") || strings.Contains(spoken, "job_1") {
		t.Errorf("spoken carries ids: %q", spoken)
	}
	if !strings.Contains(spoken, "failed") {
		t.Errorf("spoken = %q", spoken)
	}
}
