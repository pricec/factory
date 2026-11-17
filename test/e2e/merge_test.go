package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// mergeView mirrors GET /v1/merge-requests (spec §6.11).
type mergeView struct {
	Paused   bool                `json:"paused"`
	Requests []jobs.MergeRequest `json:"requests"`
}

func (s *stack) mergeQueue(t *testing.T) mergeView {
	t.Helper()
	var v mergeView
	if st := s.do("GET", "/v1/merge-requests", "", nil, &v); st != http.StatusOK {
		t.Fatalf("GET /v1/merge-requests = %d", st)
	}
	return v
}

func (s *stack) mergeByID(t *testing.T, id string) jobs.MergeRequest {
	t.Helper()
	for _, m := range s.mergeQueue(t).Requests {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("merge request %s not listed", id)
	return jobs.MergeRequest{}
}

// testMergeQueue (spec §6.11) on the real stack: the planner's
// request_merge queues two merges while the queue is paused; resume
// releases exactly one at a time (a watcher asserts it never sees two
// merging), each merge job runs pinned on the integration foreman through
// the real dispatch -> foreman -> worker -> result path and lands its
// request; then a merge whose worker reports head drift goes
// needs_attention with a factory:merge_queue alert, and the queue moves on.
func testMergeQueue(t *testing.T, s *stack) {
	planner := s.factory.PlannerToken()
	const head = "0123456789abcdef0123456789abcdef01234567"

	var paused mergeView
	if st := s.do("POST", "/v1/merge-requests/pause", "", map[string]any{}, &paused); st != http.StatusOK || !paused.Paused {
		t.Fatalf("pause = %d %+v", st, paused)
	}
	first := s.callTool(s.factoryMCP(), planner, "request_merge", map[string]any{"branch": "feat/e2e-one", "head": head, "note": "E2E one"})
	second := s.callTool(s.factoryMCP(), planner, "request_merge", map[string]any{"branch": "feat/e2e-two", "head": head, "note": "E2E two"})
	id1, _ := first["id"].(string)
	id2, _ := second["id"].(string)
	if first["state"] != jobs.MergeQueued || second["state"] != jobs.MergeQueued || id1 == "" || id2 == "" {
		t.Fatalf("request_merge = %v / %v", first, second)
	}
	time.Sleep(300 * time.Millisecond) // a paused queue must not dispatch
	for _, m := range s.mergeQueue(t).Requests {
		if m.State != jobs.MergeQueued {
			t.Fatalf("paused queue moved %s to %s", m.ID, m.State)
		}
	}

	// Watch the invariant for the whole run: never two merging.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	maxMerging := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			n, err := s.factory.Jobs().MergingRequests(s.ctx)
			if err != nil {
				continue
			}
			mu.Lock()
			maxMerging = max(maxMerging, len(n))
			mu.Unlock()
		}
	}()

	if st := s.do("POST", "/v1/merge-requests/resume", "", map[string]any{}, nil); st != http.StatusOK {
		t.Fatalf("resume = %d", st)
	}
	waitFor(t, "both merges landed", 30*time.Second, func() bool {
		return s.mergeByID(t, id1).State == jobs.MergeLanded && s.mergeByID(t, id2).State == jobs.MergeLanded
	})
	close(stop)
	wg.Wait()
	if maxMerging != 1 {
		t.Fatalf("max concurrently merging = %d, want exactly 1", maxMerging)
	}
	m1, m2 := s.mergeByID(t, id1), s.mergeByID(t, id2)
	if m1.RequestedBy != "planner" || !strings.Contains(m1.Result, fakeSummary) {
		t.Errorf("first = %+v", m1)
	}
	j1, ok1, _ := s.factory.Jobs().Get(s.ctx, m1.JobID)
	j2, ok2, _ := s.factory.Jobs().Get(s.ctx, m2.JobID)
	if !ok1 || !ok2 {
		t.Fatal("merge jobs missing")
	}
	for _, j := range []jobs.Job{j1, j2} {
		if j.State != jobs.StateDone || j.ForemanID != foremanInbox || j.Spec.Foreman != foremanInbox ||
			j.Spec.CreatedBy != jobs.MergeJobCreatedBy || !strings.Contains(j.Spec.Task, jobs.MergeProcedureDoc) {
			t.Errorf("merge job %s: state=%s foreman=%s pin=%s by=%s", j.ID, j.State, j.ForemanID, j.Spec.Foreman, j.Spec.CreatedBy)
		}
	}
	// Oldest first, and the second job was only created once the first
	// had landed.
	if j2.CreatedAt < m1.UpdatedAt {
		t.Errorf("second merge job created %s before the first landed %s", j2.CreatedAt, m1.UpdatedAt)
	}

	// Head drift: the worker reports failure; never retried; the request
	// needs attention and the operator is alerted.
	drift := s.callTool(s.factoryMCP(), planner, "request_merge", map[string]any{"branch": fakeDriftBranch, "head": "0badc0de", "note": "drift"})
	idD, _ := drift["id"].(string)
	waitFor(t, "drift request needs_attention", 30*time.Second, func() bool {
		return s.mergeByID(t, idD).State == jobs.MergeNeedsAttention
	})
	md := s.mergeByID(t, idD)
	if !strings.Contains(md.Result, "head drift") {
		t.Errorf("drift result = %q, want the worker's head-drift report", md.Result)
	}
	jd, _, _ := s.factory.Jobs().Get(s.ctx, md.JobID)
	if jd.State != jobs.StateDead || jd.Attempts != 0 {
		t.Errorf("drift job state=%s attempts=%d, want dead with no retry", jd.State, jd.Attempts)
	}
	var found *e2eAlert
	for _, a := range s.openAlerts(t) {
		if a.Source == "factory:merge_queue" && strings.Contains(a.Title, idD) {
			found = &a
			break
		}
	}
	if found == nil || found.Priority != 1 || found.ExpiresAt != nil {
		t.Fatalf("needs_attention alert = %+v", found)
	}
	if v := s.mergeQueue(t); v.Paused {
		t.Error("queue left paused")
	}
}

