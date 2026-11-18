package factoryapp_test

import (
	"net/http"
	"testing"
	"time"
)

// TestRetryAttemptResultIsNotDroppedAsDuplicate: a job whose first attempt
// failed is retried under the SAME job id; the retry attempt's result must
// reach the job rather than being rejected as a duplicate job.result in
// factory:main (which left the job running until reconcile reaped it with
// "foreman ... does not claim it").
func TestRetryAttemptResultIsNotDroppedAsDuplicate(t *testing.T) {
	e := newEnv(t)
	bearer := e.pair("alpha", nil)
	created := struct {
		JobID string `json:"job_id"`
	}{}
	if status := e.do("POST", "/v1/jobs", "", map[string]any{"task": "retry me"}, &created); status != http.StatusCreated {
		t.Fatalf("create status = %d", status)
	}
	post := func(typ string, payload map[string]any) int {
		return e.do("POST", "/v1/inboxes/factory:main/messages", bearer,
			map[string]any{"type": typ, "job_id": created.JobID, "from": "foreman:alpha", "payload": payload}, nil)
	}
	state := func() (string, int) {
		var job struct {
			State    string `json:"state"`
			Attempts int    `json:"attempts"`
		}
		e.do("GET", "/v1/jobs/"+created.JobID, "", nil, &job)
		return job.State, job.Attempts
	}

	if s := post("job.event", map[string]any{"event": "started"}); s != http.StatusCreated {
		t.Fatalf("started status = %d", s)
	}
	failed := map[string]any{
		"job_id": created.JobID, "status": "failed", "summary": "x",
		"error": map[string]any{"kind": "crash", "message": "Not logged in"},
	}
	if s := post("job.result", failed); s != http.StatusCreated {
		t.Fatalf("failed result status = %d", s)
	}
	waitFor(t, "failed with retry armed", 5*time.Second, func() bool { st, _ := state(); return st == "failed" })

	// Backoff elapses on the fake clock; the picker re-dispatches attempt 1.
	e.clock.Advance(time.Minute)
	waitFor(t, "retry dispatched", 15*time.Second, func() bool { st, a := state(); return st == "dispatched" && a == 1 })
	// Let the notifier post the retry's job.request (releases the slot).
	time.Sleep(2 * time.Second)

	if s := post("job.event", map[string]any{"event": "started"}); s != http.StatusCreated {
		t.Fatalf("retry started status = %d", s)
	}
	done := map[string]any{"job_id": created.JobID, "status": "done", "summary": "ok"}
	if s := post("job.result", done); s != http.StatusCreated {
		t.Fatalf("retry result status = %d, want 201 (dropped as duplicate?)", s)
	}
	waitFor(t, "done", 5*time.Second, func() bool { st, _ := state(); return st == "done" })
}
