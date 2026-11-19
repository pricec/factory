package factoryapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
)

// fakeBranches answers Inspect from a branch -> state map; a missing
// branch is an inspection error.
type fakeBranches map[string]BranchState

func (f fakeBranches) Inspect(_ context.Context, _, _, branch string) (BranchState, error) {
	st, ok := f[branch]
	if !ok {
		return BranchState{}, errors.New("gh unavailable")
	}
	return st, nil
}

// staleMerge files a request and drives it to needs_attention with its
// operator alert raised.
func staleMerge(t *testing.T, a *App, branch string) jobs.MergeRequest {
	t.Helper()
	ctx := context.Background()
	mr := queueMerge(t, a, branch)
	if ok, err := a.jobs.ClaimMerge(ctx, mr.ID, "job_"+branch); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	mr, applied, err := a.jobs.TransitionMerge(ctx, mr.ID, jobs.MergeNeedsAttention, "ci red")
	if err != nil || !applied {
		t.Fatalf("transition: %v %v", applied, err)
	}
	a.alertMerge(ctx, mr, "test")
	return mr
}

func openAlertsFor(t *testing.T, a *App, id string) int {
	t.Helper()
	return a.countOpenMergeAlerts(context.Background(), id)
}

func TestDismissMerge(t *testing.T) {
	ctx := context.Background()
	table := []struct {
		name    string
		state   BranchState
		missing bool // inspector errors
		reason  string
		force   bool
		wantErr error
		wantMsg string
	}{
		{name: "merged branch", state: BranchState{Kind: BranchContained}, reason: "landed via #80"},
		{name: "deleted branch", state: BranchState{Kind: BranchDeleted}, reason: "superseded"},
		{name: "unmerged refused", state: BranchState{Kind: BranchUnmerged, Ahead: 3}, reason: "x", wantErr: ErrBranchUnmerged},
		{name: "unmerged forced", state: BranchState{Kind: BranchUnmerged, Ahead: 3}, reason: "abandoned", force: true, wantMsg: "forced"},
		{name: "unknown refused", missing: true, reason: "x", wantErr: ErrBranchUnknown},
		{name: "unknown forced", missing: true, reason: "x", force: true, wantMsg: "forced"},
		{name: "reason required", state: BranchState{Kind: BranchContained}, reason: "  ", wantMsg: "reason is required"},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			a := newMergeApp(t)
			mr := staleMerge(t, a, "feat/x")
			if !tc.missing {
				a.branches = fakeBranches{"feat/x": tc.state}
			} else {
				a.branches = fakeBranches{}
			}
			if openAlertsFor(t, a, mr.ID) != 1 {
				t.Fatalf("want 1 open alert before dismiss")
			}
			got, err := a.DismissMerge(ctx, mr.ID, jobs.MergeActorPlanner, tc.reason, tc.force)
			failed := tc.wantErr != nil || tc.reason == "  "
			if failed {
				if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) || (tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg)) {
					t.Fatalf("err = %v, want %v/%q", err, tc.wantErr, tc.wantMsg)
				}
				if s := getMerge(t, a, mr.ID).State; s != jobs.MergeNeedsAttention {
					t.Fatalf("state = %s after refusal", s)
				}
				if openAlertsFor(t, a, mr.ID) != 1 {
					t.Fatalf("alert resolved despite refusal")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.State != jobs.MergeCancelled || !strings.Contains(got.Result, "dismissed (planner") || !strings.Contains(got.Result, tc.reason) {
				t.Fatalf("got %s %q", got.State, got.Result)
			}
			if tc.wantMsg != "" && !strings.Contains(got.Result, tc.wantMsg) {
				t.Fatalf("result %q missing %q", got.Result, tc.wantMsg)
			}
			if openAlertsFor(t, a, mr.ID) != 0 {
				t.Fatalf("alert not resolved")
			}
			trail, err := a.jobs.MergeAuditTrail(ctx, mr.ID)
			if err != nil || len(trail) != 1 {
				t.Fatalf("audit = %v %v", trail, err)
			}
			if r := trail[0]; r.Actor != "planner" || r.Forced != tc.force || r.FromState != jobs.MergeNeedsAttention || r.ToState != jobs.MergeCancelled || r.Reason == "" {
				t.Fatalf("audit row = %+v", r)
			}
			// A second dismiss is refused: no longer needs_attention.
			if _, err := a.DismissMerge(ctx, mr.ID, "planner", "again", true); !errors.Is(err, jobs.ErrMergeNotDismissable) {
				t.Fatalf("second dismiss err = %v", err)
			}
		})
	}
}

