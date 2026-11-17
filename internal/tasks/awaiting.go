package tasks

import (
	"sort"
	"time"
)

// Who a task is waiting on (the "needs input" signal of the operator UI).
const (
	AwaitingOperator = "operator"
	AwaitingFactory  = "factory"
)

// Awaiting reports who a task is waiting on. A task awaits the operator
// iff it is open AND its newest operator-visible thread message is from
// the planner/brain (type brain.reply). Operator-visible means
// chat.message or brain.reply; internal rows (foreman.report, notify.*,
// brain.failed, ...) are ignored. Otherwise (closed, no visible message,
// or the operator spoke last) it awaits the factory. lastVisibleType is
// "" when the thread has no operator-visible message.
func Awaiting(status, lastVisibleType string) string {
	if status == StatusOpen && lastVisibleType == TypeBrainReply {
		return AwaitingOperator
	}
	return AwaitingFactory
}

// WaitingItem is one task in the operator's triage queue.
type WaitingItem struct {
	ID       string
	Priority int
	Since    time.Time // ts of the brain reply the operator has not answered
}

// OrderWaiting sorts items in place into triage order: priority ascending
// (0 = most urgent), then oldest waiting first, then id for determinism.
func OrderWaiting(items []WaitingItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		return a.ID < b.ID
	})
}

// MergeQueue merges a freshly ordered waiting list (fresh) into the
// operator's current on-screen queue (prev) without disturbing it: ids in
// prev that are still waiting keep their place, ids no longer waiting
// drop out, and newly waiting ids are appended in fresh order, so a task
// that becomes waiting mid-session joins the queue instead of replacing
// the one on screen.
func MergeQueue(prev, fresh []string) []string {
	waiting := make(map[string]bool, len(fresh))
	for _, id := range fresh {
		waiting[id] = true
	}
	out := make([]string, 0, len(fresh))
	seen := make(map[string]bool, len(fresh))
	for _, id := range prev {
		if waiting[id] && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	for _, id := range fresh {
		if !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	return out
}

// Advance returns the id to show after the operator replied to (or
// skipped) current: the entry following current in queue, wrapping to the
// first other entry; "" when nothing else waits. If current is not in the
// queue (it was answered and dropped) the head of the queue is returned.
func Advance(queue []string, current string) string {
	idx := -1
	for i, id := range queue {
		if id == current {
			idx = i
			break
		}
	}
	if idx < 0 {
		if len(queue) > 0 {
			return queue[0]
		}
		return ""
	}
	for k := 1; k < len(queue); k++ {
		if id := queue[(idx+k)%len(queue)]; id != current {
			return id
		}
	}
	return ""
}
