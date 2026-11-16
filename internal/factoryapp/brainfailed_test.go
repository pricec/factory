package factoryapp_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// TestBrainFailedWakesPlanner: a foreman's brain.failed envelope (spec
// §3.2, 2026-11-15 amendment) is relayed by the factory:main consumer
// into the operator inbox and wakes the planner with the failure in its
// prompt - the delegation's death is never silent again. The planner
// then answers the operator itself (the stub records the wake; the
// prompt must carry the failure so the real planner could fall back to
// create_job).
func TestBrainFailedWakesPlanner(t *testing.T) {
	e := newEnv(t)
	alpha := e.pair("alpha", nil)

	var mu sync.Mutex
	var prompts []string
	e.a.SetPlannerExec(func(ctx context.Context, prompt string) (string, error) {
		mu.Lock()
		prompts = append(prompts, prompt)
		mu.Unlock()
		return "handled the failed delegation myself", nil
	})

	// The foreman posts its failure report to factory:main (foreman
	// tokens are post-only there, spec §7.3).
	payload, err := json.Marshal(map[string]string{
		"envelope_id":   "msg_nudge",
		"envelope_type": "chat.message",
		"error":         "brain session for chat.message msg_nudge: brain session exceeded 5m0s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status := e.postToFactoryMain(alpha, wire.MessageEnvelope{
		Type:    "brain.failed",
		JobID:   "job_probe",
		From:    "foreman:alpha",
		Payload: payload,
	}); status != 201 {
		t.Fatalf("post brain.failed status = %d, want 201", status)
	}

	// The consumer relays it into planner:main and the planner wakes.
	waitFor(t, "planner wake on brain.failed", 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(prompts) == 1
	})
	mu.Lock()
	prompt := prompts[0]
	mu.Unlock()
	for _, want := range []string{"brain session failed", "exceeded 5m0s", "create_job"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("planner prompt missing %q:\n%s", want, prompt)
		}
	}

	// The relayed copy is visible in the operator inbox (spec §9.5
	// replay): the operator sees what the planner saw.
	var envs []wire.MessageEnvelope
	if status := e.do("GET", "/v1/inboxes/planner:main/recent?limit=20", "", nil, &envs); status != 200 {
		t.Fatalf("planner:main recent status = %d", status)
	}
	found := false
	for _, env := range envs {
		if env.Type == "brain.failed" && env.From == "foreman:alpha" {
			found = true
		}
	}
	if !found {
		t.Errorf("relay missing from planner:main: %+v", envs)
	}
}
