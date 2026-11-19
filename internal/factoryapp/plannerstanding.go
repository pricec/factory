package factoryapp

import (
	"fmt"
	"strings"

	"github.com/pricec/factory/internal/llmctx"
	"github.com/pricec/factory/internal/wire"
)

// Standing Planner chat context bounds. The chat is permanent, so a wake
// never replays its history: the prompt carries the planner-maintained
// running summary of older messages plus at most standingWindow recent
// ones, and drops the recent ones the summary already covers (keeping
// standingKeepRecent for conversational continuity).
const (
	standingWindow     = 20
	standingKeepRecent = 6
	// standingFoldAt: when this many shown messages are not yet in the
	// summary, the prompt asks the planner to fold the older ones in.
	standingFoldAt = 14
)

// standingView splits the fetched thread (oldest first) for the prompt:
// shown = messages to render, foldThrough = the id the planner should
// fold the summary up to ("" = no fold needed this wake).
func standingView(thread []wire.MessageEnvelope, through string) (shown []wire.MessageEnvelope, foldThrough string) {
	start := 0
	for i, e := range thread {
		if through != "" && e.ID == through {
			start = i + 1
		}
	}
	shown = thread[start:]
	if len(shown) < standingKeepRecent {
		shown = thread[max(0, len(thread)-standingKeepRecent):]
	}
	if len(thread)-start >= standingFoldAt {
		// Fold everything but the newest standingKeepRecent messages.
		foldThrough = thread[len(thread)-standingKeepRecent-1].ID
	}
	return shown, foldThrough
}

// buildStandingWakePrompt renders the wake prompt for the standing chat.
func buildStandingWakePrompt(w PlannerWake) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are the factory planner, in the STANDING factory chat (id %s): the operator's permanent, single back-and-forth with you, not tied to any unit of work. %s\n", w.Task.ID, w.Reason)

	sb.WriteString(deviceSection(w.Client))
	sb.WriteString("\n=== operator priority criteria ===\n")
	if strings.TrimSpace(w.Criteria) == "" {
		sb.WriteString("(none set; record them with set_priority_criteria when the operator states some)\n")
	} else {
		sb.WriteString(string(llmctx.Cap([]byte(w.Criteria))) + "\n")
	}

	sb.WriteString("\n=== summary of older chat messages (maintained by you) ===\n")
	if strings.TrimSpace(w.Summary) == "" {
		sb.WriteString("(none yet)\n")
	} else {
		sb.WriteString(w.Summary + "\n")
	}

	shown, fold := standingView(w.Thread, w.SummaryThrough)
	isTrigger := map[string]bool{}
	for _, e := range w.Triggers {
		isTrigger[e.ID] = true
	}
	fmt.Fprintf(&sb, "\n=== recent chat: %d message(s), oldest first; [new] rows woke you ===\n", len(shown))
	if len(shown) == 0 {
		sb.WriteString("(empty)\n")
	}
	inThread := map[string]bool{}
	for _, env := range w.Thread {
		inThread[env.ID] = true
	}
	for i, env := range shown {
		fmt.Fprintf(&sb, "\n--- message %d%s ---\n%s", i+1, promptTags(env, isTrigger[env.ID]), llmctx.EnvelopeSnippet(env))
	}
	for _, env := range w.Triggers {
		if !inThread[env.ID] {
			fmt.Fprintf(&sb, "\n--- new message%s ---\n%s", promptTags(env, true), llmctx.EnvelopeSnippet(env))
		}
	}
	sb.WriteString(standingWakeFooter)
	if hasType(w.Triggers, alertRaisedType) {
		sb.WriteString(alertWakePrompt)
	}
	if fold != "" {
		fmt.Fprintf(&sb, "\nBefore you finish, fold the older messages into the summary: call update_standing_summary with a merged, compact summary "+
			"(old summary + the messages above except the newest %d; keep decisions, task ids, open questions) and through_message_id=%s.\n", standingKeepRecent, fold)
	}
	return sb.String()
}

const standingWakeFooter = `
Rules for this chat:
- Answer status questions ("where is task X", "what's held", "what merged today") by using the read tools (list_tasks, get_task, list_jobs, read_result, list_foremen, merge queue and alert tools); do not guess from memory.
- Reply to the operator ONLY with notify_operator (it defaults to this chat). Be brief.
- This chat owns no jobs and can never be closed. When the operator asks for real work, call create_task for it (or route it to an EXISTING task by using that task's id), then create_job with that task_id, and tell the operator the task id. If the new work continues an existing task but has a new goal, pass follow_up_of=that task to create_task. create_job without a task_id is refused here.
- You may act directly when asked: cancel_job, reprioritise (update_task, set_priority_criteria), request_merge, message_foreman, raise_alert.
- Every code job you create MUST carry touches: the narrowest repo-relative path globs it will edit; use ["*"] only for exclusivity.
- Handle the [new] messages with your tools, then finish; do not repeat work. Keep token use low: prefer targeted reads over broad ones.
`