// testJobOwnedMerge (spec §6.11 job-owned merges, §9.2) on the real stack:
// a worker calls request_merge on the foreman's worker MCP with its
// per-job token; the foreman relays it to the factory's merge queue with
// the job id; the request is job-owned, runs through the same serialized
// merge job, and lands - no planner involved. A refusal by the factory
// (bad head) comes back to the worker as a tool error.
func testJobOwnedMerge(t *testing.T, s *stack) {
	ctx := s.ctx
	const head = "0123456789abcdef0123456789abcdef01234567"
	spec := wire.NewJobSpec("e2e job-owned merge: change code, queue the merge", "factory")
	if err := s.factory.Jobs().Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	workerTok, err := s.foreman.Auth().MintWorkerToken(ctx, foremanInbox, spec.ID)
	if err != nil {
		t.Fatalf("MintWorkerToken: %v", err)
	}

	sc := s.callTool(s.workerMCP(), workerTok, "request_merge", map[string]any{
		"branch": "feat/e2e-owned", "head": head, "note": "E2E owned",
	})
	id, _ := sc["id"].(string)
	if id == "" || sc["state"] != jobs.MergeQueued {
		t.Fatalf("worker request_merge = %v", sc)
	}
	mr := s.mergeByID(t, id)
	if mr.RequestedBy != jobs.RequestedByJob || mr.SourceJobID != spec.ID || mr.Branch != "feat/e2e-owned" || mr.Head != head {
		t.Fatalf("job-owned request = %+v", mr)
	}
	waitFor(t, "job-owned merge landed", 30*time.Second, func() bool {
		return s.mergeByID(t, id).State == jobs.MergeLanded
	})
	if got := s.mergeByID(t, id); got.JobID == "" || got.JobID == spec.ID {
		t.Fatalf("merge job = %q, want a separate merge job (source %s)", got.JobID, spec.ID)
	}

	// The factory's validation reaches the worker as a tool error.
	_, out := s.rpc(s.workerMCP(), workerTok, "tools/call", map[string]any{
		"name": "request_merge", "arguments": map[string]any{"branch": "feat/e2e-owned-bad", "head": "not-a-sha"},
	})
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "hex commit sha") {
		t.Fatalf("bad head: %s, want the factory's validation message", raw)
	}
}
