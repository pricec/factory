package factoryapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

const (
	rdMerged = "89abcdef0123456789abcdef0123456789abcdef"
	rdRepo   = "pricec/media-curator"
)

func queueRepoMerge(t *testing.T, a *App, repo string) jobs.MergeRequest {
	t.Helper()
	mr, err := a.jobs.CreateMergeRequest(context.Background(), jobs.NewMergeRequest{Repo: repo, Branch: "feat/x", Head: mqHead, RequestedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return mr
}

func reportJob(t *testing.T, a *App, jobID, status, result string) {
	t.Helper()
	ctx := context.Background()
	if err := a.jobs.ApplyEvent(ctx, jobID, "started"); err != nil {
		t.Fatal(err)
	}
	res := wire.JobResult{JobID: jobID, Status: status, Summary: "s", Result: json.RawMessage(result)}
	if status == wire.StatusFailed {
		res.Error = &wire.JobError{Kind: wire.KindOther, Message: "deploy: health check failed; rolled back: yes"}
	}
	payload, _ := json.Marshal(res)
	a.handleEnvelope(ctx, wire.MessageEnvelope{Type: "job.result", JobID: jobID, Payload: payload}, map[string]bool{})
}

// mergeAndDispatch runs a media-curator request through its merge job and
// returns the request id and the deploy job the queue created.
func mergeAndDispatch(t *testing.T, a *App, mergeResult string) (string, jobs.Job) {
	t.Helper()
	ctx := context.Background()
	if err := a.jobs.UpsertForeman(ctx, "foreman:megatron", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mr := queueRepoMerge(t, a, rdRepo)
	pass(t, a)
	mergeJob := getMerge(t, a, mr.ID).JobID
	reportJob(t, a, mergeJob, wire.StatusDone, mergeResult)
	got := getMerge(t, a, mr.ID)
	if got.State != jobs.MergeMerging || got.JobID == mergeJob {
		t.Fatalf("after merge job done: state=%s job=%s (merge job %s); want merging on a new deploy job", got.State, got.JobID, mergeJob)
	}
	dj, ok, err := a.jobs.Get(ctx, got.JobID)
	if err != nil || !ok {
		t.Fatalf("deploy job: ok=%v err=%v", ok, err)
	}
	return mr.ID, dj
}

func TestRemoteDeployDispatchedPinnedToHookForeman(t *testing.T) {
	a := newMergeApp(t)
	id, dj := mergeAndDispatch(t, a, `{"merged":true,"merge_commit":"`+rdMerged+`"}`)
	if dj.Spec.Foreman != "foreman:megatron" {
		t.Fatalf("deploy job foreman = %q", dj.Spec.Foreman)
	}
	if !jobs.IsRemoteDeployJob(dj.Spec) || len(dj.Spec.Secrets) != 1 || dj.Spec.Secrets[0] != "github-pat" {
		t.Fatalf("spec not a remote deploy with github-pat: %+v", dj.Spec)
	}
	if !strings.Contains(dj.Spec.Task, rdMerged) || strings.Contains(dj.Spec.Task, "ghp_") {
		t.Fatalf("task must name the merged sha and carry no token:\n%s", dj.Spec.Task)
	}
	assertOneMerging(t, a, 1)

	reportJob(t, a, dj.ID, wire.StatusDone, `{"merged":true,"merge_commit":"`+rdMerged+`","deployed":true,"deployed_commit":"`+rdMerged+`","restart_deferred":true}`)
	got := getMerge(t, a, id)
	if got.State != jobs.MergeLanded || jobs.MergeNotDeployed(got) {
		t.Fatalf("want landed+deployed, got %s %q", got.State, got.Result)
	}
}

func TestRemoteDeployFailureIsDeployStage(t *testing.T) {
	a := newMergeApp(t)
	id, dj := mergeAndDispatch(t, a, `{"merged":true,"merge_commit":"`+rdMerged+`"}`)
	reportJob(t, a, dj.ID, wire.StatusFailed, `{"merged":true,"merge_commit":"`+rdMerged+`","deployed":false,"stage":"deploy"}`)
	got := getMerge(t, a, id)
	if got.State != jobs.MergeNeedsAttention || got.Failure != jobs.MergeFailureDeploy {
		t.Fatalf("want needs_attention/deploy, got %s/%s", got.State, got.Failure)
	}
	assertOneMerging(t, a, 0)
	if len(mergeAlerts(t, a)) == 0 {
		t.Fatal("no operator alert for a failed deploy")
	}
}

func TestRemoteDeployBadMergeCommitNeedsAttention(t *testing.T) {
	a := newMergeApp(t)
	ctx := context.Background()
	mr := queueRepoMerge(t, a, rdRepo)
	pass(t, a)
	reportJob(t, a, getMerge(t, a, mr.ID).JobID, wire.StatusDone, `{"merged":true,"merge_commit":"abc1234"}`)
	got := getMerge(t, a, mr.ID)
	if got.State != jobs.MergeNeedsAttention || got.Failure != jobs.MergeFailureDeploy {
		t.Fatalf("want needs_attention/deploy, got %s/%s", got.State, got.Failure)
	}
	_ = ctx
}

// An integration-host repo (or a remote repo whose merge job did not merge)
// keeps the old behavior: no deploy job.
func TestRemoteDeployNotDispatchedOtherwise(t *testing.T) {
	for _, tc := range []struct{ name, repo, result string }{
		{"integration hook repo", "pricec/movies-chat", `{"merged":true,"merge_commit":"` + rdMerged + `","deployed":true}`},
		{"unhooked repo", "pricec/other", `{"merged":true,"merge_commit":"` + rdMerged + `","deployed":false}`},
		{"remote repo, not merged", rdRepo, `{"merged":false,"reason":"conflict"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newMergeApp(t)
			mr := queueRepoMerge(t, a, tc.repo)
			pass(t, a)
			first := getMerge(t, a, mr.ID).JobID
			reportJob(t, a, first, wire.StatusDone, tc.result)
			got := getMerge(t, a, mr.ID)
			if got.State == jobs.MergeMerging || got.JobID != first {
				t.Fatalf("unexpected deploy dispatch: %s job %s (merge job %s)", got.State, got.JobID, first)
			}
		})
	}
}
