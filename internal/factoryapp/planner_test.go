package factoryapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// TestChatEndpoint: POST /v1/chat stores the operator's message as a
// chat.message envelope in planner:main (spec §9.5).
func TestChatEndpoint(t *testing.T) {
	e := newEnv(t)

	var env wire.MessageEnvelope
	if status := e.do("POST", "/v1/chat", "", map[string]any{"text": "what's running?"}, &env); status != http.StatusCreated {
		t.Fatalf("chat status = %d, want 201", status)
	}
	if env.Type != "chat.message" || env.From != "human:operator" {
		t.Fatalf("envelope = %+v", env)
	}
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil || p.Text != "what's running?" {
		t.Fatalf("payload = %s (%v)", env.Payload, err)
	}
	// Visible in the chat log via the (unauthenticated) recent endpoint.
	var envs []wire.MessageEnvelope
	if status := e.do("GET", "/v1/inboxes/planner:main/recent", "", nil, &envs); status != http.StatusOK {
		t.Fatalf("recent status = %d", status)
	}
	if len(envs) != 1 || envs[0].ID != env.ID {
		t.Fatalf("planner:main recent = %+v", envs)
	}

	// Validation: empty text and bad JSON -> 400.
	var errResp map[string]any
	if status := e.do("POST", "/v1/chat", "", map[string]any{"text": ""}, &errResp); status != http.StatusBadRequest {
		t.Fatalf("empty text status = %d, want 400", status)
	}
	if status := e.do("POST", "/v1/chat", "", "{", &errResp); status != http.StatusBadRequest {
		t.Fatalf("bad json status = %d, want 400", status)
	}
}

// TestPlannerLoop: chat.message wakes the planner brain with the inbox
// history replayed into the prompt; brain replies and informational
// envelopes are acked without a wake (spec §9.5).
func TestPlannerLoop(t *testing.T) {
	e := newEnv(t)

	var mu sync.Mutex
	var prompts []string
	e.a.SetPlannerExec(func(ctx context.Context, prompt string) (string, error) {
		mu.Lock()
		prompts = append(prompts, prompt)
		mu.Unlock()
		// The real brain would call notify_operator; emulate the reply.
		_, err := e.a.Inbox().Post(ctx, "planner:main", wire.MessageEnvelope{
			Type:    "brain.reply",
			From:    "factory:brain",
			Payload: []byte(`{"text":"nothing running yet"}`),
		})
		return "ok", err
	})

	if status := e.do("POST", "/v1/chat", "", map[string]any{"text": "status?"}, nil); status != http.StatusCreated {
		t.Fatalf("chat status = %d", status)
	}

	waitFor(t, "brain wake", 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(prompts) == 1
	})
	mu.Lock()
	p := prompts[0]
	mu.Unlock()
	if !strings.Contains(p, "factory planner") {
		t.Errorf("prompt missing planner role: %q", snippetOf(p, 120))
	}
	if !strings.Contains(p, "status?") {
		t.Errorf("prompt missing the chat text: %q", snippetOf(p, 200))
	}

	// Reply lands in the chat log.
	waitFor(t, "brain.reply in planner:main", 5*time.Second, func() bool {
		var envs []wire.MessageEnvelope
		if status := e.do("GET", "/v1/inboxes/planner:main/recent?limit=20", "", nil, &envs); status != http.StatusOK {
			return false
		}
		for _, env := range envs {
			if env.Type == "brain.reply" {
				return true
			}
		}
		return false
	})

	// The brain must NOT wake again for the informational brain.reply.
	mu.Lock()
	n := len(prompts)
	mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != n {
		t.Errorf("brain re-woke for informational envelope: %d -> %d wakes", n, len(prompts))
	}
}

