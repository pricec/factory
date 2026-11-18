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
	// Advance the fake clock past the 30s backoff until the picker has
	// re-dispatched attempt 1 (state is only observable by polling).
	var st string
	var att int
	waitFor(t, "retry dispatched (see last state in log)", 60*time.Second, func() bool {
		e.clock.Advance(time.Minute)
		st, att = state()
		return att == 1 && (st == "dispatched" || st == "running")
	})
	t.Logf("retry state=%s attempts=%d", st, att)
	// The notifier releases the slot when it posts the retry's job.request;
	// the picker has already moved the job, so poll the result post below.

	// A real foreman only reports started after it received job.request, so
	// give the notifier (500ms tick) time to post it - and release the slot -
	// while the job is still dispatched.
	time.Sleep(3 * time.Second)
	var started int
	waitFor(t, "retry started accepted", 30*time.Second, func() bool {
		started = post("job.event", map[string]any{"event": "started"})
		st, _ := state()
		return started == http.StatusCreated && st == "running"
	})
	done := map[string]any{"job_id": created.JobID, "status": "done", "summary": "ok"}
	var last int
	waitFor(t, "retry result accepted (last status shown on failure)", 30*time.Second, func() bool {
		last = post("job.result", done)
		return last == http.StatusCreated
	})
	waitFor(t, "done", 30*time.Second, func() bool { st, _ := state(); return st == "done" })
}
