package factoryapp

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pricec/factory/internal/llmctx"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// Standing Planner chat context bounds. The chat is permanent, so a wake
// never replays its history: the prompt carries the planner-maintained
// running summary of older messages plus at most N recent TURNS (N =
// the standing-chat turn limit, tasks.DefaultStandingChatTurns unless
// overridden; see standingTurns). A turn is an operator-visible message
// (chat.message / brain.reply); internal rows (alerts, job news) ride along
// with the turns they sit among but never count toward, or push out, the
// limit. Recent turns the summary already covers are dropped (keeping
// standingKeepRecent(N) for conversational continuity).

// standingFetchCap is how many raw thread rows a standing wake reads:
// generous enough that internal rows cannot crowd the last N turns out.
func standingFetchCap(turns int) int { return max(40, 6*turns) }

// standingKeepRecent: turns left unfolded when the planner folds the summary.
func standingKeepRecent(turns int) int { return max(1, turns*3/10) }

// standingTurns normalises a configured limit (<= 0 -> the default).
func standingTurns(n int) int {
	if n <= 0 {
		return tasks.DefaultStandingChatTurns
	}
	return n
}

// standingView splits the fetched thread (oldest first) for the prompt:
// shown = messages to render (at most `turns` turns plus the internal rows
// after the oldest of them), foldThrough = the id the planner should fold
// the summary up to ("" = no fold needed this wake).
func standingView(thread []wire.MessageEnvelope, through string, turns int) (shown []wire.MessageEnvelope, foldThrough string) {
	turns = standingTurns(turns)
	keep := standingKeepRecent(turns)
	start := 0
	for i, e := range thread {
		if through != "" && e.ID == through {
			start = i + 1
		}
	}
	// idxs = indexes of unsummarized turns; fall back to the last `keep`
	// turns overall when the summary covers (nearly) everything.
	turnIdx := func(from int) []int {
		var out []int
		for i := from; i < len(thread); i++ {
			if !tasks.IsInternal(thread[i].Type) {
				out = append(out, i)
			}
		}
		return out
	}
	idxs := turnIdx(start)
	from := start
	if len(idxs) < keep {
		all := turnIdx(0)
		if len(all) > keep {
			from = all[len(all)-keep]
		} else {
			from = 0
		}
	} else if len(idxs) > turns {
		from = idxs[len(idxs)-turns]
	}
	shown = thread[from:]
	if len(idxs) >= turns {
		// Fold everything but the newest `keep` turns.
		cut := idxs[len(idxs)-keep]
		if cut > 0 {
			foldThrough = thread[cut-1].ID
		}
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

	shown, fold := standingView(w.Thread, w.SummaryThrough, w.StandingTurns)
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
			"(old summary + the messages above except the newest %d turns; keep decisions, task ids, open questions) and through_message_id=%s.\n", standingKeepRecent(standingTurns(w.StandingTurns)), fold)
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
- For code and build jobs, leave timeout_s unset (the tier ceiling applies); never set timeout_s below 1800 when builds or tests run.
- Handle the [new] messages with your tools, then finish; do not repeat work. Keep token use low: prefer targeted reads over broad ones.
`

// EnvStandingChatTurns overrides the standing-chat turn limit.
const EnvStandingChatTurns = "FACTORY_STANDING_CHAT_TURNS"

// standingTurnsFrom resolves the limit: config, then env, then the default.
func standingTurnsFrom(cfgValue int, envValue string) int {
	if cfgValue > 0 {
		return cfgValue
	}
	if n, err := strconv.Atoi(strings.TrimSpace(envValue)); err == nil && n > 0 {
		return n
	}
	return tasks.DefaultStandingChatTurns
}
