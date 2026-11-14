// TestE2E is the final black-box proof (issue #22): one real factory + one
// real foreman + a fake opencode, driven end to end over the public HTTP and
// MCP surfaces. Subtests run sequentially against the shared boot; each
// scenario that needs state isolation creates its own scratch job (and,
// where a second credential would collide with the real foreman's, its own
// synthetic foreman credential).
package e2e_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

func TestE2E(t *testing.T) {
	s := boot(t)

	// The foreman registers (pairing + startup capabilities push) before
	// any dispatch can match it (spec §6.3, §6.7).
	waitFor(t, "foreman online", 5*time.Second, func() bool {
		var foremen []jobs.Foreman
		_ = s.do("GET", "/v1/foremen", "", nil, &foremen)
		for _, f := range foremen {
			if f.ID == foremanInbox && f.Online {
				return true
			}
		}
		return false
	})

	t.Run("HappyPath", func(t *testing.T) { testHappyPath(t, s) })
	t.Run("QuestionLoop", func(t *testing.T) { testQuestionLoop(t, s) })
	t.Run("DeadLetter", func(t *testing.T) { testDeadLetter(t, s) })
	t.Run("SummaryView", func(t *testing.T) { testSummaryView(t, s) })
}

// testHappyPath: create a job via the planner MCP create_job, let the real
// dispatch notifier route the job.request to the foreman, the spawner run
// the fake opencode, the harness post the fallback result, the foreman
// forward it byte-identical to factory:main, and the consumer apply it.
func testHappyPath(t *testing.T, s *stack) {
	ctx := s.ctx
	task := "e2e happy path: count the widgets"

	sc := s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "create_job", map[string]any{
		"task": task,
	})
	jobID, _ := sc["job_id"].(string)
	if !strings.HasPrefix(jobID, "job_") {
		t.Fatalf("create_job job_id = %v, want job_-prefixed", sc["job_id"])
	}
	if sc["state"] != "dispatched" {
		t.Fatalf("create_job state = %v, want dispatched", sc["state"])
	}
	if foreman, _ := sc["foreman"].(string); foreman != foremanInbox {
		t.Fatalf("create_job foreman = %v, want %s", sc["foreman"], foremanInbox)
	}
	s.happyJobID, s.happyTask = jobID, task

	// The whole stack processes it: notifier -> foreman client -> spawner
	// -> fake opencode -> harness fallback post -> local consumer forward
	// -> factory consumer apply.
	var job jobs.Job
	waitFor(t, "job done", 8*time.Second, func() bool {
		_ = s.do("GET", "/v1/jobs/"+jobID, "", nil, &job)
		return job.State == jobs.StateDone
	})
	if job.ForemanID != foremanInbox {
		t.Errorf("job foreman_id = %q, want %s", job.ForemanID, foremanInbox)
	}
	var results []wire.JobResult
	if err := json.Unmarshal(job.Results, &results); err != nil {
		t.Fatalf("parse results %s: %v", job.Results, err)
	}
	if len(results) != 1 {
		t.Fatalf("results has %d entries, want exactly 1: %s", len(results), job.Results)
	}
	if results[0].Summary != fakeSummary {
		t.Errorf("summary = %q, want the fake opencode stdout %q", results[0].Summary, fakeSummary)
	}
	if results[0].Status != wire.StatusDone {
		t.Errorf("status = %s, want done", results[0].Status)
	}

	// Worker ran: the spawner persisted the JobSpec and the harness wrote
	// the generated opencode config into the per-job directory (spec §8.2,
	// §8.4). (.result_posted is only written by report_result - the
	// primary path - which the pinned printf-only fake script does not
	// exercise; the fallback result is the pinned script's report.)
	jobDir := filepath.Join(s.foremanDataDir, "jobs", jobID)
	for _, name := range []string{"job.json", "opencode.json"} {
		if _, err := os.Stat(filepath.Join(jobDir, name)); err != nil {
			t.Errorf("worker artifact %s: %v", name, err)
		}
	}

	// Worker token invalidated on the terminal result (spec §7.4): the
	// bearer the spawner minted (captured via the BuildHarnessConfig
	// override) no longer identifies.
	bearer := s.workerBearer(jobID)
	if bearer == "" {
		t.Fatal("no worker bearer captured for the happy-path job")
	}
	waitFor(t, "worker token revoked", 5*time.Second, func() bool {
		_, _, err := s.foreman.Auth().Identify(ctx, bearer)
		var ue *auth.UnauthorizedError
		return err != nil && errors.As(err, &ue)
	})
	// ... and over the wire: the job inbox read with the revoked bearer
	// is a 401 (spec §10).
	if st := s.req(s.foremanSrv.URL, "GET", "/v1/inboxes/job:"+jobID+"/messages", bearer, nil, nil); st != http.StatusUnauthorized {
		t.Errorf("job inbox read with revoked worker bearer = %d, want 401", st)
	}

	// Spend endpoint counts the usage (spec §6.5): at least the happy
	// job was created today. The fallback result carries no usage, so
	// total_usd may be 0 - the job count is the assertion.
	day := time.Now().UTC().Format("2006-01-02")
	var totals struct {
		Total float64 `json:"total_usd"`
		Jobs  int     `json:"jobs"`
	}
	if st := s.do("GET", "/v1/spend?day="+day, "", nil, &totals); st != http.StatusOK {
		t.Fatalf("GET /v1/spend status = %d, want 200", st)
	}
	if totals.Jobs < 1 {
		t.Errorf("spend jobs = %d, want >= 1", totals.Jobs)
	}
	if totals.Total < 0 {
		t.Errorf("spend total_usd = %f, want >= 0", totals.Total)
	}
}

