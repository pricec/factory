package factoryapp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

func msgs(n int) []wire.MessageEnvelope {
	out := make([]wire.MessageEnvelope, n)
	for i := range out {
		out[i] = wire.MessageEnvelope{ID: fmt.Sprintf("m%02d", i), Type: "chat.message", Payload: []byte(`{"text":"t"}`)}
	}
	return out
}

func TestStandingView(t *testing.T) {
	tests := []struct {
		name      string
		n         int
		through   string
		wantShown int
		wantFold  string
	}{
		{"short, no summary", 3, "", 3, ""},
		{"window full, nothing summarized -> fold", 20, "", 20, "m13"},
		{"summary covers all but 7 -> show 7, no fold", 20, "m12", 7, ""},
		{"summary covers everything -> keep recent 6", 20, "m19", 6, ""},
		{"stale summary id outside window -> all unsummarized", 20, "gone", 20, "m13"},
		{"13 unsummarized: below fold threshold", 20, "m06", 13, ""},
		{"14 unsummarized: fold", 20, "m05", 14, "m13"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shown, fold := standingView(msgs(tc.n), tc.through)
			if len(shown) != tc.wantShown || fold != tc.wantFold {
				t.Fatalf("shown=%d fold=%q, want %d %q", len(shown), fold, tc.wantShown, tc.wantFold)
			}
		})
	}
}

func TestStandingPromptBounded(t *testing.T) {
	w := PlannerWake{
		Reason: reasonStanding, Task: tasks.Task{ID: tasks.StandingID, Standing: true},
		Thread: msgs(20), Summary: "earlier: operator prefers terse answers",
	}
	p := buildStandingWakePrompt(w)
	for _, want := range []string{"STANDING factory chat", "earlier: operator prefers terse", "through_message_id=m13", "create_task"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if n := strings.Count(p, "--- message "); n != 20 {
		t.Errorf("rendered %d messages, want 20", n)
	}
}

// TestRunTaskWakeStandingWindow: only the last standingWindow messages are
// read for a standing wake, and the stored summary rides along.
func TestRunTaskWakeStandingWindow(t *testing.T) {
	a := newInternalApp(t)
	ctx := context.Background()
	var last []wire.MessageEnvelope
	for i := 0; i < standingWindow+10; i++ {
		env, err := a.postToTask(ctx, tasks.StandingID, wire.MessageEnvelope{
			Type: "chat.message", From: "human:operator", Payload: []byte(fmt.Sprintf(`{"text":"hello %d"}`, i)),
		})
		if err != nil {
			t.Fatal(err)
		}
		last = []wire.MessageEnvelope{env}
	}
	if err := a.tasks.SetStandingSummary(ctx, "SUMMARY-MARK", ""); err != nil {
		t.Fatal(err)
	}
	var prompt string
	a.SetPlannerExec(func(_ context.Context, p string) (string, error) { prompt = p; return "", nil })
	if err := a.runTaskWake(ctx, tasks.StandingID, last); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "SUMMARY-MARK") || !strings.Contains(prompt, "hello 29") {
		t.Errorf("prompt lacks the summary or newest message")
	}
	if strings.Contains(prompt, "hello 5") || strings.Contains(prompt, "hello 9") {
		t.Errorf("old message leaked into the window")
	}
}

func TestReaperSkipsStanding(t *testing.T) {
	a := newInternalApp(t)
	ctx := context.Background()
	r := &taskReaper{tasks: a.tasks, jobs: a.jobs, Now: func() time.Time { return time.Now().Add(1000 * time.Hour) }}
	got, err := r.Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range got {
		if id == tasks.StandingID {
			t.Fatal("reaper closed the standing chat")
		}
	}
	if st, _ := a.tasks.GetTask(ctx, tasks.StandingID); st.Status != tasks.StatusOpen {
		t.Fatalf("standing status = %q", st.Status)
	}
}