// TestPlannerBrainError: a failed brain session posts a brain.reply error
// note so the operator is never left waiting (spec §9.5).
func TestPlannerBrainError(t *testing.T) {
	e := newEnv(t)
	e.a.SetPlannerExec(func(ctx context.Context, prompt string) (string, error) {
		return "", context.DeadlineExceeded
	})

	if status := e.do("POST", "/v1/chat", "", map[string]any{"text": "hello?"}, nil); status != http.StatusCreated {
		t.Fatalf("chat status = %d", status)
	}
	waitFor(t, "error reply", 5*time.Second, func() bool {
		var envs []wire.MessageEnvelope
		if status := e.do("GET", "/v1/inboxes/planner:main/recent?limit=20", "", nil, &envs); status != http.StatusOK {
			return false
		}
		for _, env := range envs {
			if env.Type == "brain.reply" && strings.Contains(string(env.Payload), "could not run") {
				return true
			}
		}
		return false
	})
}

// TestNotifyJobOnTerminal: a job reaching done posts a notify.job envelope
// into planner:main (spec §9.5).
func TestNotifyJobOnTerminal(t *testing.T) {
	e := newEnv(t)

	// Pair first so create dispatches; event -> running; result -> done
	// (terminal) -> onTerminal -> notify.job.
	bearer := e.pair("alpha", nil)
	created := struct {
		JobID string `json:"job_id"`
	}{}
	if status := e.do("POST", "/v1/jobs", "", map[string]any{"task": "notify me when done"}, &created); status != http.StatusCreated {
		t.Fatalf("create status = %d", status)
	}
	if status := e.do("POST", "/v1/inboxes/factory:main/messages", bearer,
		map[string]any{
			"type": "job.event", "job_id": created.JobID, "from": "foreman:alpha",
			"payload": map[string]any{"event": "started"},
		}, nil); status != http.StatusCreated {
		t.Fatalf("post event status = %d", status)
	}
	body := map[string]any{
		"type": "job.result", "job_id": created.JobID, "from": "foreman:alpha",
		"payload": map[string]any{
			"job_id": created.JobID, "status": "done", "summary": "did the thing",
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "cost_usd": 0.001},
		},
	}
	if status := e.do("POST", "/v1/inboxes/factory:main/messages", bearer, body, nil); status != http.StatusCreated {
		t.Fatalf("post result status = %d", status)
	}

	waitFor(t, "notify.job in planner:main", 5*time.Second, func() bool {
		var envs []wire.MessageEnvelope
		if status := e.do("GET", "/v1/inboxes/planner:main/recent?limit=20", "", nil, &envs); status != http.StatusOK {
			return false
		}
		for _, env := range envs {
			if env.Type == "notify.job" && strings.Contains(string(env.Payload), created.JobID) &&
				strings.Contains(string(env.Payload), "done") {
				return true
			}
		}
		return false
	})
}

// TestNotifyJobOnCancel: a job cancelled through the cancel API posts a
// notify.job envelope with the resulting state into planner:main (spec
// §9.5, 2026-11-15 amendment) - the operator sees cancellations like any
// other terminal transition. No pairing: the job stays pending, which is
// the incident path the cancel API exists for.
func TestNotifyJobOnCancel(t *testing.T) {
	e := newEnv(t)

	created := struct {
		JobID string `json:"job_id"`
	}{}
	if status := e.do("POST", "/v1/jobs", "", map[string]any{"task": "duplicate job"}, &created); status != http.StatusCreated {
		t.Fatalf("create status = %d", status)
	}
	if status := e.do("POST", "/v1/jobs/"+created.JobID+"/cancel", "",
		map[string]any{"reason": "duplicate", "terminal_state": "superseded"}, nil); status != http.StatusOK {
		t.Fatalf("cancel status = %d", status)
	}

	waitFor(t, "notify.job (superseded) in planner:main", 5*time.Second, func() bool {
		var envs []wire.MessageEnvelope
		if status := e.do("GET", "/v1/inboxes/planner:main/recent?limit=20", "", nil, &envs); status != http.StatusOK {
			return false
		}
		for _, env := range envs {
			if env.Type == "notify.job" && strings.Contains(string(env.Payload), created.JobID) &&
				strings.Contains(string(env.Payload), "superseded") {
				return true
			}
		}
		return false
	})
}

func snippetOf(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
