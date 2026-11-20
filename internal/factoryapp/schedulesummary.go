package factoryapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// Caps for the standing-chat summary of a scheduled run.
const (
	scheduleSummaryChars = 400 // text: result summary
	scheduleSpokenChars  = 300 // spoken: same cap as brain.reply `spoken`
)

// postScheduleSummary posts ONE brain.reply into the standing Planner chat
// when a job fired by a schedule reaches a terminal state, so the operator
// sees scheduled results without opening the run's task (spec §6.8). Opt-out
// per schedule (notify_standing). Cancelled and superseded runs are the
// operator's own doing and post nothing; failed runs are marked FAILED. It
// never wakes the planner (a brain.reply does not) and never errors.
func (a *App) postScheduleSummary(ctx context.Context, jobID, finalState string) {
	var failed bool
	switch finalState {
	case jobs.StateDone:
	case jobs.StateDead, jobs.StateTimedOut:
		failed = true
	default:
		return
	}
	sch, ok, err := a.jobs.ScheduleForJob(ctx, jobID)
	if err != nil {
		log.Printf("factoryapp: schedule summary: lookup %s: %v", jobID, err)
		return
	}
	if !ok || !sch.NotifyStanding || a.knownTask(ctx, tasks.StandingID) == "" {
		return
	}
	job, ok, err := a.jobs.Get(ctx, jobID)
	if err != nil || !ok {
		return
	}
	taskID := sch.TaskID
	if t := a.jobThread(ctx, jobID); t != "" {
		taskID = t
	}
	title := ""
	if taskID != "" {
		if t, err := a.tasks.GetTask(ctx, taskID); err == nil {
			title = t.Title
		}
	}
	if title == "" {
		title = clipRunes(strings.TrimSpace(strings.SplitN(job.Spec.Task, "\n", 2)[0]), 60)
	}
	text, spoken := scheduleSummaryText(title, taskID, jobID, failed, finalState, lastJobSummary(job.Results), job.LastError)
	data, _ := json.Marshal(map[string]string{"text": text, "spoken": spoken})
	if _, err := a.postToTask(ctx, tasks.StandingID, wire.MessageEnvelope{
		Type:    tasks.TypeBrainReply,
		From:    "factory",
		JobID:   jobID,
		Payload: data,
	}); err != nil {
		log.Printf("factoryapp: schedule summary for %s: %v", jobID, err)
	}
}

// scheduleSummaryText renders the chat text and the short spoken line.
func scheduleSummaryText(title, taskID, jobID string, failed bool, state, summary, lastErr string) (text, spoken string) {
	mark, verb := "Scheduled run done", "finished"
	if failed {
		mark, verb = "Scheduled run FAILED", "failed"
	}
	detail := strings.TrimSpace(summary)
	if failed {
		if e := strings.TrimSpace(lastErr); e != "" {
			detail = strings.TrimSpace(detail + " (" + e + ")")
		}
		if detail == "" {
			detail = "state " + state
		}
	}
	if detail == "" {
		detail = "no summary reported"
	}
	detail = clipRunes(strings.Join(strings.Fields(detail), " "), scheduleSummaryChars)
	ref := "job " + jobID
	if taskID != "" {
		ref = "task " + taskID
	}
	text = fmt.Sprintf("%s: %s (%s)\n%s", mark, title, ref, detail)
	spoken = clipRunes(fmt.Sprintf("Scheduled run %s %s. %s", title, verb, detail), scheduleSpokenChars)
	return text, spoken
}

// clipRunes truncates s to max runes with an ellipsis.
func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}
