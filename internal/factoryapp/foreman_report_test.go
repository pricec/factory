package factoryapp_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/foremanapp"
	"github.com/pricec/factory/internal/wire"
)

// promptRecorder replaces the planner's opencode session with a recorder
// that acks each wake with a brain.reply, mirroring the real planner.
type promptRecorder struct {
	mu      sync.Mutex
	prompts []string
}

func (r *promptRecorder) exec(e *testEnv) func(ctx context.Context, prompt string) (string, error) {
	return func(ctx context.Context, prompt string) (string, error) {
		r.mu.Lock()
		r.prompts = append(r.prompts, prompt)
		r.mu.Unlock()
		_, err := e.a.Inbox().Post(ctx, "planner:main", wire.MessageEnvelope{
			Type: "brain.reply", From: "factory:brain", Payload: []byte(`{"text":"ok"}`),
		})
		return "ok", err
	}
}

func (r *promptRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.prompts)
}

func (r *promptRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.prompts) == 0 {
		return ""
	}
	return r.prompts[len(r.prompts)-1]
}

// operatorEnvelopes returns planner:main's envelopes of the given type.
func operatorEnvelopes(t *testing.T, e *testEnv, typ string) []wire.MessageEnvelope {
	t.Helper()
	envs, err := e.a.Inbox().Recent(context.Background(), "planner:main", 100)
	if err != nil {
		t.Fatalf("planner:main recent: %v", err)
	}
	var out []wire.MessageEnvelope
	for _, env := range envs {
		if env.Type == typ {
			out = append(out, env)
		}
	}
	return out
}

// TestFactoryMainRelaysReportsAndWakes: foreman.report and brain.failed
// posted into factory:main are relayed verbatim into planner:main and wake
// the planner with a report-specific reason; an unhandled type is relayed
// as notify.unhandled and does not wake (spec §3.2, §9.5).
func TestFactoryMainRelaysReportsAndWakes(t *testing.T) {
	cases := []struct {
		name     string
		typ      string
		wantWake bool
		wantOut  string // operator-inbox type the relay produces
	}{
		{"foreman.report relays and wakes", "foreman.report", true, "foreman.report"},
		{"brain.failed relays and wakes", "brain.failed", true, "brain.failed"},
		{"unhandled type notifies, no wake", "mystery.type", false, "notify.unhandled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			rec := &promptRecorder{}
			e.a.SetPlannerExec(rec.exec(e))

			payload := []byte(`{"text":"toast task finished"}`)
			if _, err := e.a.Inbox().Post(context.Background(), "factory:main", wire.MessageEnvelope{
				Type: tc.typ, From: "foreman:alpha", JobID: "job_abc", Payload: payload,
			}); err != nil {
				t.Fatalf("post to factory:main: %v", err)
			}

			waitFor(t, "relay", 5*time.Second, func() bool {
				return len(operatorEnvelopes(t, e, tc.wantOut)) == 1
			})
			out := operatorEnvelopes(t, e, tc.wantOut)[0]
			if tc.wantOut == tc.typ {
				// A relay keeps the sender and job, and the payload
				// verbatim.
				if out.From != "foreman:alpha" || out.JobID != "job_abc" || string(out.Payload) != string(payload) {
					t.Fatalf("relayed %+v, want from foreman:alpha job job_abc payload %s", out, payload)
				}
			}

			if tc.wantWake {
				waitFor(t, "planner wake", 5*time.Second, func() bool { return rec.count() == 1 })
				if !strings.Contains(rec.last(), "foreman reports") {
					t.Errorf("wake reason lacks foreman report framing: %q", rec.last())
				}
				if !strings.Contains(rec.last(), "toast task finished") {
					t.Errorf("wake prompt lacks the report payload: %q", rec.last())
				}
				return
			}
			// No wake for informational notices: give the loop a beat, then
			// confirm the planner stayed asleep.
			time.Sleep(200 * time.Millisecond)
			if n := rec.count(); n != 0 {
				t.Fatalf("planner woke %d time(s) for %s, want none", n, tc.typ)
			}
			notice := operatorEnvelopes(t, e, "notify.unhandled")[0]
			var p map[string]any
			if err := json.Unmarshal(notice.Payload, &p); err != nil || p["type"] != "mystery.type" || p["from"] != "foreman:alpha" {
				t.Fatalf("notify.unhandled payload = %s (%v)", notice.Payload, err)
			}
		})
	}
}

// TestForemanReportEndToEnd: a foreman's report reaches the operator over
// the real path with no direct /v1/chat post - the foreman's poster (the
// parent forwarder's HTTP leg) posts to the factory with its paired bearer;
// the factory relays it into planner:main and wakes the planner
// (spec §7.3, §9.5 acceptance).
func TestForemanReportEndToEnd(t *testing.T) {
	e := newEnv(t)
	rec := &promptRecorder{}
	e.a.SetPlannerExec(rec.exec(e))
	bearer := e.pair("alpha", nil)

	poster := &foremanapp.Poster{FactoryURL: e.ts.URL, Bearer: bearer}
	if err := poster.PostToParent(context.Background(), wire.MessageEnvelope{
		Type:    "foreman.report",
		From:    "foreman:alpha",
		Payload: []byte(`{"text":"install finished on alpha"}`),
	}); err != nil {
		t.Fatalf("foreman report post: %v", err)
	}

	waitFor(t, "operator relay", 5*time.Second, func() bool {
		return len(operatorEnvelopes(t, e, "foreman.report")) == 1
	})
	waitFor(t, "planner wake", 5*time.Second, func() bool { return rec.count() == 1 })
	if !strings.Contains(rec.last(), "install finished on alpha") {
		t.Fatalf("wake prompt lacks the report: %q", rec.last())
	}
}
