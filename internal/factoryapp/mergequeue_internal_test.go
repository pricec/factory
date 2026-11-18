// Merge queue serialization (spec §6.11): exactly one merging at a time,
// terminal releases the next, pause holds dispatches, cancel mid-flight
// cancels the job, failures go needs_attention with an operator alert.
package factoryapp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

const mqHead = "0123456789abcdef0123456789abcdef01234567"

// newMergeApp builds an App (no Run loops: passes are test-driven) with
// the integration foreman registered online.
func newMergeApp(t *testing.T) *App {
	t.Helper()
	t.Setenv(EnvMergeForeman, "")
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	a, err := New(Config{DataDir: t.TempDir(), Addr: ":8093", AdvertiseURL: "http://factory.test", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.mergeForeman != DefaultMergeForeman {
		t.Fatalf("merge foreman = %q, want %q", a.mergeForeman, DefaultMergeForeman)
	}
	if err := a.jobs.UpsertForeman(context.Background(), DefaultMergeForeman, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	return a
}

func queueMerge(t *testing.T, a *App, branch string) jobs.MergeRequest {
	t.Helper()
	mr, err := a.jobs.CreateMergeRequest(context.Background(), jobs.NewMergeRequest{Branch: branch, Head: mqHead, RequestedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return mr
}

func getMerge(t *testing.T, a *App, id string) jobs.MergeRequest {
	t.Helper()
	mr, ok, err := a.jobs.GetMergeRequest(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("get %s: ok=%v err=%v", id, ok, err)
	}
	return mr
}

// assertOneMerging is the serialization invariant: at most one merging
// request and at most one live merge job, ever.
func assertOneMerging(t *testing.T, a *App, want int) {
	t.Helper()
	ctx := context.Background()
	merging, err := a.jobs.MergingRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live, err := a.jobs.ActiveMergeJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(merging) != want || live != want {
		t.Fatalf("merging = %d, live merge jobs = %d, want %d", len(merging), live, want)
	}
}

func pass(t *testing.T, a *App) string {
	t.Helper()
	id, err := a.MergeQueuePass(context.Background())
	if err != nil {
		t.Fatalf("MergeQueuePass: %v", err)
	}
	return id
}

// finishJob drives a dispatched merge job through started + a result via
// the real consumer path (so the no-retry rule applies).
func finishJob(t *testing.T, a *App, jobID, status string) {
	t.Helper()
	ctx := context.Background()
	if err := a.jobs.ApplyEvent(ctx, jobID, "started"); err != nil {
		t.Fatal(err)
	}
	res := wire.JobResult{
		JobID: jobID, Status: status, Summary: "merged and deployed",
		Result: json.RawMessage(`{"merged":true,"merge_commit":"0123456789ab"}`),
	}
	if status == wire.StatusFailed {
		res.Summary = "head drift: origin has 89abcdef"
		res.Error = &wire.JobError{Kind: wire.KindOther, Message: "head drift"}
	}
	payload, _ := json.Marshal(res)
	a.handleEnvelope(ctx, wire.MessageEnvelope{Type: "job.result", JobID: jobID, Payload: payload}, map[string]bool{})
}

// finishJobStage fails jobID with a report whose result.stage is stage.
func finishJobStage(t *testing.T, a *App, jobID, stage string) {
	t.Helper()
	ctx := context.Background()
	if err := a.jobs.ApplyEvent(ctx, jobID, "started"); err != nil {
		t.Fatal(err)
	}
	res := wire.JobResult{
		JobID: jobID, Status: wire.StatusFailed, Summary: stage + ": stopped",
		Result: json.RawMessage(`{"stage":"` + stage + `"}`),
		Error:  &wire.JobError{Kind: wire.KindOther, Message: stage + ": stopped"},
	}
	payload, _ := json.Marshal(res)
	a.handleEnvelope(ctx, wire.MessageEnvelope{Type: "job.result", JobID: jobID, Payload: payload}, map[string]bool{})
}

// TestMergeQueueEscalation: merge jobs start on the default tier; a CI
// failure retries exactly once on hardest (same request, still merging, one
// live job); a second failure is not retried and needs attention.
func TestMergeQueueEscalation(t *testing.T) {
	a := newMergeApp(t)
	ctx := context.Background()
	first := queueMerge(t, a, "feat/one")
	pass(t, a)
	m := getMerge(t, a, first.ID)
	j1, _, _ := a.jobs.Get(ctx, m.JobID)
	if j1.Spec.Tier != wire.TierDefault {
		t.Fatalf("first merge job tier = %q, want default", j1.Spec.Tier)
	}

	finishJobStage(t, a, j1.ID, "ci")
	m = getMerge(t, a, first.ID)
	if m.State != jobs.MergeMerging || m.JobID == j1.ID {
		t.Fatalf("after default-tier ci failure: state=%s job=%s, want merging on a new job", m.State, m.JobID)
	}
	j2, _, _ := a.jobs.Get(ctx, m.JobID)
	if j2.Spec.Tier != wire.TierHardest || j2.Spec.Foreman != DefaultMergeForeman {
		t.Fatalf("escalated job tier=%q pin=%q, want hardest on the merge foreman", j2.Spec.Tier, j2.Spec.Foreman)
	}
	if old, _, _ := a.jobs.Get(ctx, j1.ID); old.State != jobs.StateDead {
		t.Fatalf("failed job state = %s, want dead", old.State)
	}
	assertOneMerging(t, a, 1)
	if n := len(mergeAlerts(t, a)); n != 0 {
		t.Fatalf("escalation raised %d alerts, want 0", n)
	}

	finishJobStage(t, a, j2.ID, "ci")
	m = getMerge(t, a, first.ID)
	if m.State != jobs.MergeNeedsAttention || m.JobID != j2.ID {
		t.Fatalf("after hardest failure: state=%s job=%s, want needs_attention on %s", m.State, m.JobID, j2.ID)
	}
	if d, _, _ := a.jobs.Get(ctx, j2.ID); d.State != jobs.StateDead {
		t.Fatalf("hardest job state = %s, want dead (no further retry)", d.State)
	}
	assertOneMerging(t, a, 0)
	if n := len(mergeAlerts(t, a)); n != 1 {
		t.Fatalf("alerts = %d, want 1", n)
	}
}

// TestMergeQueueNoEscalationAfterMerge: stages where a re-run is unsafe or
// pointless (merge, deploy) never escalate.
func TestMergeQueueNoEscalationAfterMerge(t *testing.T) {
	for _, stage := range []string{"merge", "deploy", "head_drift"} {
		a := newMergeApp(t)
		first := queueMerge(t, a, "feat/one")
		pass(t, a)
		m := getMerge(t, a, first.ID)
		finishJobStage(t, a, m.JobID, stage)
		if got := getMerge(t, a, first.ID); got.State != jobs.MergeNeedsAttention || got.JobID != m.JobID {
			t.Errorf("%s: state=%s job=%s, want needs_attention on the original job", stage, got.State, got.JobID)
		}
	}
}

func mergeAlerts(t *testing.T, a *App) []tasks.Alert {
	t.Helper()
	all, err := a.tasks.ListAlerts(context.Background(), tasks.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var out []tasks.Alert
	for _, al := range all {
		if al.Source == mergeAlertSource {
			out = append(out, al)
		}
	}
	return out
}

// TestMergeQueueSerialization: two queued -> exactly one dispatched (and
// pinned); further passes dispatch nothing; the terminal transition lands
// the first and releases the second.
func TestMergeQueueSerialization(t *testing.T) {
	a := newMergeApp(t)
	ctx := context.Background()
	first, second := queueMerge(t, a, "feat/one"), queueMerge(t, a, "feat/two")

	if got := pass(t, a); got != first.ID {
		t.Fatalf("dispatched %q, want the oldest %s", got, first.ID)
	}
	for range 3 {
		if got := pass(t, a); got != "" {
			t.Fatalf("second dispatch %q while one is merging", got)
		}
	}
	assertOneMerging(t, a, 1)
	m1 := getMerge(t, a, first.ID)
	if m1.State != jobs.MergeMerging || m1.JobID == "" {
		t.Fatalf("first = %+v", m1)
	}
	if getMerge(t, a, second.ID).State != jobs.MergeQueued {
		t.Fatal("second left the queue")
	}
	job, ok, err := a.jobs.Get(ctx, m1.JobID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if job.State != jobs.StateDispatched || job.ForemanID != DefaultMergeForeman || job.Spec.Foreman != DefaultMergeForeman {
		t.Errorf("merge job state=%s foreman=%s pin=%s, want dispatched to %s", job.State, job.ForemanID, job.Spec.Foreman, DefaultMergeForeman)
	}
	if job.Spec.Task != jobs.MergeTask(m1) {
		t.Error("merge job task is not the generated MergeTask text")
	}

	finishJob(t, a, m1.JobID, wire.StatusDone)
	m1 = getMerge(t, a, first.ID)
	if m1.State != jobs.MergeLanded || !strings.Contains(m1.Result, "merged and deployed") {
		t.Fatalf("first after done = %+v", m1)
	}
	assertOneMerging(t, a, 0)
	if got := pass(t, a); got != second.ID {
		t.Fatalf("after landing dispatched %q, want %s", got, second.ID)
	}
	assertOneMerging(t, a, 1)
}

// TestMergeQueueConcurrentPasses hammers the pass from many goroutines:
// still exactly one merging.
func TestMergeQueueConcurrentPasses(t *testing.T) {
	a := newMergeApp(t)
	for _, b := range []string{"a", "b", "c", "d", "e"} {
		queueMerge(t, a, "feat/"+b)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	dispatched := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := a.MergeQueuePass(context.Background()); err == nil && id != "" {
				mu.Lock()
				dispatched++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if dispatched != 1 {
		t.Fatalf("dispatched %d, want exactly 1", dispatched)
	}
	assertOneMerging(t, a, 1)
}

// TestMergeQueuePause: pause holds new dispatches, an in-flight merge
// still finishes, resume releases the next; the flag persists.
func TestMergeQueuePause(t *testing.T) {
	a := newMergeApp(t)
	ctx := context.Background()
	first, second := queueMerge(t, a, "feat/one"), queueMerge(t, a, "feat/two")
	pass(t, a)
	if err := a.jobs.SetMergePaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	m1 := getMerge(t, a, first.ID)
	finishJob(t, a, m1.JobID, wire.StatusDone) // in-flight finishes while paused
	if getMerge(t, a, first.ID).State != jobs.MergeLanded {
		t.Fatal("in-flight merge did not finish while paused")
	}
	if got := pass(t, a); got != "" {
		t.Fatalf("paused queue dispatched %s", got)
	}
	if paused, _, _ := a.jobs.MergePaused(ctx); !paused {
		t.Fatal("pause flag not persisted")
	}
	assertOneMerging(t, a, 0)
	if err := a.jobs.SetMergePaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := pass(t, a); got != second.ID {
		t.Fatalf("resume dispatched %q, want %s", got, second.ID)
	}
}

// TestMergeQueueCancelMidFlight: cancelling the merging request cancels
// its job and releases the queue.
func TestMergeQueueCancelMidFlight(t *testing.T) {
	a := newMergeApp(t)
	ctx := context.Background()
	first, second := queueMerge(t, a, "feat/one"), queueMerge(t, a, "feat/two")
	pass(t, a)
	m1 := getMerge(t, a, first.ID)
	if err := a.jobs.ApplyEvent(ctx, m1.JobID, "started"); err != nil {
		t.Fatal(err)
	}
	got, err := a.jobs.CancelMergeRequest(ctx, first.ID, "operator changed mind")
	if err != nil || got.State != jobs.MergeCancelled {
		t.Fatalf("cancel = %+v, %v", got, err)
	}
	if job, _, _ := a.jobs.Get(ctx, m1.JobID); job.State != jobs.StateCancelled {
		t.Fatalf("merge job state = %s, want cancelled", job.State)
	}
	assertOneMerging(t, a, 0)
	if id := pass(t, a); id != second.ID {
		t.Fatalf("after cancel dispatched %q, want %s", id, second.ID)
	}
	if n := len(mergeAlerts(t, a)); n != 0 {
		t.Errorf("cancel raised %d merge alerts, want 0", n)
	}
}

// TestMergeQueueFailureNeedsAttention: a failed merge job (e.g. head
// drift) is never retried: dead -> needs_attention + one P1 alert, and the
// queue moves on.
func TestMergeQueueFailureNeedsAttention(t *testing.T) {
	a := newMergeApp(t)
	ctx := context.Background()
	first, second := queueMerge(t, a, "feat/one"), queueMerge(t, a, "feat/two")
	pass(t, a)
	m1 := getMerge(t, a, first.ID)
	finishJob(t, a, m1.JobID, wire.StatusFailed)
	job, _, _ := a.jobs.Get(ctx, m1.JobID)
	if job.State != jobs.StateDead || job.NextAttempt != "" {
		t.Fatalf("merge job state=%s next_attempt=%q, want dead with no retry", job.State, job.NextAttempt)
	}
	m1 = getMerge(t, a, first.ID)
	if m1.State != jobs.MergeNeedsAttention || !strings.Contains(m1.Result, "head drift") {
		t.Fatalf("request = %+v", m1)
	}
	al := mergeAlerts(t, a)
	if len(al) != 1 || al[0].Priority != alertPriorityMerge || !strings.Contains(al[0].Title, first.ID) ||
		!strings.Contains(al[0].Body, "head drift") || al[0].ExpiresAt != nil {
		t.Fatalf("alerts = %+v", al)
	}
	if id := pass(t, a); id != second.ID {
		t.Fatalf("after failure dispatched %q, want %s", id, second.ID)
	}
}

// TestMergeQueueReconcile: a merging row whose job vanished (crash between
// claim and create) is closed needs_attention, not left blocking.
func TestMergeQueueReconcile(t *testing.T) {
	a := newMergeApp(t)
	ctx := context.Background()
	first, second := queueMerge(t, a, "feat/one"), queueMerge(t, a, "feat/two")
	if ok, err := a.jobs.ClaimMerge(ctx, first.ID, wire.NewJobID()); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if id := pass(t, a); id != second.ID {
		t.Fatalf("dispatched %q, want %s after reconciling", id, second.ID)
	}
	if st := getMerge(t, a, first.ID).State; st != jobs.MergeNeedsAttention {
		t.Fatalf("orphan state = %s, want needs_attention", st)
	}
	if len(mergeAlerts(t, a)) != 1 {
		t.Fatal("no alert for the orphaned merge")
	}
}

// TestMergeQueueOfflineForeman: with the integration foreman offline the
// job is created pinned and pending; the request is merging (the pending
// picker dispatches when the foreman returns) and nothing else dispatches.
func TestMergeQueueOfflineForeman(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	a, err := New(Config{DataDir: t.TempDir(), Addr: ":8094", Now: func() time.Time { return now }, MergeForeman: "integ"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.mergeForeman != "foreman:integ" {
		t.Fatalf("merge foreman = %q, want foreman:integ", a.mergeForeman)
	}
	first := queueMerge(t, a, "feat/one")
	queueMerge(t, a, "feat/two")
	if id := pass(t, a); id != first.ID {
		t.Fatalf("dispatched %q", id)
	}
	job, _, _ := a.jobs.Get(context.Background(), getMerge(t, a, first.ID).JobID)
	if job.State != jobs.StatePending || job.Spec.Foreman != "foreman:integ" {
		t.Fatalf("job state=%s pin=%s, want pending pinned to foreman:integ", job.State, job.Spec.Foreman)
	}
	if id := pass(t, a); id != "" {
		t.Fatalf("second dispatch %q", id)
	}
	assertOneMerging(t, a, 1)
}

// TestMergeQueueDoneWithoutMergedIsNotLanded: a merge job that ends done
// without result.merged:true (the 2026-11-18 incident: "nothing was merged
// due to conflicts" reported as done) must not land the request; it goes
// needs_attention, classified by result.reason, and the queue moves on.
func TestMergeQueueDoneWithoutMergedIsNotLanded(t *testing.T) {
	for _, tc := range []struct {
		name, result, failure string
	}{
		{"merged false", `{"merged":false,"reason":"conflict"}`, jobs.MergeFailureConflict},
		{"no result", ``, jobs.MergeFailureOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newMergeApp(t)
			ctx := context.Background()
			first, second := queueMerge(t, a, "feat/one"), queueMerge(t, a, "feat/two")
			if got := pass(t, a); got != first.ID {
				t.Fatalf("dispatched %q, want %s", got, first.ID)
			}
			jobID := getMerge(t, a, first.ID).JobID
			if err := a.jobs.ApplyEvent(ctx, jobID, "started"); err != nil {
				t.Fatal(err)
			}
			res := wire.JobResult{JobID: jobID, Status: wire.StatusDone, Summary: "nothing merged: conflicts with master"}
			if tc.result != "" {
				res.Result = json.RawMessage(tc.result)
			}
			payload, _ := json.Marshal(res)
			a.handleEnvelope(ctx, wire.MessageEnvelope{Type: "job.result", JobID: jobID, Payload: payload}, map[string]bool{})
			m := getMerge(t, a, first.ID)
			if m.State != jobs.MergeNeedsAttention || m.Failure != tc.failure {
				t.Fatalf("state=%s failure=%s, want needs_attention/%s (%+v)", m.State, m.Failure, tc.failure, m)
			}
			if got := pass(t, a); got != second.ID {
				t.Fatalf("queue did not move on: dispatched %q, want %s", got, second.ID)
			}
		})
	}
}
