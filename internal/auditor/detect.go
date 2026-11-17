package auditor

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// DetectOptions tune the detectors.
type DetectOptions struct {
	WorktreeRoot string
	StaleAfter   time.Duration
	// MaxLines caps the line estimate of the PR plans the detectors
	// propose (CheckPlan re-checks it anyway).
	MaxLines int
}

// KnownIssues are issues already being handled elsewhere (spec §6.12):
// matching findings are reported with a note and never auto-fixed.
var KnownIssues = []struct {
	Match *regexp.Regexp
	Note  string
}{
	{
		regexp.MustCompile(`(?i)report_result|error\.kind|error field|error: required when status`),
		"report_result rejecting the `error` field is being fixed on fix/report-result-typed-schema-v2.",
	},
	{
		regexp.MustCompile(`(?i)conflict`),
		"merge-request jobs failing on conflicts with master is a known issue.",
	},
}

func knownNote(texts ...string) string {
	for _, k := range KnownIssues {
		for _, t := range texts {
			if k.Match.MatchString(t) {
				return k.Note
			}
		}
	}
	return ""
}

// Detect runs every deterministic detector over snap. Findings carry a
// stable fingerprint and a metric; detectors that see an obviously
// reversible fix attach an auto-fix Plan (still subject to CheckPlan).
func Detect(snap Snapshot, o DetectOptions) []Finding {
	if o.StaleAfter <= 0 {
		o.StaleAfter = 7 * 24 * time.Hour
	}
	if o.MaxLines <= 0 || o.MaxLines > HardMaxChangedLines {
		o.MaxLines = HardMaxChangedLines
	}
	var fs []Finding
	for _, d := range []func(Snapshot, DetectOptions) []Finding{
		detectFailureTrend, detectRecurringErrors, detectLowTimeouts, detectStuck,
		detectHeld, detectThrottleBypass, detectOverBudget, detectRetries, detectTierSpend,
		detectMergeOutcomes, detectForemen, detectWorktrees,
	} {
		fs = append(fs, d(snap, o)...)
	}
	fs = dropCoveredErrors(fs)
	for i := range fs {
		fs[i].Source = "detector"
		if fs[i].Known == "" {
			fs[i].Known = knownNote(append([]string{fs[i].Title}, fs[i].Evidence...)...)
		}
		if fs[i].Known != "" {
			fs[i].AutoFix = nil
		}
	}
	SortFindings(fs)
	return fs
}

// dropCoveredErrors removes recurring-error findings whose jobs are all
// already explained by a more specific finding (the low-timeout one;
// generic roll-ups such as the failure rate never cover a cause).
func dropCoveredErrors(fs []Finding) []Finding {
	var out []Finding
	for i, f := range fs {
		covered := false
		if strings.HasPrefix(f.Fingerprint, "error:") && len(f.JobIDs) > 0 {
			for j, g := range fs {
				if i != j && strings.HasPrefix(g.Fingerprint, "timeout:") && subset(f.JobIDs, g.JobIDs) {
					covered = true
					break
				}
			}
		}
		if !covered {
			out = append(out, f)
		}
	}
	return out
}

func subset(a, b []string) bool {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	for _, x := range a {
		if !in[x] {
			return false
		}
	}
	return true
}

func in24h(snap Snapshot, t time.Time) bool { return snap.Now.Sub(t) <= Window24h }

func failedState(s string) bool {
	return s == "failed" || s == "timed_out" || s == "dead"
}

