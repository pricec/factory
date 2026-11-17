// Job-owned merges (spec §6.11): a job's own merge request is tracked
// against its task; a fixable failure dispatches a bounded fix job on the
// same branch; everything else escalates to the planner (waking note) and
// the operator (alert); dependency-gated requests wait and are never
// silent about it.
package factoryapp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/auditor"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

const ownerForeman = "foreman:alpha"

// ownedJob creates a task and a code-change job linked to it, dispatched
// to (and started on) ownerForeman: the job that will file its own merge.
func ownedJob(t *testing.T, a *App, title string) (jobs.Job, tasks.Task) {
	t.Helper()
	ctx := context.Background()
	if err := a.jobs.UpsertForeman(ctx, ownerForeman, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	task, err := a.tasks.CreateTask(ctx, title, tasks.DefaultPriority)
	if err != nil {
		t.Fatal(err)
	}
	spec := wire.NewJobSpec("ORIGIN TASK: "+title, "factory")
	spec.ID = wire.NewJobID()
	spec.Tier = wire.TierDefault
	if err := a.jobs.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := a.tasks.LinkJob(ctx, task.ID, spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.jobs.Dispatch(ctx, spec.ID, ownerForeman); err != nil {
		t.Fatal(err)
	}
	if err := a.jobs.ApplyEvent(ctx, spec.ID, "started"); err != nil {
		t.Fatal(err)
	}
	job, _, _ := a.jobs.Get(ctx, spec.ID)
	return job, task
}

// ownMerge files jobID's own merge request, the way the foreman relay
// does (POST /v1/merge-requests with job_id).
func ownMerge(t *testing.T, a *App, jobID, branch string, after ...string) jobs.MergeRequest {
	t.Helper()
	mr, err := a.jobs.CreateMergeRequest(context.Background(), jobs.NewMergeRequest{Branch: branch, Head: mqHead, JobID: jobID, After: after})
	if err != nil {
		t.Fatal(err)
	}
	return mr
}

// report sends jobID's result through the real consumer path.
func report(t *testing.T, a *App, jobID, status, summary, stage string) {
	t.Helper()
	res := wire.JobResult{JobID: jobID, Status: status, Summary: summary}
	if status == wire.StatusFailed {
		res.Error = &wire.JobError{Kind: wire.KindOther, Message: summary}
	}
	if stage != "" {
		res.Result = json.RawMessage(`{"stage":"` + stage + `"}`)
	}
	payload, _ := json.Marshal(res)
	a.handleEnvelope(context.Background(), wire.MessageEnvelope{Type: "job.result", JobID: jobID, Payload: payload}, map[string]bool{})
}

// failMerge dispatches the next merge (which must be want) and fails its
// merge job with summary / result.stage.
func failMerge(t *testing.T, a *App, want jobs.MergeRequest, summary, stage string) jobs.MergeRequest {
	t.Helper()
	if id := pass(t, a); id != want.ID {
		t.Fatalf("dispatched %q, want %s", id, want.ID)
	}
	m := getMerge(t, a, want.ID)
	if err := a.jobs.ApplyEvent(context.Background(), m.JobID, "started"); err != nil {
		t.Fatal(err)
	}
	report(t, a, m.JobID, wire.StatusFailed, summary, stage)
	return getMerge(t, a, want.ID)
}

// mergeNotes returns the notify.merge payloads on a task thread, oldest
// first, with the envelopes they rode in.
func mergeNotes(t *testing.T, a *App, taskID string) ([]mergeNote, []wire.MessageEnvelope) {
	t.Helper()
	thread, err := a.inbox.ThreadMessages(context.Background(), taskID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var notes []mergeNote
	var envs []wire.MessageEnvelope
	for _, env := range thread {
		if env.Type != notifyMergeType {
			continue
		}
		var n mergeNote
		if err := json.Unmarshal(env.Payload, &n); err != nil {
			t.Fatal(err)
		}
		notes, envs = append(notes, n), append(envs, env)
	}
	return notes, envs
}

func lastNote(t *testing.T, a *App, taskID string) (mergeNote, wire.MessageEnvelope) {
	t.Helper()
	notes, envs := mergeNotes(t, a, taskID)
	if len(notes) == 0 {
		t.Fatalf("no notify.merge on task %s", taskID)
	}
	return notes[len(notes)-1], envs[len(envs)-1]
}

// TestJobOwnedMergeFixLoop is the whole automatic path: the job's merge
// fails on red checks -> fix job 1 on the same branch, same foreman, same
// task, no alert, no planner wake; its request fails on a conflict -> fix
// job 2; its request fails again -> the bound is hit: P1 alert on the task
// and a waking note for the planner, and no third fix job.
func TestJobOwnedMergeFixLoop(t *testing.T) {
	a := newMergeApp(t)
	ctx := context.Background()
	src, task := ownedJob(t, a, "usage reset countdown")
	first := ownMerge(t, a, src.ID, "feat/countdown")
	report(t, a, src.ID, wire.StatusDone, "pushed feat/countdown, queued "+first.ID, "")

	requester := src.ID
	current := first
	failures := []struct{ summary, stage, kind string }{
		{"checks red after rerun: TestCountdown", "ci", jobs.MergeFailureCI},
		{"conflict: internal/factoryapp/ui.html", "", jobs.MergeFailureConflict},
	}
	for round, f := range failures {
		got := failMerge(t, a, current, f.summary, f.stage)
		if got.State != jobs.MergeNeedsAttention || got.Failure != f.kind || got.FixRound != round || got.SourceJobID != requester {
			t.Fatalf("round %d: request = %+v", round, got)
		}
		if al := mergeAlerts(t, a); len(al) != 0 {
			t.Fatalf("round %d: a fixable failure raised an alert: %+v", round, al)
		}
		note, env := lastNote(t, a, task.ID)
		if note.Action != mergeActionFixDispatched || note.FixJobID == "" || note.MergeRequestID != current.ID || wakesPlanner(env) {
			t.Fatalf("round %d: note = %+v (wakes planner: %v); want a non-waking fix_dispatched", round, note, wakesPlanner(env))
		}
		fix, ok, err := a.jobs.Get(ctx, note.FixJobID)
		if err != nil || !ok {
			t.Fatalf("round %d: fix job %s missing: %v", round, note.FixJobID, err)
		}
		p, isFix := jobs.MergeFixPayloadOf(fix.Spec)
		if !isFix || p.FixRound != round+1 || p.Branch != "feat/countdown" || p.OriginJobID != src.ID || p.Failure != f.kind {
			t.Fatalf("round %d: fix payload = %+v (%v)", round, p, isFix)
		}
		if fix.State != jobs.StateDispatched || fix.ForemanID != ownerForeman || fix.Spec.Foreman != ownerForeman || fix.Spec.Tier != wire.TierDefault {
			t.Fatalf("round %d: fix job state=%s foreman=%s pin=%s tier=%s", round, fix.State, fix.ForemanID, fix.Spec.Foreman, fix.Spec.Tier)
		}
		if !strings.Contains(fix.Spec.Task, "ORIGIN TASK: usage reset countdown") || !strings.Contains(fix.Spec.Task, f.summary) {
			t.Fatalf("round %d: fix task lacks the origin task or the failure:\n%s", round, fix.Spec.Task)
		}
		if tid, ok, _ := a.tasks.TaskForJob(ctx, fix.ID); !ok || tid != task.ID {
			t.Fatalf("round %d: fix job linked to task %q, want %s", round, tid, task.ID)
		}
		// The fix job does its work and files the next request itself.
		if err := a.jobs.ApplyEvent(ctx, fix.ID, "started"); err != nil {
			t.Fatal(err)
		}
		current = ownMerge(t, a, fix.ID, "feat/countdown")
		if current.FixRound != round+1 {
			t.Fatalf("round %d: re-request fix_round = %d, want %d", round, current.FixRound, round+1)
		}
		report(t, a, fix.ID, wire.StatusDone, "fixed, queued "+current.ID, "")
		requester = fix.ID
	}

	// Third failure: the bound.
	got := failMerge(t, a, current, "checks red after rerun: TestCountdown", "ci")
	if got.FixRound != jobs.MaxMergeFixRounds {
		t.Fatalf("final request fix_round = %d, want %d", got.FixRound, jobs.MaxMergeFixRounds)
	}
	note, env := lastNote(t, a, task.ID)
	if note.Action != mergeActionEscalated || note.FixJobID != "" || !wakesPlanner(env) || !strings.Contains(note.Text, "used up") {
		t.Fatalf("final note = %+v (wakes planner: %v); want a waking escalation", note, wakesPlanner(env))
	}
	al := mergeAlerts(t, a)
	if len(al) != 1 || al[0].Priority != alertPriorityMerge || al[0].TaskID != task.ID ||
		!strings.Contains(al[0].Body, "used up") || !strings.Contains(al[0].Body, requester) {
		t.Fatalf("alerts = %+v", al)
	}
	all, err := a.jobs.List(ctx, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	fixes := 0
	for _, j := range all {
		if j.Spec.CreatedBy == jobs.MergeFixCreatedBy {
			fixes++
		}
	}
	if fixes != jobs.MaxMergeFixRounds {
		t.Fatalf("%d fix jobs were created, want exactly %d (the loop is bounded)", fixes, jobs.MaxMergeFixRounds)
	}
	// Nothing is left merging and the queue is free.
	assertOneMerging(t, a, 0)
}

// TestJobOwnedMergeEscalations: failures the branch's job cannot fix, and
// situations where a fix job must not start, go straight to the alert and
// (for an open task) a waking note - never a fix job.
func TestJobOwnedMergeEscalations(t *testing.T) {
	tests := []struct {
		name       string
		summary    string
		stage      string
		closeTask  bool
		wantKind   string
		wantWhy    string
		wantWaking bool
	}{
		{"deploy failure", "deploy: health check failed; rolled back: yes", "deploy", false, jobs.MergeFailureDeploy, "not something the branch's job can fix", true},
		{"preflight", "preflight: gh auth status failed", "", false, jobs.MergeFailurePreflight, "not something the branch's job can fix", true},
		{"unclassified", "the worker gave up", "", false, jobs.MergeFailureOther, "not something the branch's job can fix", true},
		{"closed task", "checks red after rerun: TestX", "ci", true, jobs.MergeFailureCI, "is closed", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newMergeApp(t)
			ctx := context.Background()
			src, task := ownedJob(t, a, "some change")
			mr := ownMerge(t, a, src.ID, "feat/change")
			report(t, a, src.ID, wire.StatusDone, "queued", "")
			if tc.closeTask {
				closed, reason := tasks.StatusClosed, "operator closed it"
				if _, err := a.tasks.UpdateTask(ctx, task.ID, tasks.TaskUpdate{Status: &closed, CloseReason: &reason}); err != nil {
					t.Fatal(err)
				}
			}
			got := failMerge(t, a, mr, tc.summary, tc.stage)
			if got.Failure != tc.wantKind {
				t.Fatalf("failure = %q, want %q", got.Failure, tc.wantKind)
			}
			note, env := lastNote(t, a, task.ID)
			if note.Action != mergeActionEscalated || note.FixJobID != "" || wakesPlanner(env) != tc.wantWaking || !strings.Contains(note.Text, tc.wantWhy) {
				t.Fatalf("note = %+v (wakes planner: %v)", note, wakesPlanner(env))
			}
			al := mergeAlerts(t, a)
			if len(al) != 1 || !strings.Contains(al[0].Body, tc.wantWhy) || al[0].TaskID != task.ID {
				t.Fatalf("alerts = %+v", al)
			}
			all, _ := a.jobs.List(ctx, "", "", 100)
			for _, j := range all {
				if j.Spec.CreatedBy == jobs.MergeFixCreatedBy {
					t.Fatalf("a fix job was created: %s", j.ID)
				}
			}
			if cur, _ := a.tasks.GetTask(ctx, task.ID); tc.closeTask && cur.Status != tasks.StatusClosed {
				t.Fatal("a merge outcome reopened a closed task")
			}
		})
	}
}

// TestJobOwnedMergeLanded: the landed outcome reaches the task as one
// waking note (so the planner tells the operator) without any alert, and
// the job's own notify.job already names the queued request so the
// planner does not queue or chase it.
func TestJobOwnedMergeLanded(t *testing.T) {
	a := newMergeApp(t)
	ctx := context.Background()
	src, task := ownedJob(t, a, "small change")
	mr := ownMerge(t, a, src.ID, "feat/small")
	report(t, a, src.ID, wire.StatusDone, "queued "+mr.ID, "")

	thread, err := a.inbox.ThreadMessages(ctx, task.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var jobNote struct {
		MergeRequests []struct{ ID, Branch, State string } `json:"merge_requests"`
	}
	found := false
	for _, env := range thread {
		if env.Type == notifyJobType && env.JobID == src.ID {
			found = true
			if err := json.Unmarshal(env.Payload, &jobNote); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found || len(jobNote.MergeRequests) != 1 || jobNote.MergeRequests[0].ID != mr.ID ||
		jobNote.MergeRequests[0].Branch != "feat/small" || jobNote.MergeRequests[0].State != jobs.MergeQueued {
		t.Fatalf("notify.job merge_requests = %+v (found %v)", jobNote.MergeRequests, found)
	}

	if id := pass(t, a); id != mr.ID {
		t.Fatalf("dispatched %q, want %s", id, mr.ID)
	}
	finishJob(t, a, getMerge(t, a, mr.ID).JobID, wire.StatusDone)
	if st := getMerge(t, a, mr.ID).State; st != jobs.MergeLanded {
		t.Fatalf("state = %s, want landed", st)
	}
	notes, envs := mergeNotes(t, a, task.ID)
	if len(notes) != 1 || notes[0].Action != mergeActionLanded || notes[0].MergeRequestID != mr.ID || !wakesPlanner(envs[0]) {
		t.Fatalf("notes = %+v", notes)
	}
	if al := mergeAlerts(t, a); len(al) != 0 {
		t.Fatalf("a landed merge raised alerts: %+v", al)
	}
	if got := wakeReason(envs); !strings.Contains(got, "notify.merge") || !strings.Contains(got, "merged and deployed") {
		t.Fatalf("wake reason = %q", got)
	}
}

// TestOperatorMergeFailureUnchanged: a request nobody's job owns keeps the
// old behavior exactly: alert, no fix job, no task note.
func TestOperatorMergeFailureUnchanged(t *testing.T) {
	a := newMergeApp(t)
	mr := queueMerge(t, a, "feat/by-hand")
	got := failMerge(t, a, mr, "checks red after rerun: TestX", "ci")
	if got.Failure != jobs.MergeFailureCI {
		t.Fatalf("failure = %q", got.Failure)
	}
	al := mergeAlerts(t, a)
	if len(al) != 1 || al[0].TaskID != "" || strings.Contains(al[0].Body, "Requested by job") {
		t.Fatalf("alerts = %+v", al)
	}
	all, _ := a.jobs.List(context.Background(), "", "", 100)
	for _, j := range all {
		if j.Spec.CreatedBy == jobs.MergeFixCreatedBy {
			t.Fatalf("a fix job was created for an operator request: %s", j.ID)
		}
	}
}

// TestMergeDependencyQueueing: "queue after branch X merges". The
// dependent never blocks the queue, does not dispatch while X is unlanded,
// its task hears (waking: coordination) when X's merge fails for good, and
// it dispatches by itself once a request for X lands.
func TestMergeDependencyQueueing(t *testing.T) {
	a := newMergeApp(t)
	uiJob, uiTask := ownedJob(t, a, "ui half")
	apiJob, _ := ownedJob(t, a, "api half")
	ui := ownMerge(t, a, uiJob.ID, "feat/ui", "feat/api") // older, gated
	if !reflect.DeepEqual(ui.WaitingOn, []string{"feat/api"}) {
		t.Fatalf("waiting_on = %v", ui.WaitingOn)
	}
	if id := pass(t, a); id != "" {
		t.Fatalf("dispatched %q with the dependency not even queued", id)
	}
	api := ownMerge(t, a, apiJob.ID, "feat/api")
	// The dependency fails in a way no job can fix (deploy): escalated.
	failMerge(t, a, api, "deploy: unit did not come up; rolled back: yes", "deploy")
	note, env := lastNote(t, a, uiTask.ID)
	if note.Action != mergeActionDependencyFailed || note.MergeRequestID != ui.ID || !wakesPlanner(env) ||
		!strings.Contains(note.Text, api.ID) || !reflect.DeepEqual(note.WaitingOn, []string{"feat/api"}) {
		t.Fatalf("dependent's note = %+v (wakes planner: %v)", note, wakesPlanner(env))
	}
	if id := pass(t, a); id != "" {
		t.Fatalf("dispatched %q after the dependency failed", id)
	}
	if st := getMerge(t, a, ui.ID).State; st != jobs.MergeQueued {
		t.Fatalf("dependent state = %s, want still queued", st)
	}
	// The dependency is re-requested and lands: the dependent goes next.
	api2 := ownMerge(t, a, apiJob.ID, "feat/api")
	if id := pass(t, a); id != api2.ID {
		t.Fatalf("dispatched %q, want %s", id, api2.ID)
	}
	finishJob(t, a, getMerge(t, a, api2.ID).JobID, wire.StatusDone)
	if id := pass(t, a); id != ui.ID {
		t.Fatalf("dispatched %q, want the released dependent %s", id, ui.ID)
	}
	assertOneMerging(t, a, 1)
}

// TestMergeDependencyFixInFlightIsQuiet: while the dependency's failure is
// being fixed automatically, the dependent's planner is not woken.
func TestMergeDependencyFixInFlightIsQuiet(t *testing.T) {
	a := newMergeApp(t)
	uiJob, uiTask := ownedJob(t, a, "ui half")
	apiJob, _ := ownedJob(t, a, "api half")
	ownMerge(t, a, uiJob.ID, "feat/ui", "feat/api")
	api := ownMerge(t, a, apiJob.ID, "feat/api")
	failMerge(t, a, api, "checks red after rerun: TestAPI", "ci")
	if notes, _ := mergeNotes(t, a, uiTask.ID); len(notes) != 0 {
		t.Fatalf("dependent was notified while a fix job is in flight: %+v", notes)
	}
}

// TestMergeDependencyStalled: a request waiting on a branch nobody queued
// raises ONE alert and one waking note after mergeBlockedAlertAfter.
func TestMergeDependencyStalled(t *testing.T) {
	a := newMergeApp(t)
	job, task := ownedJob(t, a, "depends on a ghost")
	mr := ownMerge(t, a, job.ID, "feat/ui", "feat/ghost")
	pass(t, a)
	if al := mergeAlerts(t, a); len(al) != 0 {
		t.Fatalf("alerted before the wait elapsed: %+v", al)
	}
	old := mergeBlockedAlertAfter
	mergeBlockedAlertAfter = 0
	t.Cleanup(func() { mergeBlockedAlertAfter = old })
	pass(t, a)
	pass(t, a)
	al := mergeAlerts(t, a)
	if len(al) != 1 || !strings.Contains(al[0].Title, mr.ID) || !strings.Contains(al[0].Body, "feat/ghost") ||
		al[0].TaskID != task.ID {
		t.Fatalf("alerts = %+v, want exactly one for the stalled request", al)
	}
	notes, envs := mergeNotes(t, a, task.ID)
	if len(notes) != 1 || notes[0].Action != mergeActionDependencyStall || !wakesPlanner(envs[0]) {
		t.Fatalf("notes = %+v", notes)
	}
	// A dependency that IS queued is progress, not a stall.
	b := newMergeApp(t)
	job2, _ := ownedJob(t, b, "depends on a queued branch")
	if err := b.jobs.SetMergePaused(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	ownMerge(t, b, job2.ID, "feat/ui", "feat/api")
	queueMerge(t, b, "feat/api")
	pass(t, b)
	if al := mergeAlerts(t, b); len(al) != 0 {
		t.Fatalf("alerted although the dependency is queued: %+v", al)
	}
}

// TestWakesPlannerMerge: notify.merge wakes only when threaded and marked
// waking; the wake reason tells the planner what is (not) its job.
func TestWakesPlannerMerge(t *testing.T) {
	mk := func(thread string, wake bool) wire.MessageEnvelope {
		p, _ := json.Marshal(mergeNote{Wake: wake, Action: mergeActionLanded})
		return wire.MessageEnvelope{Type: notifyMergeType, ThreadID: thread, Payload: p}
	}
	tests := []struct {
		name string
		env  wire.MessageEnvelope
		want bool
	}{
		{"threaded waking", mk("task_1", true), true},
		{"threaded record-only", mk("task_1", false), false},
		{"unthreaded", mk("", true), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := wakesPlanner(tc.env); got != tc.want {
				t.Fatalf("wakesPlanner = %v, want %v", got, tc.want)
			}
		})
	}
	// The job wake no longer tells the planner to queue merges.
	job := wakeReason([]wire.MessageEnvelope{{Type: notifyJobType}})
	for _, want := range []string{"own their work through to the merge", "do NOT call request_merge for it", "do NOT dispatch a merge or fix follow-up"} {
		if !strings.Contains(job, want) {
			t.Errorf("job wake reason lacks %q:\n%s", want, job)
		}
	}
	both := wakeReason([]wire.MessageEnvelope{{Type: notifyJobType}, mk("task_1", true)})
	if !strings.Contains(both, reasonJob) || !strings.Contains(both, reasonMerge) {
		t.Errorf("mixed wake reason = %q", both)
	}
}

// TestOwnsMergePinsCreators pins the worker harness's literal creator
// names (it cannot import these packages) to the real constants: merge
// jobs and auditor jobs must never be told to file their own merge, and
// merge-fix jobs must.
func TestOwnsMergePinsCreators(t *testing.T) {
	tests := []struct {
		createdBy string
		want      bool
	}{
		{jobs.MergeJobCreatedBy, false},
		{auditor.CreatedBy, false},
		{jobs.MergeFixCreatedBy, true},
		{"factory", true},
	}
	for _, tc := range tests {
		if got := workerharness.OwnsMerge(wire.NewJobSpec("x", tc.createdBy)); got != tc.want {
			t.Errorf("OwnsMerge(created_by %q) = %v, want %v", tc.createdBy, got, tc.want)
		}
	}
}
