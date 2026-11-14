package factoryapp_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// waitFor helper re-exported for use here (defined in factoryapp_test.go).

// TestCreateJobEndpoint: POST /v1/jobs drives the same path as the MCP
// create_job tool - spend breaker, capability match, create, dispatch.
func TestCreateJobEndpoint(t *testing.T) {
	e := newEnv(t)

	// No matching foreman yet: job is created and left pending.
	var created struct {
		JobID   string `json:"job_id"`
		State   string `json:"state"`
		Foreman any    `json:"foreman"`
	}
	body := map[string]any{"task": "summarize the logs", "requires": []string{"gpu"}}
	if status := e.do("POST", "/v1/jobs", "", body, &created); status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", status)
	}
	if created.JobID == "" || created.State != "pending" || created.Foreman != "" {
		t.Fatalf("created = %+v, want pending job with empty foreman", created)
	}
	var job jobs.Job
	if status := e.do("GET", "/v1/jobs/"+created.JobID, "", nil, &job); status != http.StatusOK {
		t.Fatalf("get job status = %d", status)
	}
	if job.State != "pending" || job.Spec.Task != "summarize the logs" {
		t.Fatalf("job = %+v", job)
	}

	// Pair a foreman with matching capabilities: next create dispatches.
	e.pair("alpha", map[string]string{"gpu": "a100"})
	created = struct {
		JobID   string `json:"job_id"`
		State   string `json:"state"`
		Foreman any    `json:"foreman"`
	}{}
	if status := e.do("POST", "/v1/jobs", "", body, &created); status != http.StatusCreated {
		t.Fatalf("create w/ match status = %d, want 201", status)
	}
	if created.State != "dispatched" || created.Foreman != "foreman:alpha" {
		t.Fatalf("created = %+v, want dispatched to foreman:alpha", created)
	}
	// The dispatch notifier must have posted a job.request into the
	// foreman's inbox (spec §7.3).
	waitFor(t, "job.request in foreman inbox", 5*time.Second, func() bool {
		var envs []wire.MessageEnvelope
		if status := e.do("GET", "/v1/inboxes/foreman:alpha/recent?limit=10", "", nil, &envs); status != http.StatusOK {
			return false
		}
		for _, env := range envs {
			if env.Type == "job.request" && env.JobID == created.JobID {
				return true
			}
		}
		return false
	})

	// Bad bodies: 400 with the error model.
	for _, tt := range []struct {
		name string
		in   any
	}{
		{"not json", "{"},
		{"missing task", map[string]any{"task": ""}},
		{"bad priority", map[string]any{"task": "x", "priority": 99}},
		{"bad tier", map[string]any{"task": "x", "tier": "turbo"}},
	} {
		var errResp map[string]any
		if status := e.do("POST", "/v1/jobs", "", tt.in, &errResp); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tt.name, status)
		}
	}

	// Tier pass-through (spec §5.1, §6.5): `tier: "hardest"` rides the
	// job.request to the foreman.
	hardest := map[string]any{"task": "refactor the kernel", "tier": "hardest"}
	if status := e.do("POST", "/v1/jobs", "", hardest, &created); status != http.StatusCreated {
		t.Fatalf("hardest-tier create status = %d, want 201", status)
	}
	if status := e.do("GET", "/v1/jobs/"+created.JobID, "", nil, &job); status != http.StatusOK {
		t.Fatalf("get hardest job status = %d", status)
	}
	if job.Spec.Tier != wire.TierHardest {
		t.Errorf("hardest job tier = %q, want %q", job.Spec.Tier, wire.TierHardest)
	}
}

// TestMessageForemanEndpoint: POST /v1/foremen/{id}/messages posts from the
// planner identity into the foreman's inbox, same as the MCP tool.
func TestMessageForemanEndpoint(t *testing.T) {
	e := newEnv(t)

	// Table-driven validation failures.
	for _, tt := range []struct {
		name string
		body any
	}{
		{"not json", "{"},
		{"missing type", map[string]any{"x": 1}},
		{"job-scoped without job_id", map[string]any{"type": "job.event"}},
	} {
		var errResp map[string]any
		if status := e.do("POST", "/v1/foremen/alpha/messages", "", tt.body, &errResp); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tt.name, status)
		}
	}

	var env wire.MessageEnvelope
	body := map[string]any{"type": "brain.ping", "payload": map[string]any{"q": "status?"}}
	if status := e.do("POST", "/v1/foremen/alpha/messages", "", body, &env); status != http.StatusCreated {
		t.Fatalf("post status = %d, want 201", status)
	}
	if env.From != "factory:planner" || env.Type != "brain.ping" {
		t.Fatalf("posted envelope = %+v", env)
	}
	if !isEnvelopeIn(t, e, "foreman:alpha", env.ID) {
		t.Fatalf("message %s not stored in foreman:alpha inbox", env.ID)
	}
}

func isEnvelopeIn(t *testing.T, e *testEnv, inboxID, mid string) bool {
	t.Helper()
	var envs []wire.MessageEnvelope
	if status := e.do("GET", "/v1/inboxes/"+inboxID+"/recent?limit=50", "", nil, &envs); status != http.StatusOK {
		t.Fatalf("recent status = %d", status)
	}
	for _, env := range envs {
		if env.ID == mid {
			return true
		}
	}
	return false
}

// TestCreateJobSchedule: schedule_cron on POST /v1/jobs ALSO creates a
// schedule with the same template (spec §9.4).
func TestCreateJobSchedule(t *testing.T) {
	e := newEnv(t)
	var created struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	body := map[string]any{"task": "nightly sweep", "schedule_cron": "0 3 * * *"}
	if status := e.do("POST", "/v1/jobs", "", body, &created); status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", status)
	}
	var scheds []struct {
		ID       string          `json:"id"`
		CronSpec string          `json:"cron_spec"`
		Template json.RawMessage `json:"job_template"`
	}
	if status := e.do("GET", "/v1/schedules", "", nil, &scheds); status != http.StatusOK {
		t.Fatalf("schedules status = %d", status)
	}
	if len(scheds) != 1 || scheds[0].CronSpec != "0 3 * * *" {
		t.Fatalf("schedules = %+v, want one 0 3 * * *", scheds)
	}
	var tmpl wire.JobSpec
	if err := json.Unmarshal(scheds[0].Template, &tmpl); err != nil {
		t.Fatalf("template: %v", err)
	}
	if tmpl.Task != "nightly sweep" {
		t.Fatalf("template task = %q", tmpl.Task)
	}
}
