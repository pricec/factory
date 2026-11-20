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
		{"20 turns, nothing summarized -> newest 10, fold", 20, "", 10, "m16"},
		{"summary covers all but 7 -> show 7, no fold", 20, "m12", 7, ""},
		{"summary covers everything -> keep recent 3", 20, "m19", 3, ""},
		{"stale summary id -> all unsummarized, capped", 20, "gone", 10, "m16"},
		{"9 unsummarized: below fold threshold", 20, "m10", 9, ""},
		{"10 unsummarized: fold", 20, "m09", 10, "m16"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shown, fold := standingView(msgs(tc.n), tc.through, 0)
			if len(shown) != tc.wantShown || fold != tc.wantFold {
				t.Fatalf("shown=%d fold=%q, want %d %q", len(shown), fold, tc.wantShown, tc.wantFold)
			}
		})
	}
}

// TestStandingViewConfigurable: the limit is a parameter, not hard-coded.
func TestStandingViewConfigurable(t *testing.T) {
	if shown, _ := standingView(msgs(30), "", 4); len(shown) != 4 {
		t.Errorf("turns=4 -> %d shown", len(shown))
	}
	if shown, _ := standingView(msgs(30), "", 0); len(shown) != tasks.DefaultStandingChatTurns {
		t.Errorf("default -> %d shown, want %d", len(shown), tasks.DefaultStandingChatTurns)
	}
	for _, tc := range []struct {
		cfg  int
		env  string
		want int
	}{{0, "", 10}, {0, "7", 7}, {5, "7", 5}, {0, "junk", 10}, {0, "-3", 10}} {
		if got := standingTurnsFrom(tc.cfg, tc.env); got != tc.want {
			t.Errorf("standingTurnsFrom(%d,%q) = %d, want %d", tc.cfg, tc.env, got, tc.want)
		}
	}
}

// TestStandingViewInternalRowsDontCount: alert rows never push turns out.
func TestStandingViewInternalRowsDontCount(t *testing.T) {
	var th []wire.MessageEnvelope
	for i := 0; i < 12; i++ {
		th = append(th, wire.MessageEnvelope{ID: fmt.Sprintf("t%02d", i), Type: "chat.message"})
		for j := 0; j < 5; j++ {
			th = append(th, wire.MessageEnvelope{ID: fmt.Sprintf("a%02d.%d", i, j), Type: alertRaisedType})
		}
	}
	shown, _ := standingView(th, "", 10)
	turns := 0
	for _, e := range shown {
		if e.Type == "chat.message" {
			turns++
		}
	}
	if turns != 10 || shown[0].ID != "t02" {
		t.Fatalf("turns=%d first=%s, want 10 turns starting at t02", turns, shown[0].ID)
	}
}

func TestStandingPromptBounded(t *testing.T) {
	w := PlannerWake{
		Reason: reasonStanding, Task: tasks.Task{ID: tasks.StandingID, Standing: true},
		Thread: msgs(20), Summary: "earlier: operator prefers terse answers",
	}
	p := buildStandingWakePrompt(w)
	for _, want := range []string{"STANDING factory chat", "earlier: operator prefers terse", "through_message_id=m16", "create_task", "recent chat: 10 message(s)"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if n := strings.Count(p, "--- message "); n != 10 {
		t.Errorf("rendered %d messages, want 10", n)
	}
}

// TestRunTaskWakeStandingWindow: only the last standing turns are
// rendered for a standing wake, and the stored summary rides along.
func TestRunTaskWakeStandingWindow(t *testing.T) {
	a := newInternalApp(t)
	ctx := context.Background()
	var last []wire.MessageEnvelope
	for i := 0; i < 40; i++ {
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
	if !strings.Contains(prompt, "SUMMARY-MARK") || !strings.Contains(prompt, "hello 39") {
		t.Errorf("prompt lacks the summary or newest message")
	}
	if strings.Contains(prompt, "hello 29\n") || strings.Contains(prompt, "hello 5\n") {
		t.Errorf("old message leaked into the window")
	}
	if n := strings.Count(prompt, "--- message "); n != tasks.DefaultStandingChatTurns {
		t.Errorf("rendered %d messages, want %d", n, tasks.DefaultStandingChatTurns)
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

func TestWindowStanding(t *testing.T) {
	var th []wire.MessageEnvelope
	for i := 0; i < 12; i++ {
		th = append(th, wire.MessageEnvelope{ID: fmt.Sprintf("t%02d", i), Type: "chat.message"},
			wire.MessageEnvelope{ID: fmt.Sprintf("a%02d", i), Type: alertRaisedType})
	}
	got, older := windowStanding(th, 10)
	if older != 2 || got[0].ID != "t02" || len(got) != 20 {
		t.Fatalf("older=%d first=%s len=%d", older, got[0].ID, len(got))
	}
	if got, older := windowStanding(th, 12); older != 0 || len(got) != len(th) {
		t.Fatalf("no-op window: older=%d len=%d", older, len(got))
	}
}