// detectFailureTrend: the 24h failure rate against the 7-day baseline.
func detectFailureTrend(snap Snapshot, _ DetectOptions) []Finding {
	var n24, f24, n7, f7 int
	var ids []string
	for _, j := range snap.Jobs {
		if j.State == "held" || j.State == "pending" || j.Kind == "audit" || j.Kind == "audit_fix" {
			continue
		}
		n7++
		failed := failedState(j.State)
		if failed {
			f7++
		}
		if in24h(snap, j.CreatedAt) {
			n24++
			if failed {
				f24++
				ids = append(ids, j.ID)
			}
		}
	}
	if f24 < 3 || n24 == 0 {
		return nil
	}
	r24 := float64(f24) / float64(n24)
	base := float64(f7-f24) / float64(max(n7-n24, 1))
	if r24 < 0.2 && r24 < 2*base {
		return nil
	}
	return []Finding{{
		Fingerprint: fingerprint("failure_rate", "all"),
		Title:       fmt.Sprintf("Job failure rate %.0f%% in the last 24h (7-day baseline %.0f%%)", 100*r24, 100*base),
		Evidence: []string{
			fmt.Sprintf("24h: %d of %d jobs failed, timed out or went dead", f24, n24),
			fmt.Sprintf("previous 6 days: %d of %d", f7-f24, n7-n24),
		},
		JobIDs:      ids,
		Metric:      float64(f24),
		MetricLabel: "failed jobs in 24h",
		Cause:       "See the recurring-error and timeout findings of the same audit for the dominant causes.",
		Fix:         "Address the top recurring error first; re-run the failed jobs after the fix lands.",
		Benefit:     fmt.Sprintf("Up to %d fewer failed jobs per day and the rework they cause.", f24),
		Effort:      "Depends on the dominant cause (see related findings).",
	}}
}

var (
	normIDRe  = regexp.MustCompile(`\b(job|task|msg|mr|sched)_[0-9A-Z]{10,}\b`)
	normHexRe = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	normNumRe = regexp.MustCompile(`\d+(\.\d+)?`)
	normWSRe  = regexp.MustCompile(`\s+`)
)

// NormalizeError reduces an error string to its recurring shape: first
// line only, ids/shas/numbers replaced, whitespace collapsed, 120 chars.
func NormalizeError(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = normIDRe.ReplaceAllString(s, "${1}_<id>")
	s = normHexRe.ReplaceAllString(s, "<sha>")
	s = normNumRe.ReplaceAllString(s, "N")
	s = normWSRe.ReplaceAllString(s, " ")
	return clip(strings.TrimSpace(s), 120)
}

// errorHints maps error shapes to a cause and fix.
var errorHints = []struct {
	match               *regexp.Regexp
	cause, fix, benefit string
}{
	{
		regexp.MustCompile(`(?i)executable file not found`),
		"The job was dispatched to a foreman whose host lacks the worker CLI it needs (its capability tags do not list it, but nothing in the request required it).",
		"Make the matcher require the runner's tool for worker jobs (e.g. tools:opencode / tools:claude), or install the CLI on that foreman.",
		"No more jobs lost to hosts that cannot run them.",
	},
	{
		regexp.MustCompile(`(?i)^opencode run \[message`),
		"The worker launched opencode with arguments that version rejects (it printed its usage text instead of running): the foreman's opencode CLI and the harness invocation disagree.",
		"Pin/upgrade opencode on the affected foremen, or adapt the harness invocation to that CLI version.",
		"Every job on the affected foremen stops crashing at launch.",
	},
	{
		regexp.MustCompile(`(?i)exceeded timeout_s`),
		"Jobs ran into their wall-clock timeout.",
		"See the low-timeout finding; raise the timeout for that kind of work.",
		"Fewer killed jobs and less wasted spend.",
	},
}

