// Dismissing and sweeping needs_attention merge requests (2026-11-19
// amendment, spec §6.11). A needs_attention request is terminal, and the
// cancel API refuses it, so stale rows (work merged by hand, superseded by
// another branch, or an empty branch) piled up for the operator. Two paths
// close them, both to cancelled with an audit row, and both ack the
// request's open alerts:
//
//   - DismissMerge (planner dismiss_merge tool, POST
//     /v1/merge-requests/{id}/dismiss): reason required; refused while the
//     branch has commits the base branch lacks unless force=true;
//   - MergeSweepPass (loop every mergeSweepTick, POST
//     /v1/merge-requests/sweep): closes requests whose branch is merged
//     into the base, deleted, or empty. Anything it cannot verify stays.
package factoryapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
)

// mergeSweepTick is the cadence of the needs_attention sweep. A var so
// tests can shorten it.
var mergeSweepTick = 10 * time.Minute

// Branch kinds reported by a BranchInspector.
const (
	// BranchDeleted: the branch no longer exists on the remote.
	BranchDeleted = "deleted"
	// BranchContained: the branch has no commits the base lacks (already
	// merged, or empty).
	BranchContained = "contained"
	// BranchUnmerged: the branch has commits the base lacks.
	BranchUnmerged = "unmerged"
)

// BranchState is a branch's standing against its base branch.
type BranchState struct {
	Kind string
	// Ahead is the number of commits on the branch the base lacks
	// (BranchUnmerged).
	Ahead int
}

// BranchInspector reports a branch's standing against base for a repo
// (gh OWNER/REPO form). An error means unknown: callers never close on it.
type BranchInspector interface {
	Inspect(ctx context.Context, repo, base, branch string) (BranchState, error)
}

// ghInspector asks the GitHub compare API through the gh CLI.
type ghInspector struct{}