// testQuestionLoop: drive ask_parent as an MCP client with a minted worker
// token; answer via the mcpforeman answer_worker with the brain token;
// then the timeout default with no staged answer.
func testQuestionLoop(t *testing.T, s *stack) {
	ctx := s.ctx

	// Scratch job at the factory (Store Create is the pinned shortcut;
	// ask_parent only needs the job id + a worker credential).
	spec := wire.NewJobSpec("e2e question loop scratch", "factory")
	if err := s.factory.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	workerTok, err := s.foreman.Auth().MintWorkerToken(ctx, foremanInbox, spec.ID)
	if err != nil {
		t.Fatalf("MintWorkerToken: %v", err)
	}

	// Stage the answer first via answer_worker (mcpforeman, brain token):
	// it posts job.answer into the per-job inbox (spec §3.2, §9.3).
	sc := s.callTool(s.brainMCP(), s.foreman.BrainToken(), "answer_worker", map[string]any{
		"job_id": spec.ID,
		"answer": "use the blue one",
	})
	if sc["ok"] != true {
		t.Fatalf("answer_worker: %v, want ok:true", sc)
	}

	// ask_parent returns the staged answer (spec §9.2).
	sc = s.callTool(s.workerMCP(), workerTok, "ask_parent", map[string]any{
		"question":     "which config?",
		"wait_seconds": 5,
	})
	if sc["answer"] != "use the blue one" {
		t.Fatalf("ask_parent answer = %v, want the staged answer", sc["answer"])
	}

	// No staged answer + wait_seconds=1: the default answer on timeout
	// (spec §11), and the call returns near the 1s wait.
	start := time.Now()
	sc = s.callTool(s.workerMCP(), workerTok, "ask_parent", map[string]any{
		"question":     "anyone?",
		"wait_seconds": 1,
	})
	elapsed := time.Since(start)
	if sc["answer"] != "proceed with your best judgment" {
		t.Fatalf("ask_parent timeout answer = %v, want the default", sc["answer"])
	}
	if elapsed > 10*time.Second {
		t.Errorf("ask_parent timeout took %v, want ~1s", elapsed)
	}
}

