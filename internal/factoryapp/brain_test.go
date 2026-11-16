package factoryapp_test

import (
	"encoding/json"
	"net/http"
	"strings"
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
		JobID    string   `json:"job_id"`
		State    string   `json:"state"`
		Foreman  any      `json:"foreman"`
		Warnings []string `json:"warnings"`
	}
	body := map[string]any{"task": "summarize the logs", "requires": []string{"gpu"}}
	if status := e.do("POST", "/v1/jobs", "", body, &created); status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", status)
	}
	if created.JobID == "" || created.State != "pending" || created.Foreman != "" {
		t.Fatalf("created = %+v, want pending job with empty foreman", created)
	}
	// No foremen registered: requires cannot be validated - the 201 body
	// carries a loud warning (spec §6.7, 2026-11-15 amendment).
	if len(created.Warnings) == 0 {
		t.Fatalf("warnings = %v, want a loud warning", created.Warnings)
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
		JobID    string   `json:"job_id"`
		State    string   `json:"state"`
		Foreman  any      `json:"foreman"`
		Warnings []string `json:"warnings"`
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

	// Requires validation (spec §6.7, 2026-11-15 amendment): alpha is
	// registered with gpu only, so capability values like "linux" name no
	// registered tag key - 400 unknown_requires, nothing created.
	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	status := e.do("POST", "/v1/jobs", "", map[string]any{"task": "x", "requires": []string{"linux"}}, &errResp)
	if status != http.StatusBadRequest {
		t.Fatalf("values-as-names requires status = %d, want 400: %+v", status, errResp)
	}
	if errResp.Error.Code != "unknown_requires" {
		t.Errorf("code = %q, want unknown_requires (%+v)", errResp.Error.Code, errResp)
	}
	if !strings.Contains(errResp.Error.Message, "linux") {
		t.Errorf("message = %q, want mention of the offending entry", errResp.Error.Message)
	}

	// name:value form rides the real matcher: gpu=a100 satisfies gpu:a100.
	created = struct {
		JobID    string   `json:"job_id"`
		State    string   `json:"state"`
		Foreman  any      `json:"foreman"`
		Warnings []string `json:"warnings"`
	}{}
	valueForm := map[string]any{"task": "constrain by value", "requires": []string{"gpu:a100"}}
	if status := e.do("POST", "/v1/jobs", "", valueForm, &created); status != http.StatusCreated {
		t.Fatalf("name:value create status = %d, want 201", status)
	}
	if created.State != "dispatched" || created.Foreman != "foreman:alpha" {
		t.Fatalf("created = %+v, want dispatched to foreman:alpha", created)
	}
	if len(created.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for validated requires", created.Warnings)
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
	// The endpoint resolves {id} against the foremen registry; pairing
	// registers the foreman (factoryapp handlePair upserts by identity).
	e.pair("alpha", nil)

	// Table-driven validation failures.
	for _, tt := range []struct {
		name    string
		path    string
		body    any
		wantSt  int
		wantMsg string
	}{
		{"not json", "/v1/foremen/alpha/messages", "{", http.StatusBadRequest, ""},
		{"missing type", "/v1/foremen/alpha/messages", map[string]any{"payload": map[string]any{"q": 1}}, http.StatusBadRequest, "type"},
		{"job-scoped without job_id", "/v1/foremen/alpha/messages", map[string]any{"type": "job.event", "payload": map[string]any{}}, http.StatusBadRequest, "job_id"},
		{"missing payload", "/v1/foremen/alpha/messages", map[string]any{"type": "note"}, http.StatusBadRequest, "payload"},
		{"null payload", "/v1/foremen/alpha/messages", map[string]any{"type": "note", "payload": nil}, http.StatusBadRequest, "payload"},
		{"unknown foreman", "/v1/foremen/ghost/messages", map[string]any{"type": "note", "payload": map[string]any{}}, http.StatusNotFound, "unknown_foreman"},
		{"double-prefixed id", "/v1/foremen/foreman:foreman:alpha/messages", map[string]any{"type": "note", "payload": map[string]any{}}, http.StatusNotFound, "unknown_foreman"},
	} {
		var errResp map[string]any
		if status := e.do("POST", tt.path, "", tt.body, &errResp); status != tt.wantSt {
			t.Errorf("%s: status = %d, want %d: %v", tt.name, status, tt.wantSt, errResp)
			continue
		}
		if tt.wantMsg != "" {
			raw, _ := json.Marshal(errResp["error"])
			if !strings.Contains(string(raw), tt.wantMsg) {
				t.Errorf("%s: error = %s, want %q mention", tt.name, raw, tt.wantMsg)
			}
		}
	}

	// Bare id and full id address the SAME inbox (never a double-prefixed
	// phantom); the body lands in foreman:alpha both times.
	for _, path := range []string{"/v1/foremen/alpha/messages", "/v1/foremen/foreman:alpha/messages"} {
		var env wire.MessageEnvelope
		body := map[string]any{"type": "brain.ping", "payload": map[string]any{"q": "status?"}}
		if status := e.do("POST", path, "", body, &env); status != http.StatusCreated {
			t.Fatalf("post %s status = %d, want 201", path, status)
		}
		if env.From != "factory:planner" || env.Type != "brain.ping" {
			t.Fatalf("posted envelope = %+v", env)
		}
		if !isEnvelopeIn(t, e, "foreman:alpha", env.ID) {
			t.Fatalf("message %s not stored in foreman:alpha inbox", env.ID)
		}
	}
	var phantom []wire.MessageEnvelope
	if status := e.do("GET", "/v1/inboxes/foreman:foreman:alpha/recent?limit=50", "", nil, &phantom); status != http.StatusOK || len(phantom) != 0 {
		t.Errorf("foreman:foreman:alpha inbox: status=%d n=%d - a double-prefixed id must never open one", status, len(phantom))
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
