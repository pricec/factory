// TestE2E is the final black-box proof (issue #22): one real factory + one
// real foreman + a fake opencode, driven end to end over the public HTTP and
// MCP surfaces. Subtests run sequentially against the shared boot; each
// scenario that needs state isolation creates its own scratch job (and,
// where a second credential would collide with the real foreman's, its own
// synthetic foreman credential).
package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/models"
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
	t.Run("CancelAPI", func(t *testing.T) { testCancelAPI(t, s) })
	t.Run("ModelOverride", func(t *testing.T) { testModelOverride(t, s) })
	t.Run("SettingsPanel", func(t *testing.T) { testSettingsPanel(t, s) })
	t.Run("BrainFailedFeedback", func(t *testing.T) { testBrainFailedFeedback(t, s) })
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

// testCancelAPI: the cancel/supersede surface (2026-11-15 amendment, spec
// §6.9): a scratch pending job cancels over HTTP into the cancelled
// terminal state, the MCP cancel_job tool reports the same transition, a
// second cancel is rejected 409, and the terminal notify.job reaches the
// operator inbox. No foreman-kill exists in v1, so this drives only
// factory-side state.
func testCancelAPI(t *testing.T, s *stack) {
	ctx := s.ctx

	// Scratch pending job: created directly on the store (the
	// testDeadLetter pattern for jobs whose lifecycle the test controls).
	spec := wire.NewJobSpec("e2e cancel scratch", "factory")
	if err := s.factory.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// HTTP cancel: pending -> cancelled (200 + job record).
	var cancelled jobs.Job
	if st := s.do("POST", "/v1/jobs/"+spec.ID+"/cancel", "",
		map[string]any{"reason": "duplicate dispatch", "terminal_state": "superseded"}, &cancelled); st != http.StatusOK {
		t.Fatalf("POST cancel status = %d, want 200", st)
	}
	if cancelled.State != jobs.StateSuperseded {
		t.Fatalf("cancelled job state = %s, want superseded", cancelled.State)
	}
	if !strings.Contains(cancelled.LastError, "duplicate dispatch") {
		t.Errorf("last_error = %q, want the reason", cancelled.LastError)
	}
	var got jobs.Job
	_ = s.do("GET", "/v1/jobs/"+spec.ID, "", nil, &got)
	if got.State != jobs.StateSuperseded {
		t.Fatalf("GET after cancel: state = %s, want superseded", got.State)
	}

	// Cancelling a terminal job is rejected (409 already_terminal).
	var conflict map[string]any
	if st := s.do("POST", "/v1/jobs/"+spec.ID+"/cancel", "", nil, &conflict); st != http.StatusConflict {
		t.Fatalf("re-cancel status = %d, want 409: %v", st, conflict)
	}

	// The terminal transition notified the operator (spec §9.5).
	waitFor(t, "notify.job (superseded) in planner:main", 5*time.Second, func() bool {
		var envs []wire.MessageEnvelope
		if status := s.do("GET", "/v1/inboxes/planner:main/recent?limit=50", "", nil, &envs); status != http.StatusOK {
			return false
		}
		for _, env := range envs {
			if env.Type == "notify.job" && strings.Contains(string(env.Payload), spec.ID) &&
				strings.Contains(string(env.Payload), "superseded") {
				return true
			}
		}
		return false
	})

	// MCP tool: cancel_job on a fresh pending job surfaces the state; the
	// already-terminal job is a tool error naming the state.
	fresh := wire.NewJobSpec("e2e cancel scratch 2", "factory")
	if err := s.factory.Jobs().Create(ctx, fresh); err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	sc := s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "cancel_job",
		map[string]any{"job_id": fresh.ID})
	if sc["state"] != jobs.StateCancelled {
		t.Fatalf("cancel_job state = %v, want cancelled", sc["state"])
	}
	_, out := s.rpc(s.factoryMCP(), s.factory.PlannerToken(), "tools/call", map[string]any{
		"name":      "cancel_job",
		"arguments": map[string]any{"job_id": fresh.ID},
	})
	res, _ := out["result"].(map[string]any)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("cancel_job on terminal job: want tool error, got %v", out)
	}
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), "already terminal") || !strings.Contains(string(raw), "cancelled") {
		t.Errorf("tool error should name the terminal state: %s", raw)
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

// testModelOverride: an explicit model rides the whole chain over the
// public surfaces - create (both the MCP create_job tool and its POST
// /v1/jobs UI twin) -> dispatch notifier -> foreman spawner -> the
// harness config the worker runs with - and overrides the tier mapping
// there (spec §5.1, §6.5, 2026-11-15 amendments).
func testModelOverride(t *testing.T, s *stack) {
	// MCP path: an explicit ollama model on a tier:"hardest" job - the
	// model must win over the tier's pinned GLM default.
	sc := s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "create_job", map[string]any{
		"task": "e2e model override via mcp", "tier": "hardest", "model": "ollama/qwen3:8b",
	})
	mcpJobID, _ := sc["job_id"].(string)
	if !strings.HasPrefix(mcpJobID, "job_") {
		t.Fatalf("create_job job_id = %v, want job_-prefixed", sc["job_id"])
	}
	if sc["state"] != "dispatched" {
		t.Fatalf("create_job state = %v, want dispatched", sc["state"])
	}
	var job jobs.Job
	waitFor(t, "foreman spawned the mcp job", 5*time.Second, func() bool {
		_ = s.do("GET", "/v1/jobs/"+mcpJobID, "", nil, &job)
		return s.harnessModel(mcpJobID) != ""
	})
	if got := s.harnessModel(mcpJobID); got != "ollama/qwen3:8b" {
		t.Errorf("harness model = %q, want the explicit %q (tier mapping must not win)", got, "ollama/qwen3:8b")
	}
	if job.Spec.Model != "ollama/qwen3:8b" || job.Spec.Tier != "hardest" {
		t.Errorf("stored spec = model %q tier %q, want model ollama/qwen3:8b tier hardest", job.Spec.Model, job.Spec.Tier)
	}

	// UI twin: POST /v1/jobs with a bare model (no tier) dispatches the
	// same way.
	var created struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if st := s.do("POST", "/v1/jobs", "", map[string]any{
		"task": "e2e model override via ui", "model": "openrouter/z-ai/glm-4.7-flash",
	}, &created); st != http.StatusCreated {
		t.Fatalf("POST /v1/jobs status = %d, want 201", st)
	}
	waitFor(t, "foreman spawned the ui job", 5*time.Second, func() bool {
		return s.harnessModel(created.JobID) != ""
	})
	if got := s.harnessModel(created.JobID); got != "openrouter/z-ai/glm-4.7-flash" {
		t.Errorf("harness model = %q, want the explicit %q", got, "openrouter/z-ai/glm-4.7-flash")
	}
}

