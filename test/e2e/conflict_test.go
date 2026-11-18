package e2e_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// testConflictCoordinator (spec §6.10 conflict coordinator) on the real
// stack: two jobs declare the same file. The first runs (held open by the
// onBuild hook) and files its job-owned merge request; the second is HELD
// with "conflict: waits for <first>" - through the first finishing and
// while its merge is only queued - and is released and dispatched by the
// real pending picker only after the first's merge LANDS.
func testConflictCoordinator(t *testing.T, s *stack) {
	planner := s.factory.PlannerToken()
	const head = "0123456789abcdef0123456789abcdef01234567"
	touches := []string{"internal/e2e/conflict/**", "docs/e2e.md"}

	// The queue stays paused so the first job's request stays queued until
	// the test lands it by hand (the fake worker cannot report merged:true).
	if st := s.do("POST", "/v1/merge-requests/pause", "", map[string]any{}, nil); st != http.StatusOK {
		t.Fatalf("pause = %d", st)
	}
	defer s.do("POST", "/v1/merge-requests/resume", "", map[string]any{}, nil)

	gate := make(chan struct{})
	started := make(chan string, 4)
	s.mu.Lock()
	s.onBuild = func(spec wire.JobSpec) {
		if strings.Contains(spec.Task, "conflict: first job") {
			started <- spec.ID
			<-gate
		}
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.onBuild = nil
		s.mu.Unlock()
	}()

	sc := s.callTool(s.factoryMCP(), planner, "create_job", map[string]any{
		"task": "e2e conflict: first job", "touches": touches,
	})
	first, _ := sc["job_id"].(string)
	if first == "" || sc["state"] == jobs.StateHeld {
		t.Fatalf("first create_job = %v", sc)
	}
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("first job never started")
	}

	// A second job on the same file is held with the conflict reason.
	sc2 := s.callTool(s.factoryMCP(), planner, "create_job", map[string]any{
		"task": "e2e conflict: second job", "touches": []string{"internal/e2e/conflict/x.go"},
	})
	second, _ := sc2["job_id"].(string)
	wantReason := "conflict: waits for " + first + " (internal/e2e/conflict/**)"
	if sc2["state"] != jobs.StateHeld || sc2["held_reason"] != wantReason {
		t.Fatalf("second create_job = %v, want held %q", sc2, wantReason)
	}
	// A job on other files, and one without touches, are not held.
	if sc3 := s.callTool(s.factoryMCP(), planner, "create_job", map[string]any{
		"task": "e2e conflict: unrelated", "touches": []string{"internal/e2e/other/**"},
	}); sc3["state"] == jobs.StateHeld {
		t.Fatalf("disjoint job was held: %v", sc3)
	}
	// list_jobs shows the reason.
	var listed string
	for _, row := range s.callToolSlice(s.factoryMCP(), planner, "list_jobs", "jobs", map[string]any{"state": jobs.StateHeld}) {
		if row["id"] == second {
			listed, _ = row["held_reason"].(string)
		}
	}
	if listed != wantReason {
		t.Fatalf("list_jobs held_reason = %q, want %q", listed, wantReason)
	}

	// The first job files its merge, then finishes.
	tok, err := s.foreman.Auth().MintWorkerToken(s.ctx, foremanInbox, first)
	if err != nil {
		t.Fatal(err)
	}
	mrc := s.callTool(s.workerMCP(), tok, "request_merge", map[string]any{"branch": "feat/e2e-conflict", "head": head, "note": "E2E conflict"})
	mrID, _ := mrc["id"].(string)
	if mrID == "" {
		t.Fatalf("request_merge = %v", mrc)
	}
	close(gate)
	waitFor(t, "first job done", 20*time.Second, func() bool {
		j, ok, _ := s.factory.Jobs().Get(s.ctx, first)
		return ok && j.State == jobs.StateDone
	})

	// Done is not landed: the merge is still queued, the second stays held.
	for range 3 {
		if _, err := s.factory.ReleasePass(s.ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if j, _, _ := s.factory.Jobs().Get(s.ctx, second); j.State != jobs.StateHeld {
		t.Fatalf("second job is %s while the first's merge is only queued", j.State)
	}

	// The merge lands: the second is released, dispatched and runs.
	if ok, err := s.factory.Jobs().ClaimMerge(s.ctx, mrID, "job_e2e_merger"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, ok, err := s.factory.Jobs().TransitionMerge(s.ctx, mrID, jobs.MergeLanded, "landed (e2e)"); err != nil || !ok {
		t.Fatalf("land: %v %v", ok, err)
	}
	waitFor(t, "second job released and done", 40*time.Second, func() bool {
		_, _ = s.factory.ReleasePass(s.ctx)
		j, ok, _ := s.factory.Jobs().Get(s.ctx, second)
		return ok && j.State == jobs.StateDone
	})
	j, _, _ := s.factory.Jobs().Get(s.ctx, second)
	if j.HeldReason != "" || !strings.Contains(j.ForemanID, foremanID) {
		t.Fatalf("second job after release: %+v", j)
	}
}