// testDeadLetter: a validation-kind failure never retries - the job goes
// straight to dead, shows in /v1/deadletter, and requeue resets it to
// pending. The timed backoff is owned by the unit tests.
func testDeadLetter(t *testing.T, s *stack) {
	ctx := s.ctx

	// A second, synthetic foreman credential at the factory's own auth
	// store: post-only on factory:main (spec §7.3), so the test can drive
	// the factory:main consumer directly WITHOUT disturbing the real
	// foreman's paired.json credential (and without racing the real
	// foreman's worker against this job's terminal state).
	pairTok, _, err := s.factory.Auth().IssuePairingToken(ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	bravoBearer, _, err := s.factory.Auth().PairForeman(ctx, pairTok, "bravo")
	if err != nil {
		t.Fatalf("PairForeman: %v", err)
	}

	spec := wire.NewJobSpec("e2e dead-letter scratch", "factory")
	if err := s.factory.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Dispatched to the synthetic foreman: the job.request sits unconsumed
	// in its factory-side inbox; only the state machine matters here.
	if err := s.factory.Jobs().Dispatch(ctx, spec.ID, "foreman:bravo"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	post := func(env wire.MessageEnvelope) int {
		return s.req(s.factorySrv.URL, "POST", "/v1/inboxes/factory:main/messages", bravoBearer, env, nil)
	}
	from := "foreman:bravo"

	// started event: dispatched -> running (spec §6.2).
	if st := post(wire.MessageEnvelope{
		Type: "job.event", JobID: spec.ID, From: from,
		Payload: json.RawMessage(`{"event":"started"}`),
	}); st != http.StatusCreated {
		t.Fatalf("post job.event status = %d, want 201", st)
	}
	var job jobs.Job
	waitFor(t, "job running", 5*time.Second, func() bool {
		_ = s.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
		return job.State == jobs.StateRunning
	})

	// Terminal result with error kind validation (spec §5.2): the
	// consumer applies it (running -> failed), then MarkFailedForRetry
	// sees validation's 0 retries and dead-letters immediately (spec
	// §6.4) - no backoff wait to skip.
	if st := post(wire.MessageEnvelope{
		Type: "job.result", JobID: spec.ID, From: from,
		Payload: mustJSON(t, wire.JobResult{
			JobID:   spec.ID,
			Status:  wire.StatusFailed,
			Summary: "bad input",
			Error:   &wire.JobError{Kind: wire.KindValidation, Message: "widget count is not a number"},
		}),
	}); st != http.StatusCreated {
		t.Fatalf("post job.result status = %d, want 201", st)
	}
	waitFor(t, "job dead", 5*time.Second, func() bool {
		_ = s.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
		return job.State == jobs.StateDead
	})
	if job.Attempts != 0 {
		t.Errorf("dead job attempts = %d, want 0 (validation never retried)", job.Attempts)
	}
	if !strings.Contains(job.LastError, "widget count is not a number") {
		t.Errorf("last_error = %q, want the validation message", job.LastError)
	}
	var results []wire.JobResult
	if err := json.Unmarshal(job.Results, &results); err != nil || len(results) != 1 {
		t.Errorf("dead job results = %s (err %v), want 1 entry", job.Results, err)
	}

	// Dead-letter listing shows it (spec §6.9).
	var dead []jobs.Job
	if st := s.do("GET", "/v1/deadletter", "", nil, &dead); st != http.StatusOK {
		t.Fatalf("GET /v1/deadletter status = %d, want 200", st)
	}
	listed := false
	for _, d := range dead {
		if d.ID == spec.ID {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("job %s not in /v1/deadletter: %+v", spec.ID, dead)
	}

	// Requeue: dead -> pending with attempts reset (spec §6.4).
	var requeued jobs.Job
	if st := s.do("POST", "/v1/deadletter/"+spec.ID+"/requeue", "", nil, &requeued); st != http.StatusOK {
		t.Fatalf("POST requeue status = %d, want 200", st)
	}
	if requeued.State != jobs.StatePending || requeued.Attempts != 0 {
		t.Fatalf("requeue response = state %s attempts %d, want pending/0", requeued.State, requeued.Attempts)
	}
	_ = s.do("GET", "/v1/jobs/"+spec.ID, "", nil, &job)
	if job.State != jobs.StatePending || job.Attempts != 0 {
		t.Fatalf("requeued job = state %s attempts %d, want pending/0", job.State, job.Attempts)
	}
}

// testSummaryView: the planner's list view carries summaries only - never
// the results payload or the spec - and read_result serves the full
// payload (spec §6.6, §9.4).
func testSummaryView(t *testing.T, s *stack) {
	if s.happyJobID == "" {
		t.Fatal("scenario 1 did not run first; happy-path job missing")
	}

	entries := s.callToolArray(s.factoryMCP(), s.factory.PlannerToken(), "list_jobs", map[string]any{})
	var entry map[string]any
	for _, e := range entries {
		if e["id"] == s.happyJobID {
			entry = e
			break
		}
	}
	if entry == nil {
		t.Fatalf("done job %s not in list_jobs: %+v", s.happyJobID, entries)
	}
	if entry["state"] != "done" {
		t.Errorf("list state = %v, want done", entry["state"])
	}
	if entry["summary"] != fakeSummary {
		t.Errorf("list summary = %v, want %q", entry["summary"], fakeSummary)
	}
	if entry["over_budget"] != false {
		t.Errorf("list over_budget = %v, want false", entry["over_budget"])
	}
	// Summary-only projection: no results/spec keys, and neither the spec
	// task nor the results payload leaks into the entry (spec §9.4).
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal list entry: %v", err)
	}
	for _, leaked := range []string{"results", "spec", s.happyTask} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("list entry leaks %q (read_result-only per spec §9.4): %s", leaked, raw)
		}
	}
	for _, key := range []string{"id", "state", "summary", "over_budget"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("list entry missing %q: %s", key, raw)
		}
	}

	// read_result: the full payload (results array + spec), and only here.
	sc := s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "read_result", map[string]any{
		"job_id": s.happyJobID,
	})
	if sc["state"] != "done" {
		t.Errorf("read_result state = %v, want done", sc["state"])
	}
	resultsJSON, err := json.Marshal(sc["results"])
	if err != nil {
		t.Fatalf("marshal read_result results: %v", err)
	}
	if !strings.Contains(string(resultsJSON), fakeSummary) {
		t.Errorf("read_result results missing the summary: %s", resultsJSON)
	}
	var specOut wire.JobSpec
	specJSON, err := json.Marshal(sc["spec"])
	if err != nil {
		t.Fatalf("marshal read_result spec: %v", err)
	}
	if err := json.Unmarshal(specJSON, &specOut); err != nil {
		t.Fatalf("read_result spec not a JobSpec: %v", err)
	}
	if specOut.Task != s.happyTask {
		t.Errorf("read_result spec.Task = %q, want %q", specOut.Task, s.happyTask)
	}
}