// testSettingsPanel: the role-default settings survive over the public
// surfaces end to end - POST /v1/settings stores them, the foreman's
// pull applies them (within its refresh interval) so no-model jobs spawn
// with the panel's default, and clearing reverts to the pinned default
// (spec §6.5, §6.9, 2026-11-15 amendments). The BuildHarnessConfig
// seam doubles as the observation point: probe it until the pull has
// landed, then create the job - no dispatch race.
func testSettingsPanel(t *testing.T, s *stack) {
	// Sanity: the default view is all pinned defaults.
	var view struct {
		Roles []struct {
			Role         string `json:"role"`
			Model        string `json:"model"`
			DefaultModel string `json:"default_model"`
			Source       string `json:"source"`
		} `json:"roles"`
	}
	if st := s.do("GET", "/v1/settings", "", nil, &view); st != http.StatusOK {
		t.Fatalf("GET /v1/settings status = %d, want 200", st)
	}
	for _, r := range view.Roles {
		if r.Role == "worker_flash" && (r.Source != "default" || r.Model != r.DefaultModel) {
			t.Fatalf("initial worker_flash = %+v, want the pinned default", r)
		}
	}

	// The harness-config seam observes the foreman's current tier
	// resolution (the same seam the harnessModels capture rides).
	probe := wire.NewJobSpec("settings probe", "e2e")
	resolvedFlash := func() string {
		return s.foreman.BuildHarnessConfig(probe, "bearer").Model
	}

	// Save a flash default via the panel's save path; wait until the
	// foreman's pull has applied it, then spawn a no-model, no-tier job.
	const panelModel = "ollama/gemma3:4b"
	if st := s.do("POST", "/v1/settings", "", map[string]string{"worker_flash": panelModel}, &view); st != http.StatusOK {
		t.Fatalf("POST /v1/settings status = %d, want 200", st)
	}
	waitFor(t, "foreman applied the panel default", 5*time.Second, func() bool {
		return resolvedFlash() == panelModel
	})
	var created struct {
		JobID string `json:"job_id"`
	}
	if st := s.do("POST", "/v1/jobs", "", map[string]any{"task": "e2e settings panel default"}, &created); st != http.StatusCreated {
		t.Fatalf("POST /v1/jobs status = %d, want 201", st)
	}
	waitFor(t, "foreman spawned the settings job", 5*time.Second, func() bool {
		return s.harnessModel(created.JobID) != ""
	})
	if got := s.harnessModel(created.JobID); got != panelModel {
		t.Errorf("no-model job harness model = %q, want the panel default %q", got, panelModel)
	}

	// Clearing reverts to the pinned default on the next pull.
	if st := s.do("POST", "/v1/settings", "", map[string]string{"worker_flash": ""}, &view); st != http.StatusOK {
		t.Fatalf("POST /v1/settings clear status = %d, want 200", st)
	}
	for _, r := range view.Roles {
		if r.Role == "worker_flash" && r.Source != "default" {
			t.Fatalf("cleared worker_flash = %+v, want source default", r)
		}
	}
	waitFor(t, "pinned default restored", 5*time.Second, func() bool {
		return resolvedFlash() == models.WorkerFlash
	})
	if st := s.do("POST", "/v1/jobs", "", map[string]any{"task": "e2e settings panel cleared"}, &created); st != http.StatusCreated {
		t.Fatalf("POST /v1/jobs (cleared) status = %d, want 201", st)
	}
	waitFor(t, "foreman spawned the cleared-defaults job", 5*time.Second, func() bool {
		return s.harnessModel(created.JobID) != ""
	})
	if got := s.harnessModel(created.JobID); got != models.WorkerFlash {
		t.Errorf("cleared-default job harness model = %q, want the pinned default %q", got, models.WorkerFlash)
	}
}

