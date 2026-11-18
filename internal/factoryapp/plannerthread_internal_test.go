package factoryapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

func tmsg(id, typ, text string) wire.MessageEnvelope {
	p, _ := json.Marshal(map[string]string{"text": text})
	return wire.MessageEnvelope{ID: id, Type: typ, From: "x", Payload: p}
}

func wakePrompt(thread []wire.MessageEnvelope, trig ...wire.MessageEnvelope) string {
	return buildTaskWakePrompt(PlannerWake{Reason: "r", Task: tasks.Task{ID: "task_1"}, Thread: thread, Triggers: trig})
}

func TestThreadLongReplyRenderedInFull(t *testing.T) {
	reply := "1. first\n" + strings.Repeat("filler ", 3000) + "\n2. SECOND-ITEM-END"
	op := tmsg("m2", "chat.message", "#2 is the priority")
	p := wakePrompt([]wire.MessageEnvelope{tmsg("m1", "brain.reply", reply), op}, op)
	if !strings.Contains(p, "SECOND-ITEM-END") || strings.Contains(p, "truncated, ") {
		t.Fatalf("long brain.reply truncated (len %d)", len(p))
	}
}

func TestThreadInternalPayloadsCapped(t *testing.T) {
	big := tmsg("j1", "notify.job", strings.Repeat("y", 20000))
	op := tmsg("m2", "chat.message", "hi")
	p := wakePrompt([]wire.MessageEnvelope{big, op}, op)
	if !strings.Contains(p, "truncated, ") || !strings.Contains(p, "get_message message_id=j1") {
		t.Fatalf("internal payload not capped with id marker")
	}
	if len(p) > 12000 {
		t.Fatalf("prompt len %d, internal payload not capped", len(p))
	}
}

func TestThreadBudgetDropsOldestFirst(t *testing.T) {
	var thread []wire.MessageEnvelope
	for i := 0; i < 40; i++ {
		thread = append(thread, tmsg(fmt.Sprintf("old%d", i), "chat.message", strings.Repeat("o", 3000)))
	}
	last := tmsg("last", "brain.reply", "NEWEST-REPLY-"+strings.Repeat("z", 30000)+"-TAIL")
	op := tmsg("op", "chat.message", "go")
	thread = append(thread, last, op)
	p := wakePrompt(thread, op)
	if !strings.Contains(p, "-TAIL") || !strings.Contains(p, "older message(s) omitted") {
		t.Fatalf("recent reply lost or no omission note")
	}
	if strings.Contains(p, "message 1 ") || !strings.Contains(p, "--- message 42 [new] ---") {
		t.Fatalf("oldest not dropped / numbering wrong")
	}
	if len(p) > plannerThreadBudget+40000 {
		t.Fatalf("prompt len %d unbounded", len(p))
	}
}