// detectRecurringErrors clusters normalized error strings of the window.
func detectRecurringErrors(snap Snapshot, _ DetectOptions) []Finding {
	type agg struct {
		sample   string
		ids      []string
		n24, n7  int
		foremen  map[string]bool
		lastSeen time.Time
	}
	groups := map[string]*agg{}
	for _, j := range snap.Jobs {
		seen := map[string]bool{}
		for _, e := range j.Errors {
			k := NormalizeError(e)
			if k == "" || seen[k] || strings.HasPrefix(k, "cancelled") || strings.HasPrefix(k, "superseded") {
				continue
			}
			seen[k] = true
			g := groups[k]
			if g == nil {
				g = &agg{sample: e, foremen: map[string]bool{}}
				groups[k] = g
			}
			g.ids = append(g.ids, j.ID)
			g.n7++
			if in24h(snap, j.UpdatedAt) {
				g.n24++
			}
			if j.ForemanID != "" {
				g.foremen[j.ForemanID] = true
			}
		}
	}
	var fs []Finding
	for k, g := range groups {
		if g.n7 < 2 {
			continue
		}
		var foremen []string
		for f := range g.foremen {
			foremen = append(foremen, f)
		}
		sort.Strings(foremen)
		f := Finding{
			Fingerprint: fingerprint("error", k),
			Title:       fmt.Sprintf("Recurring job error (%dx in 7d): %s", g.n7, clip(k, 80)),
			Evidence: []string{
				fmt.Sprintf("%d jobs in 7 days, %d in the last 24h", g.n7, g.n24),
				"Sample: " + clip(strings.Join(strings.Fields(g.sample), " "), 300),
			},
			JobIDs:      g.ids,
			Metric:      float64(g.n7),
			MetricLabel: "jobs with this error in 7d",
			Cause:       "A recurring failure mode; the same error shape keeps coming back.",
			Fix:         "Investigate the sample jobs and fix the root cause.",
			Benefit:     fmt.Sprintf("~%d fewer failed jobs per week.", g.n7),
			Effort:      "Small to medium, depending on the cause.",
		}
		if len(foremen) > 0 {
			f.Evidence = append(f.Evidence, "Foremen: "+strings.Join(foremen, ", "))
		}
		for _, h := range errorHints {
			if h.match.MatchString(g.sample) {
				f.Cause, f.Fix, f.Benefit = h.cause, h.fix, h.benefit
				break
			}
		}
		fs = append(fs, f)
	}
	return fs
}