func TestDismissMergeNotFoundAndWrongState(t *testing.T) {
	a := newMergeApp(t)
	a.branches = fakeBranches{"q": {Kind: BranchContained}}
	if _, err := a.DismissMerge(context.Background(), "mr_nope", "planner", "r", false); !errors.Is(err, jobs.ErrMergeNotFound) {
		t.Fatalf("err = %v", err)
	}
	q := queueMerge(t, a, "q")
	if _, err := a.DismissMerge(context.Background(), q.ID, "planner", "r", true); !errors.Is(err, jobs.ErrMergeNotDismissable) {
		t.Fatalf("queued dismiss err = %v", err)
	}
}

func TestMergeSweepPass(t *testing.T) {
	ctx := context.Background()
	a := newMergeApp(t)
	a.branches = fakeBranches{
		"merged":   {Kind: BranchContained},
		"gone":     {Kind: BranchDeleted},
		"unmerged": {Kind: BranchUnmerged, Ahead: 2},
		// "unknown" is absent: inspection errors.
	}
	ids := map[string]string{}
	for _, b := range []string{"merged", "gone", "unmerged", "unknown"} {
		ids[b] = staleMerge(t, a, b).ID
		// each needs its own merging slot: finish the claim above first.
	}
	// An unrelated alert must survive.
	a.raiseSystemAlert(ctx, 2, "unrelated", "", "test")

	res, err := a.MergeSweepPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cleared) != 2 || res.Kept != 2 || res.Alerts != 2 {
		t.Fatalf("sweep = %+v", res)
	}
	want := map[string]string{"merged": jobs.MergeCancelled, "gone": jobs.MergeCancelled, "unmerged": jobs.MergeNeedsAttention, "unknown": jobs.MergeNeedsAttention}
	for b, state := range want {
		if got := getMerge(t, a, ids[b]).State; got != state {
			t.Errorf("%s: state = %s, want %s", b, got, state)
		}
	}
	if openAlertsFor(t, a, ids["merged"]) != 0 || openAlertsFor(t, a, ids["unmerged"]) != 1 {
		t.Errorf("alert resolution wrong")
	}
	open, _ := a.tasks.ListAlerts(ctx, tasks.AlertFilter{States: []string{tasks.AlertOpen}})
	found := false
	for _, al := range open {
		found = found || al.Title == "unrelated"
	}
	if !found {
		t.Errorf("unrelated alert was resolved")
	}
	trail, _ := a.jobs.MergeAuditTrail(ctx, ids["gone"])
	if len(trail) != 1 || trail[0].Actor != jobs.MergeActorSweep || trail[0].Forced {
		t.Errorf("audit = %+v", trail)
	}
	// Idempotent: nothing more to clear.
	if res, _ := a.MergeSweepPass(ctx); len(res.Cleared) != 0 {
		t.Errorf("second sweep cleared %v", res.Cleared)
	}
}

func TestDismissAndSweepHTTP(t *testing.T) {
	a := newMergeApp(t)
	a.branches = fakeBranches{"a": {Kind: BranchUnmerged, Ahead: 1}, "b": {Kind: BranchContained}}
	ma, mb := staleMerge(t, a, "a"), staleMerge(t, a, "b")
	do := func(path, body string, handler http.HandlerFunc, id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	if w := do("/x", `{"reason":"r"}`, a.handleDismissMerge, ma.ID); w.Code != http.StatusConflict {
		t.Errorf("unmerged dismiss = %d %s", w.Code, w.Body)
	}
	if w := do("/x", `{"reason":""}`, a.handleDismissMerge, ma.ID); w.Code != http.StatusBadRequest {
		t.Errorf("no reason = %d %s", w.Code, w.Body)
	}
	if w := do("/x", `{"reason":"r","force":true}`, a.handleDismissMerge, ma.ID); w.Code != http.StatusOK {
		t.Errorf("forced = %d %s", w.Code, w.Body)
	}
	if w := do("/x", `{"reason":"r"}`, a.handleDismissMerge, "mr_nope"); w.Code != http.StatusNotFound {
		t.Errorf("missing = %d", w.Code)
	}
	w := do("/sweep", ``, a.handleMergeSweep, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), mb.ID) {
		t.Errorf("sweep = %d %s", w.Code, w.Body)
	}
}
