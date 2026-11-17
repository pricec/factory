// Package cigate is the merge procedure's CI gate (docs/merging.md step 5)
// as deterministic code. For a head SHA it reads the GitHub Actions runs
// and never trusts a red verdict that already exists: a completed-red head
// is re-run (`gh run rerun <id> --failed`) first, and only a red result
// from that fresh attempt is final. A one-off flake therefore cannot kill
// a merge, and a re-queued request at the same head cannot re-read the
// previous request's failed run. At most one rerun is made per Check.
// The Result records whether a rerun happened and which tests failed in
// each attempt, so real flakes stay visible.
package cigate

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Verdicts.
const (
	Green    = "green"
	Red      = "red"       // red after the rerun
	NoChecks = "no_checks" // no workflow run exists for the head
	Timeout  = "timeout"   // runs never settled in time
)

// GH runs `gh <args>` and returns stdout. Tests mock it.
type GH interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Run is one workflow run as `gh run list --json` reports it (latest attempt).
type Run struct {
	ID         int64  `json:"databaseId"`
	Name       string `json:"workflowName"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Attempt    int    `json:"attempt"`
}

func (r Run) completed() bool { return r.Status == "completed" }

func (r Run) red() bool {
	if !r.completed() {
		return false
	}
	switch r.Conclusion {
	case "success", "skipped", "neutral":
		return false
	}
	return true
}

// Result is the gate's verdict, printed as JSON for the merge report.
type Result struct {
	Verdict      string   `json:"verdict"`
	Head         string   `json:"head"`
	Rerun        bool     `json:"rerun"`
	RerunRuns    []int64  `json:"rerun_runs,omitempty"`
	FirstFailed  []string `json:"first_failed,omitempty"`  // failures before the rerun
	SecondFailed []string `json:"second_failed,omitempty"` // failures after the rerun
	FailedBoth   []string `json:"failed_both,omitempty"`   // red in both attempts
	Flaked       []string `json:"flaked,omitempty"`        // red first, green on rerun
	Summary      string   `json:"summary"`
}

// Gate checks one head.
type Gate struct {
	GH       GH
	Repo     string
	Interval time.Duration // poll interval (default 30s)
	MaxPolls int           // polls per wait (default 120, i.e. 1h at 30s)
	Sleep    func(context.Context, time.Duration)
}

func (g *Gate) interval() time.Duration {
	if g.Interval > 0 {
		return g.Interval
	}
	return 30 * time.Second
}

func (g *Gate) maxPolls() int {
	if g.MaxPolls > 0 {
		return g.MaxPolls
	}
	return 120
}

func (g *Gate) sleep(ctx context.Context) {
	if g.Sleep != nil {
		g.Sleep(ctx, g.interval())
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(g.interval()):
	}
}

func (g *Gate) list(ctx context.Context, sha string) ([]Run, error) {
	out, err := g.GH.Run(ctx, "run", "list", "-R", g.Repo, "--commit", sha, "--limit", "100",
		"--json", "databaseId,workflowName,status,conclusion,attempt")
	if err != nil {
		return nil, fmt.Errorf("gh run list: %w", err)
	}
	var runs []Run
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, fmt.Errorf("gh run list: bad json: %w", err)
	}
	return runs, nil
}

// settle polls until runs exist and every one is completed. minAttempt
// holds, per run id, the attempt that must be exceeded (set for reruns, so
// the pre-rerun completed state is never read as the new result).
func (g *Gate) settle(ctx context.Context, sha string, minAttempt map[int64]int) ([]Run, string, error) {
	for i := 0; i < g.maxPolls(); i++ {
		runs, err := g.list(ctx, sha)
		if err != nil {
			return nil, "", err
		}
		switch {
		case len(runs) == 0:
			if i >= 4 { // give GitHub a few polls to create the runs
				return nil, NoChecks, nil
			}
		default:
			done := true
			for _, r := range runs {
				if !r.completed() || r.Attempt <= minAttempt[r.ID] && minAttempt[r.ID] > 0 {
					done = false
				}
			}
			if done {
				return runs, "", nil
			}
		}
		g.sleep(ctx)
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
	}
	return nil, Timeout, nil
}

var failRE = regexp.MustCompile(`--- FAIL: (\S+)`)

// failures names what failed in the red runs: failing Go tests from the
// failed-step logs, or "run:<workflow>" when no test names are found
// (build, vet, formatting).
func (g *Gate) failures(ctx context.Context, runs []Run) ([]string, error) {
	set := map[string]bool{}
	for _, r := range runs {
		if !r.red() {
			continue
		}
		out, err := g.GH.Run(ctx, "run", "view", strconv.FormatInt(r.ID, 10), "-R", g.Repo, "--log-failed")
		if err != nil {
			return nil, fmt.Errorf("gh run view %d --log-failed: %w", r.ID, err)
		}
		found := false
		for _, m := range failRE.FindAllStringSubmatch(string(out), -1) {
			set[m[1]] = true
			found = true
		}
		if !found {
			set["run:"+r.Name] = true
		}
	}
	return sorted(set), nil
}

func sorted(set map[string]bool) []string {
	var s []string
	for k := range set {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

func redRuns(runs []Run) []Run {
	var out []Run
	for _, r := range runs {
		if r.red() {
			out = append(out, r)
		}
	}
	return out
}

// Check returns the gate verdict for sha. Existing red runs are never
// final: they are re-run once and the fresh attempt decides.
func (g *Gate) Check(ctx context.Context, sha string) (Result, error) {
	res := Result{Head: sha}
	runs, v, err := g.settle(ctx, sha, nil)
	if err != nil {
		return res, err
	}
	if v != "" {
		res.Verdict, res.Summary = v, fmt.Sprintf("checks %s for %s", v, sha)
		return res, nil
	}
	red := redRuns(runs)
	if len(red) == 0 {
		res.Verdict, res.Summary = Green, "checks green, no rerun needed"
		return res, nil
	}
	if res.FirstFailed, err = g.failures(ctx, runs); err != nil {
		return res, err
	}

	// Rerun the failed jobs of every red run, once, and wait for them.
	prior := map[int64]int{}
	for _, r := range red {
		prior[r.ID] = r.Attempt
		if _, err := g.GH.Run(ctx, "run", "rerun", strconv.FormatInt(r.ID, 10), "-R", g.Repo, "--failed"); err != nil {
			return res, fmt.Errorf("gh run rerun %d --failed: %w", r.ID, err)
		}
		res.RerunRuns = append(res.RerunRuns, r.ID)
	}
	res.Rerun = true
	runs, v, err = g.settle(ctx, sha, prior)
	if err != nil {
		return res, err
	}
	if v != "" {
		res.Verdict = v
		res.Summary = fmt.Sprintf("checks %s after rerun of %v; first attempt failed: %s", v, res.RerunRuns, strings.Join(res.FirstFailed, ", "))
		return res, nil
	}
	if len(redRuns(runs)) == 0 {
		res.Verdict, res.Flaked = Green, res.FirstFailed
		res.Summary = "checks green after one rerun (flake); failed on first attempt: " + strings.Join(res.FirstFailed, ", ")
		return res, nil
	}
	if res.SecondFailed, err = g.failures(ctx, runs); err != nil {
		return res, err
	}
	first := map[string]bool{}
	for _, f := range res.FirstFailed {
		first[f] = true
	}
	both := map[string]bool{}
	for _, f := range res.SecondFailed {
		if first[f] {
			both[f] = true
		}
	}
	res.Verdict, res.FailedBoth = Red, sorted(both)
	res.Summary = fmt.Sprintf("checks red after one rerun; failed both times: [%s]; first: [%s]; second: [%s]",
		strings.Join(res.FailedBoth, ", "), strings.Join(res.FirstFailed, ", "), strings.Join(res.SecondFailed, ", "))
	return res, nil
}