// testBrainFailedFeedback: the 2026-11-15 amendment's reason for being,
// end to end over the public surfaces. A planner nudge delegated to the
// foreman's brain dies (the brain session fails) - and unlike the silent
// dead-end this replaces, the foreman reports brain.failed to the
// factory, the consumer relays it into the operator inbox, and the
// factory planner wakes with the failure in its prompt so it can handle
// the request itself and tell the operator (spec §3.2, §9.5).
func testBrainFailedFeedback(t *testing.T, s *stack) {
	// Record every factory-planner wake (the stub replaces the real
	// opencode session; no e2e test spawns one).
	var mu sync.Mutex
	var prompts []string
	s.factory.SetPlannerExec(func(context.Context, string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		prompts = append(prompts, "wake")
		return "handled the failed delegation myself", nil
	})

	// The foreman's brain fails every session for this scenario.
	origExec := s.foreman.Brain().Exec
	s.foreman.Brain().Exec = func(context.Context, string) (string, error) {
		return "", errors.New("brain session exceeded 5m0s")
	}
	t.Cleanup(func() { s.foreman.Brain().Exec = origExec })

	// The planner delegates a nudge via message_foreman (the exact
	// production pattern that dead-ended silently before).
	sc := s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "message_foreman", map[string]any{
		"foreman_id": foremanID,
		"type":       "chat.message",
		"payload":    map[string]any{"text": "please investigate the auto-update feature"},
	})
	if sc["ok"] != true {
		t.Fatalf("message_foreman: %v", sc)
	}

	// The foreman consumes the nudge, its brain fails, the reporter
	// posts brain.failed, the factory relays it, and the factory
	// planner wakes.
	waitFor(t, "factory planner woken by brain.failed", 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(prompts) == 1
	})

	// The relay is visible in the operator inbox (the operator sees what
	// the planner saw, spec §9.5 replay).
	var envs []wire.MessageEnvelope
	if st := s.do("GET", "/v1/inboxes/planner:main/recent?limit=20", "", nil, &envs); st != http.StatusOK {
		t.Fatalf("planner:main recent status = %d", st)
	}
	found := false
	for _, env := range envs {
		if env.Type == "brain.failed" && env.From == foremanInbox {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("brain.failed relay missing from planner:main: %+v", envs)
	}

	// And the original nudge acked away on the foreman side (consumed,
	// not redelivered forever).
	var sums []struct {
		InboxID string `json:"inbox_id"`
		Unacked int    `json:"unacked"`
	}
	if st := s.do("GET", "/v1/inboxes", "", nil, &sums); st != http.StatusOK {
		t.Fatalf("GET /v1/inboxes status = %d", st)
	}
	for _, sum := range sums {
		if sum.InboxID == foremanInbox && sum.Unacked > 0 {
			t.Errorf("foreman inbox unacked = %d after brain failure, want 0 (acked, reported via brain.failed)", sum.Unacked)
		}
	}
}

// testSummaryView: the planner's list view carries summaries only - never
// the results payload or the spec - and read_result serves the full
// payload (spec §6.6, §9.4).
func testSummaryView(t *testing.T, s *stack) {
	if s.happyJobID == "" {
		t.Fatal("scenario 1 did not run first; happy-path job missing")
	}

	entries := s.callToolSlice(s.factoryMCP(), s.factory.PlannerToken(), "list_jobs", "jobs", map[string]any{})
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
