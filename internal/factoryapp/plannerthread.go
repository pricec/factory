package factoryapp

import (
	"fmt"
	"strings"

	"github.com/pricec/factory/internal/llmctx"
	"github.com/pricec/factory/internal/wire"
)

// Thread rendering rules for planner wake prompts (spec §9.5.2). The
// planner must always see what the operator said and what it said back, so
// conversational rows are never cut where it matters:
//
//   - the newest plannerFullConv chat.message / brain.reply rows, and every
//     [new] trigger, are rendered in full;
//   - older conversational rows are capped at plannerConvCap;
//   - internal payloads (job notifications, results, reports) are capped at
//     plannerInternalCap;
//   - every truncation marker names the message id; the planner fetches the
//     full text with get_message;
//   - when the rendered thread exceeds plannerThreadBudget, the OLDEST
//     rows are dropped whole (protected rows - the full-text ones - are
//     never dropped or chopped).
const (
	plannerFullConv       = 10
	plannerConvCap        = 16 << 10
	plannerInternalCap    = 4 << 10
	plannerThreadBudget   = 64 << 10
	plannerRecoverFooter  = "Rows ending in \"…[truncated … get_message message_id=…]\" were cut to bound the prompt: call get_message with that id to read the full text."
	plannerOmittedNoteFmt = "(%d older message(s) omitted to bound the prompt; get_task lists the thread and get_message fetches any message by id)\n"
)

func isConversational(env wire.MessageEnvelope) bool {
	return env.Type == "chat.message" || env.Type == "brain.reply"
}

// renderThreadRows renders rows (oldest first) per the rules above and
// returns the text and the number of rows shown. Row numbers are the
// original positions so a drop never renumbers.
func renderThreadRows(rows []wire.MessageEnvelope, isTrigger map[string]bool) (string, int) {
	protected := make([]bool, len(rows))
	conv := 0
	for i := len(rows) - 1; i >= 0; i-- {
		if isTrigger[rows[i].ID] {
			protected[i] = true
		}
		if isConversational(rows[i]) {
			if conv < plannerFullConv {
				protected[i] = true
			}
			conv++
		}
	}
	parts := make([]string, len(rows))
	sizes := make([]int, len(rows))
	total := 0
	for i, env := range rows {
		limit := plannerInternalCap
		switch {
		case protected[i]:
			limit = 0
		case isConversational(env):
			limit = plannerConvCap
		}
		parts[i] = fmt.Sprintf("\n--- message %d%s ---\n%s", i+1, promptTags(env, isTrigger[env.ID]), llmctx.EnvelopeSnippetLimit(env, limit))
		sizes[i] = len(parts[i])
		total += sizes[i]
	}
	// Over budget: drop the oldest unprotected rows first.
	omitted := 0
	drop := make([]bool, len(rows))
	for i := 0; i < len(rows) && total > plannerThreadBudget; i++ {
		if protected[i] {
			continue
		}
		drop[i] = true
		total -= sizes[i]
		omitted++
	}
	var sb strings.Builder
	if omitted > 0 {
		fmt.Fprintf(&sb, plannerOmittedNoteFmt, omitted)
	}
	for i := range rows {
		if !drop[i] {
			sb.WriteString(parts[i])
		}
	}
	return sb.String(), len(rows) - omitted
}

// triggerCap is the payload cap for a [new] row outside the thread view:
// conversational rows in full, internal ones at plannerInternalCap.
func triggerCap(env wire.MessageEnvelope) int {
	if isConversational(env) {
		return 0
	}
	return plannerInternalCap
}