// detectLowTimeouts: jobs killed by a timeout_s far below their tier's
// ceiling - the dispatcher picked a timeout too small for the work.
func detectLowTimeouts(snap Snapshot, o DetectOptions) []Finding {
	var ids []string
	var lowest int
	n24 := 0
	for _, j := range snap.Jobs {
		if j.State != "timed_out" && !(j.State == "failed" && anyContains(j.Errors, "exceeded timeout_s")) {
			continue
		}
		ceil := wire.TierCeilingS(j.Tier)
		if j.TimeoutS == 0 || j.TimeoutS*4 > ceil {
			continue
		}
		ids = append(ids, j.ID)
		if lowest == 0 || j.TimeoutS < lowest {
			lowest = j.TimeoutS
		}
		if in24h(snap, j.UpdatedAt) {
			n24++
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return []Finding{{
		Fingerprint: fingerprint("timeout", "below_tier_ceiling"),
		Title:       fmt.Sprintf("%d jobs killed by a timeout_s far below their tier ceiling", len(ids)),
		Evidence: []string{
			fmt.Sprintf("%d jobs in 7 days (%d in 24h) timed out with timeout_s <= 1/4 of the tier ceiling; lowest %ds", len(ids), n24, lowest),
			"Tier ceilings: flash 3600s, default 7200s, hardest 14400s",
		},
		JobIDs:      ids,
		Metric:      float64(len(ids)),
		MetricLabel: "jobs timed out below 1/4 of the tier ceiling in 7d",
		Cause:       "The planner passes an explicit small timeout_s (e.g. 300s) for build/code work; create_job's guidance does not warn against it.",
		Fix:         "Prompt fix: tell the planner in the create_job tool description to leave timeout_s unset (tier ceiling) for code changes and never go below 1800s for builds.",
		Benefit:     "No more code jobs killed mid-build; saves the wasted partial runs and their re-dispatch.",
		Effort:      "Tiny: a few lines in the create_job tool description.",
		AutoFix: &Plan{
			Kind:        FixPR,
			Description: "In internal/mcpfactory/tools.go, extend the create_job tool description (and only the description string): leave timeout_s unset for code/build jobs so the tier ceiling applies, and never set it below 1800 for work that runs builds or tests. Keep the addition short: the MCP surface has a description size test.",
			Paths:       []string{"internal/mcpfactory/tools.go"},
			EstLines:    min(10, o.MaxLines),
			Revert:      "Revert the merge commit of the fix branch on a new branch (`git revert -m 1 <merge sha>`) and send it through the merge queue; it only changes a tool description string.",
		},
	}}
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// detectStuck: jobs still running far past their timeout (the sweeper
// should have timed them out) or dispatched/running with a recorded error.
func detectStuck(snap Snapshot, _ DetectOptions) []Finding {
	var ids []string
	var ev []string
	for _, j := range snap.Jobs {
		if j.State != "running" && j.State != "dispatched" {
			continue
		}
		age := snap.Now.Sub(j.UpdatedAt)
		past := j.TimeoutS > 0 && age > time.Duration(j.TimeoutS)*time.Second+time.Hour
		if past || (j.Attempts > 0 && j.LastError != "") {
			ids = append(ids, j.ID)
			ev = append(ev, fmt.Sprintf("%s %s on %s for %s (timeout %ds, attempts %d): %s",
				j.ID, j.State, j.ForemanID, age.Round(time.Minute), j.TimeoutS, j.Attempts, clip(NormalizeError(j.LastError), 80)))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return []Finding{{
		Fingerprint: fingerprint("stuck", "running_with_error"),
		Title:       fmt.Sprintf("%d jobs shown as running although they failed or overran", len(ids)),
		Evidence:    limit(ev, 8),
		JobIDs:      ids,
		Metric:      float64(len(ids)),
		MetricLabel: "stuck running jobs",
		Cause:       "A retry re-dispatched the job but the worker died before reporting (launch crash), or the sweeper missed the timeout, so the state never left running.",
		Fix:         "Make the foreman report a synthesized failed result when the worker exits before reporting on a retry, and have the sweeper time out running jobs past timeout_s + grace; cancel the listed jobs now.",
		Benefit:     "Accurate job state; dead work stops counting against concurrency caps.",
		Effort:      "Small-medium (foreman harness + sweeper).",
	}}
}

// detectHeld: long-held jobs, and held jobs whose task is closed (dead).
func detectHeld(snap Snapshot, _ DetectOptions) []Finding {
	var fs []Finding
	var longIDs, deadIDs, reasons []string
	oldest := time.Duration(0)
	for _, j := range snap.Jobs {
		if j.State != "held" {
			continue
		}
		age := snap.Now.Sub(heldSince(j))
		if isDeadHeld(j, snap.Now) {
			deadIDs = append(deadIDs, j.ID)
			continue
		}
		if age > DeadHeldAfter {
			longIDs = append(longIDs, j.ID)
			reasons = append(reasons, j.ID+": "+clip(j.HeldReason, 100))
			if age > oldest {
				oldest = age
			}
		}
	}
	if len(longIDs) > 0 {
		fs = append(fs, Finding{
			Fingerprint: fingerprint("held", "long"),
			Title:       fmt.Sprintf("%d jobs held by the usage throttle for over 6h", len(longIDs)),
			Evidence:    append([]string{fmt.Sprintf("oldest held %s", oldest.Round(time.Minute))}, limit(reasons, 6)...),
			JobIDs:      longIDs,
			Metric:      float64(len(longIDs)),
			MetricLabel: "jobs held > 6h",
			Cause:       "Subscription usage stayed ORANGE/RED long enough to keep low-priority work parked.",
			Fix:         "Operator decision: re-prioritise, cancel, or wait for the usage window to reset (the throttle limits are off-limits to the auditor).",
			Benefit:     "Visibility of parked work; no silent multi-hour delays.",
			Effort:      "Operator triage only.",
		})
	}
	if len(deadIDs) > 0 {
		fs = append(fs, Finding{
			Fingerprint: fingerprint("held", "dead"),
			Title:       fmt.Sprintf("%d held jobs belong to closed tasks", len(deadIDs)),
			Evidence:    []string{"Held for over 6h; their task is closed, so nobody is waiting for them; none was created directly by the operator (created_by ui)."},
			JobIDs:      deadIDs,
			Metric:      float64(len(deadIDs)),
			MetricLabel: "dead held jobs",
			Cause:       "The task was closed while its jobs were parked by the throttle.",
			Fix:         "Cancel the held jobs.",
			Benefit:     "They stop occupying the held queue ahead of live work.",
			Effort:      "None (automatic).",
			AutoFix: &Plan{
				Kind:        FixCancelDeadHeld,
				Description: "Cancel held jobs whose task is closed: " + strings.Join(deadIDs, ", "),
				Targets:     deadIDs,
				Revert:      "Each cancelled job keeps its full spec (GET /v1/jobs/<id>); re-create it with POST /v1/jobs using that spec's task, tier, timeout_s and priority (plus task_id to re-link it).",
			},
		})
	}
	return fs
}

var bypassRe = regexp.MustCompile(`(?i)priority 0|bypass the usage hold|avoid usage hold|usage hold`)

// detectThrottleBypass: jobs superseded/cancelled to dodge the throttle by
// re-filing at task priority 0.
func detectThrottleBypass(snap Snapshot, _ DetectOptions) []Finding {
	var ids, ev []string
	for _, j := range snap.Jobs {
		if (j.State == "superseded" || j.State == "cancelled") && bypassRe.MatchString(j.LastError) {
			ids = append(ids, j.ID)
			ev = append(ev, j.ID+": "+clip(j.LastError, 140))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return []Finding{{
		Fingerprint: fingerprint("throttle", "p0_bypass"),
		Title:       fmt.Sprintf("%d jobs re-filed to sidestep the usage throttle", len(ids)),
		Evidence:    limit(ev, 6),
		JobIDs:      ids,
		Metric:      float64(len(ids)),
		MetricLabel: "throttle sidesteps in 7d",
		Cause:       "Held work gets superseded and re-created under a priority-0 (or higher-priority) task so it skips the hold.",
		Fix:         "Operator decision: either accept this as the release valve, or tell the planner (priority criteria) when it may escalate to P0. Throttle limits are off-limits to auto-fixes.",
		Benefit:     "The throttle protects the subscription as intended, or the escalation path becomes explicit.",
		Effort:      "A policy decision plus a small prompt change.",
	}}
}

func detectOverBudget(snap Snapshot, _ DetectOptions) []Finding {
	var ids, ev []string
	total := 0.0
	for _, j := range snap.Jobs {
		if j.OverBudget {
			ids = append(ids, j.ID)
			total += j.CostUSD
			ev = append(ev, fmt.Sprintf("%s (%s, %s tier): $%.2f, %d in / %d out tokens", j.ID, j.Kind, j.Tier, j.CostUSD, j.InTokens, j.OutTokens))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return []Finding{{
		Fingerprint: fingerprint("over_budget", "jobs"),
		Title:       fmt.Sprintf("%d jobs exceeded their max_cost_usd", len(ids)),
		Evidence:    limit(ev, 8),
		JobIDs:      ids,
		Metric:      total,
		MetricLabel: "cost_usd of over-budget jobs in 7d",
		Cause:       "Jobs ran longer or on a pricier tier than their budget assumed.",
		Fix:         "Review whether these jobs needed their tier; split big jobs. Budget values are off-limits to auto-fixes.",
		Benefit:     fmt.Sprintf("Up to $%.2f/week of notional spend.", total),
		Effort:      "Small (dispatch guidance).",
	}}
}

func detectRetries(snap Snapshot, _ DetectOptions) []Finding {
	var ids []string
	attempts := 0
	for _, j := range snap.Jobs {
		if j.Attempts >= 2 {
			ids = append(ids, j.ID)
			attempts += j.Attempts
		}
	}
	if len(ids) < 2 {
		return nil
	}
	return []Finding{{
		Fingerprint: fingerprint("retries", "multi_attempt"),
		Title:       fmt.Sprintf("%d jobs needed 2+ attempts", len(ids)),
		Evidence:    []string{fmt.Sprintf("%d attempts across %d jobs in 7 days", attempts, len(ids))},
		JobIDs:      ids,
		Metric:      float64(len(ids)),
		MetricLabel: "jobs with >= 2 attempts in 7d",
		Cause:       "Transient crashes or a deterministic failure that retries cannot fix.",
		Fix:         "Check the recurring-error findings; lower the retry count for deterministic error kinds (validation).",
		Benefit:     "Less spend on retries that cannot succeed.",
		Effort:      "Small.",
	}}
}

// detectTierSpend: which tier ran which kind of work, and whether the
// expensive tier is spent on work that finishes cheaply.
func detectTierSpend(snap Snapshot, _ DetectOptions) []Finding {
	type agg struct {
		n             int
		cost          float64
		in, out       int
		cheap         int
		ids, cheapIDs []string
	}
	by := map[string]*agg{}
	total := 0.0
	for _, j := range snap.Jobs {
		k := j.Kind + "/" + j.Tier
		a := by[k]
		if a == nil {
			a = &agg{}
			by[k] = a
		}
		a.n++
		a.cost += j.CostUSD
		a.in += j.InTokens
		a.out += j.OutTokens
		a.ids = append(a.ids, j.ID)
		total += j.CostUSD
		if j.Tier == wire.TierHardest && j.State == "done" && j.OutTokens > 0 && j.OutTokens < 8000 {
			a.cheap++
			a.cheapIDs = append(a.cheapIDs, j.ID)
		}
	}
	var keys []string
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return by[keys[i]].cost > by[keys[j]].cost })
	var ev []string
	for _, k := range keys {
		a := by[k]
		ev = append(ev, fmt.Sprintf("%s: %d jobs, $%.2f, %d in / %d out tokens", k, a.n, a.cost, a.in, a.out))
	}
	var fs []Finding
	for _, k := range keys {
		a := by[k]
		if !strings.HasSuffix(k, "/"+wire.TierHardest) || a.cheap < 3 || a.cheap*2 < a.n {
			continue
		}
		fs = append(fs, Finding{
			Fingerprint: fingerprint("tier", k),
			Title:       fmt.Sprintf("%s jobs run on the hardest tier but mostly finish small", k),
			Evidence: append([]string{fmt.Sprintf("%d of %d %s jobs in 7 days finished with < 8000 output tokens", a.cheap, a.n, k)},
				limit(ev, 8)...),
			JobIDs:      a.cheapIDs,
			Metric:      a.cost,
			MetricLabel: "cost_usd of " + k + " in 7d",
			Cause:       "The dispatcher (or code, for merge jobs) defaults this kind of work to the hardest tier.",
			Fix:         "Default this job kind to the `default` tier and escalate only on failure.",
			Benefit:     fmt.Sprintf("A large share of $%.2f/week of notional spend on %s.", a.cost, k),
			Effort:      "Small (one tier constant or dispatch guidance).",
		})
	}
	return fs
}

var falseLandRe = regexp.MustCompile(`(?i)couldn't merge|could not merge|didn't merge|did not merge|nothing was merged|not merged|should go to needs_attention`)

// detectMergeOutcomes: needs_attention requests, and requests closed as
// landed although the merge job reported it merged nothing.
func detectMergeOutcomes(snap Snapshot, _ DetectOptions) []Finding {
	var fs []Finding
	var falseIDs, falseEv, naIDs, naEv, jobIDs, naJobs []string
	for _, m := range snap.Merges {
		switch {
		case m.State == "landed" && falseLandRe.MatchString(m.Result):
			falseIDs = append(falseIDs, m.ID)
			jobIDs = append(jobIDs, m.JobID)
			falseEv = append(falseEv, fmt.Sprintf("%s %s@%s: %s", m.ID, m.Branch, shortSHA(m.Head), clip(m.Result, 160)))
		case m.State == "needs_attention":
			naIDs = append(naIDs, m.ID)
			naJobs = append(naJobs, m.JobID)
			naEv = append(naEv, fmt.Sprintf("%s %s@%s: %s", m.ID, m.Branch, shortSHA(m.Head), clip(m.Result, 160)))
		}
	}
	if len(falseIDs) > 0 {
		fs = append(fs, Finding{
			Fingerprint: fingerprint("merge", "false_landed"),
			Title:       fmt.Sprintf("%d merge requests marked landed although nothing merged", len(falseIDs)),
			Evidence:    limit(falseEv, 6),
			JobIDs:      jobIDs,
			Metric:      float64(len(falseIDs)),
			MetricLabel: "falsely landed merge requests in 7d",
			Cause:       "The merge job reports status=done when it stops on a conflict (report_result with failed was rejected), and the queue maps done -> landed.",
			Fix:         "Have the merge procedure report failed on conflict (now possible with the typed report_result), and make the queue require a structured `merged: true` result before landed.",
			Benefit:     "The queue and the operator stop believing unmerged branches shipped.",
			Effort:      "Small-medium (merge job result_schema + queue close rule).",
		})
	}
	if len(naIDs) > 0 {
		fs = append(fs, Finding{
			Fingerprint: fingerprint("merge", "needs_attention"),
			Title:       fmt.Sprintf("%d merge requests need attention", len(naIDs)),
			Evidence:    limit(naEv, 6),
			JobIDs:      naJobs,
			Metric:      float64(len(naIDs)),
			MetricLabel: "needs_attention merge requests in 7d",
			Cause:       "Merge jobs failed (most often: the branch conflicts with master).",
			Fix:         "Rebase the listed branches on master and re-queue them.",
			Benefit:     "Unblocks the queued work.",
			Effort:      "Small per branch.",
		})
	}
	return fs
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// detectForemen: offline foremen and build-version drift.
func detectForemen(snap Snapshot, _ DetectOptions) []Finding {
	var fs []Finding
	builds := map[string]int{}
	for _, f := range snap.Foremen {
		if f.Online && f.Build != "" {
			builds[f.Build]++
		}
	}
	common, best := "", 0
	for b, n := range builds {
		if n > best || (n == best && b > common) {
			common, best = b, n
		}
	}
	var offline, drift []string
	for _, f := range snap.Foremen {
		switch {
		case !f.Online:
			offline = append(offline, fmt.Sprintf("%s offline since %s", f.ID, f.LastSeen.Format(time.RFC3339)))
		case f.Build != common:
			b := f.Build
			if b == "" {
				b = "(no build id: pre-auto-update binary)"
			}
			drift = append(drift, fmt.Sprintf("%s runs %s (tools: %s)", f.ID, b, orDefault(f.Tools, "none")))
		}
	}
	if len(offline) > 0 {
		fs = append(fs, Finding{
			Fingerprint: fingerprint("foreman", "offline"),
			Title:       fmt.Sprintf("%d foremen offline", len(offline)),
			Evidence:    offline,
			Metric:      float64(len(offline)),
			MetricLabel: "offline foremen",
			Cause:       "The foreman service stopped or the host is unreachable.",
			Fix:         "Check the foreman service on those hosts.",
			Benefit:     "Restores capacity; pinned jobs stop pending.",
			Effort:      "Operator check.",
		})
	}
	if len(drift) > 0 {
		fs = append(fs, Finding{
			Fingerprint: fingerprint("foreman", "build_drift"),
			Title:       fmt.Sprintf("%d foremen are not on the current build %s", len(drift), common),
			Evidence:    drift,
			Metric:      float64(len(drift)),
			MetricLabel: "foremen off the common build",
			Cause:       "These foremen predate auto-update or failed to update.",
			Fix:         "Update them (manual install for pre-auto-update builds). Deploy procedure is off-limits to auto-fixes.",
			Benefit:     "Consistent behaviour across foremen; fixes reach every host.",
			Effort:      "Small per host.",
		})
	}
	return fs
}

// detectWorktrees: clean merged worktrees (auto-cleanup) and old unmerged
// ones (proposal).
func detectWorktrees(snap Snapshot, o DetectOptions) []Finding {
	if snap.WorktreeErr != "" {
		return nil
	}
	var fs []Finding
	var merged, mergedEv, old, oldEv []string
	for _, w := range snap.Worktrees {
		switch {
		case w.Prunable:
			old = append(old, w.Path)
			oldEv = append(oldEv, w.Path+": prunable (directory missing)")
		case w.Merged && w.Clean:
			merged = append(merged, filepath.Clean(w.Path))
			mergedEv = append(mergedEv, fmt.Sprintf("%s [%s @ %s] merged into master, clean", w.Path, orDefault(w.Branch, "detached"), shortSHA(w.Head)))
		case !w.Merged && !w.LastCommit.IsZero() && snap.Now.Sub(w.LastCommit) > o.StaleAfter:
			old = append(old, w.Path)
			oldEv = append(oldEv, fmt.Sprintf("%s [%s] unmerged, last commit %s, clean=%v", w.Path, orDefault(w.Branch, "detached"), w.LastCommit.Format("2006-01-02"), w.Clean))
		}
	}
	if len(merged) > 0 {
		var revert []string
		for _, w := range snap.Worktrees {
			if w.Merged && w.Clean && !w.Prunable {
				if w.Branch != "" {
					revert = append(revert, fmt.Sprintf("git worktree add %s -b %s %s", w.Path, w.Branch, w.Head))
				} else {
					revert = append(revert, fmt.Sprintf("git worktree add --detach %s %s", w.Path, w.Head))
				}
			}
		}
		fs = append(fs, Finding{
			Fingerprint: fingerprint("worktree", "merged_clean"),
			Title:       fmt.Sprintf("%d stale worktrees are merged and clean", len(merged)),
			Evidence:    limit(mergedEv, 10),
			Metric:      float64(len(merged)),
			MetricLabel: "merged clean worktrees",
			Cause:       "Job worktrees are not removed after their branch lands.",
			Fix:         "Remove the worktrees and their (merged) local branches.",
			Benefit:     "Less disk use and less confusion over which checkout is live.",
			Effort:      "None (automatic).",
			AutoFix: &Plan{
				Kind:        FixCleanupWorktrees,
				Description: "Remove these clean, merged worktrees with `git worktree remove` (never --force) and delete their local branches with `git branch -d` (never -D); skip any that turned dirty or unmerged.",
				Targets:     merged,
				Revert:      "Every branch is merged into master, so nothing is lost. To recreate, run in the repo: " + strings.Join(revert, "; "),
			},
		})
	}
	if len(old) > 0 {
		fs = append(fs, Finding{
			Fingerprint: fingerprint("worktree", "stale_unmerged"),
			Title:       fmt.Sprintf("%d worktrees are stale but unmerged or prunable", len(old)),
			Evidence:    limit(oldEv, 10),
			Metric:      float64(len(old)),
			MetricLabel: "stale unmerged worktrees",
			Cause:       "Abandoned or superseded branches whose worktrees were never removed.",
			Fix:         "Operator decision per worktree: finish, archive (push the branch) or delete.",
			Benefit:     "A clean /home/price/work.",
			Effort:      "A few minutes of triage.",
		})
	}
	return fs
}