func (ghInspector) run(ctx context.Context, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

func (g ghInspector) Inspect(ctx context.Context, repo, base, branch string) (BranchState, error) {
	out, stderr, err := g.run(ctx, "api", "repos/"+repo+"/compare/"+base+"..."+branch, "--jq", ".ahead_by")
	if err != nil {
		if !strings.Contains(stderr, "404") && !strings.Contains(stderr, "Not Found") {
			return BranchState{}, fmt.Errorf("gh compare %s: %v: %s", branch, err, stderr)
		}
		// 404: the branch is gone - or the base/repo is unreachable. Only
		// call it deleted when the base resolves.
		if _, stderr2, err2 := g.run(ctx, "api", "repos/"+repo+"/branches/"+base, "--jq", ".name"); err2 != nil {
			return BranchState{}, fmt.Errorf("gh base %s: %v: %s", base, err2, stderr2)
		}
		return BranchState{Kind: BranchDeleted}, nil
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return BranchState{}, fmt.Errorf("gh compare %s: unexpected ahead_by %q", branch, out)
	}
	if n == 0 {
		return BranchState{Kind: BranchContained}, nil
	}
	return BranchState{Kind: BranchUnmerged, Ahead: n}, nil
}

// Dismiss errors (the HTTP and MCP surfaces map them).
var (
	// ErrBranchUnmerged: the branch still has commits the base lacks.
	ErrBranchUnmerged = errors.New("factoryapp: branch has unmerged commits")
	// ErrBranchUnknown: the branch state could not be determined.
	ErrBranchUnknown = errors.New("factoryapp: branch state unknown")
)

// inspect resolves mr's branch standing against its repo's base branch.
func (a *App) inspect(ctx context.Context, mr jobs.MergeRequest) (BranchState, error) {
	repo := mr.Repo
	if repo == "" {
		repo = jobs.DefaultMergeRepo
	}
	return a.branches.Inspect(ctx, repo, jobs.MergeBaseBranch(repo), mr.Branch)
}

// DismissMerge closes the needs_attention request id as cancelled with
// reason (required), writes the audit row and acks its open alerts. The
// branch must have no commits the base branch lacks (merged, empty or
// deleted); force=true skips that check and is recorded in the audit row.
// Without force an undeterminable branch state is refused too.
func (a *App) DismissMerge(ctx context.Context, id, actor, reason string, force bool) (jobs.MergeRequest, error) {
	if strings.TrimSpace(reason) == "" {
		return jobs.MergeRequest{}, fmt.Errorf("dismiss_merge: reason is required")
	}
	mr, ok, err := a.jobs.GetMergeRequest(ctx, id)
	if err != nil {
		return jobs.MergeRequest{}, err
	}
	if !ok {
		return jobs.MergeRequest{}, fmt.Errorf("%w: %s", jobs.ErrMergeNotFound, id)
	}
	if mr.State != jobs.MergeNeedsAttention {
		return jobs.MergeRequest{}, fmt.Errorf("%w: %s is %s", jobs.ErrMergeNotDismissable, id, mr.State)
	}
	forced := false
	st, ierr := a.inspect(ctx, mr)
	switch {
	case ierr != nil && !force:
		return jobs.MergeRequest{}, fmt.Errorf("%w: %v (use force=true with a reason to dismiss anyway)", ErrBranchUnknown, ierr)
	case ierr != nil:
		forced = true
		reason += " [forced; branch state unknown: " + ierr.Error() + "]"
	case st.Kind == BranchUnmerged:
		if !force {
			return jobs.MergeRequest{}, fmt.Errorf("%w: %s has %d commit(s) not in the base branch (use force=true with a reason to dismiss anyway)",
				ErrBranchUnmerged, mr.Branch, st.Ahead)
		}
		forced = true
		reason += fmt.Sprintf(" [forced; %d unmerged commit(s)]", st.Ahead)
	default:
		reason += " [branch " + st.Kind + "]"
	}
	return a.closeMerge(ctx, mr, actor, reason, forced)
}

// closeMerge dismisses mr in the store and acks its alerts.
func (a *App) closeMerge(ctx context.Context, mr jobs.MergeRequest, actor, reason string, forced bool) (jobs.MergeRequest, error) {
	got, err := a.jobs.DismissMergeRequest(ctx, mr.ID, actor, reason, forced)
	if err != nil {
		return jobs.MergeRequest{}, err
	}
	n := a.resolveMergeAlerts(ctx, got.ID)
	log.Printf("factoryapp: merge queue: %s dismissed by %s (forced=%v, %d alert(s) resolved): %s", got.ID, actor, forced, n, reason)
	a.kickMergeQueue()
	return got, nil
}

// resolveMergeAlerts acks every open alert whose title names request id
// (the needs_attention alert, alertMerge) and returns how many it acked.
func (a *App) resolveMergeAlerts(ctx context.Context, id string) int {
	open, err := a.tasks.ListAlerts(ctx, tasks.AlertFilter{States: []string{tasks.AlertOpen}})
	if err != nil {
		log.Printf("factoryapp: merge queue: list alerts for %s: %v", id, err)
		return 0
	}
	n := 0
	for _, al := range open {
		if !strings.Contains(al.Title, id) {
			continue
		}
		acked, err := a.tasks.TransitionAlert(ctx, al.ID, []string{tasks.AlertOpen}, tasks.AlertAcked)
		if err != nil {
			continue // raced to another state; nothing to resolve
		}
		a.publishAlertChanged(acked)
		n++
	}
	return n
}

// MergeSweepResult is what one sweep did.
type MergeSweepResult struct {
	// Cleared lists the request ids closed.
	Cleared []string `json:"cleared"`
	// Kept counts needs_attention requests left alone (unmerged branch, or
	// state unknown).
	Kept int `json:"kept"`
	// Alerts is how many alerts were acked.
	Alerts int `json:"alerts_resolved"`
}

// MergeSweepPass closes every needs_attention request whose branch is
// merged into its base, deleted or empty, acking the matching alerts. A
// request whose branch cannot be inspected stays. Exported for tests, the
// loop and POST /v1/merge-requests/sweep.
func (a *App) MergeSweepPass(ctx context.Context) (MergeSweepResult, error) {
	res := MergeSweepResult{Cleared: []string{}}
	stale, err := a.jobs.NeedsAttentionMerges(ctx)
	if err != nil {
		return res, err
	}
	for _, mr := range stale {
		st, err := a.inspect(ctx, mr)
		if err != nil {
			log.Printf("factoryapp: merge sweep: %s (%s): %v (kept)", mr.ID, mr.Branch, err)
			res.Kept++
			continue
		}
		if st.Kind == BranchUnmerged {
			res.Kept++
			continue
		}
		before := a.countOpenMergeAlerts(ctx, mr.ID)
		if _, err := a.closeMerge(ctx, mr, jobs.MergeActorSweep, "branch "+mr.Branch+" is "+st.Kind+" against the base branch", false); err != nil {
			log.Printf("factoryapp: merge sweep: close %s: %v", mr.ID, err)
			res.Kept++
			continue
		}
		res.Cleared = append(res.Cleared, mr.ID)
		res.Alerts += before
	}
	return res, nil
}

func (a *App) countOpenMergeAlerts(ctx context.Context, id string) int {
	open, err := a.tasks.ListAlerts(ctx, tasks.AlertFilter{States: []string{tasks.AlertOpen}})
	if err != nil {
		return 0
	}
	n := 0
	for _, al := range open {
		if strings.Contains(al.Title, id) {
			n++
		}
	}
	return n
}

// mergeSweepLoop sweeps once at start and then every mergeSweepTick.
func (a *App) mergeSweepLoop(ctx context.Context) {
	ticker := time.NewTicker(mergeSweepTick)
	defer ticker.Stop()
	for {
		if r, err := a.MergeSweepPass(ctx); err != nil && ctx.Err() == nil {
			log.Printf("factoryapp: merge sweep: %v", err)
		} else if len(r.Cleared) > 0 {
			log.Printf("factoryapp: merge sweep: cleared %d: %v", len(r.Cleared), r.Cleared)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// handleDismissMerge serves POST /v1/merge-requests/{id}/dismiss
// {reason, force?}.
func (a *App) handleDismissMerge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
		Force  bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "malformed body: "+err.Error())
		return
	}
	mr, err := a.DismissMerge(r.Context(), r.PathValue("id"), jobs.MergeActorOperator, in.Reason, in.Force)
	if err != nil {
		writeDismissError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mr)
}

// handleMergeSweep serves POST /v1/merge-requests/sweep: one sweep now.
func (a *App) handleMergeSweep(w http.ResponseWriter, r *http.Request) {
	res, err := a.MergeSweepPass(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func writeDismissError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, jobs.ErrMergeNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, jobs.ErrMergeNotDismissable), errors.Is(err, ErrBranchUnmerged):
		writeError(w, http.StatusConflict, "not_dismissable", err.Error())
	case errors.Is(err, ErrBranchUnknown):
		writeError(w, http.StatusBadGateway, "branch_unknown", err.Error())
	case jobs.IsBadMergeRequest(err) || strings.Contains(err.Error(), "reason is required"):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
