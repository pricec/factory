package auditor

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Fix kinds an auto-fix plan may take (spec §6.12). Anything else is a
// proposal.
const (
	// FixPR is one small branch -> CI -> merge-queue change: a prompt/doc
	// fix, a non-safety config tune (timeouts, a job kind's default tier,
	// retry counts) or another small code fix.
	FixPR = "pr"
	// FixCleanupWorktrees removes clean worktrees (and their local
	// branches) already merged into master, under the worktree root.
	FixCleanupWorktrees = "cleanup_worktrees"
	// FixCancelDeadHeld cancels held jobs whose task is closed.
	FixCancelDeadHeld = "cancel_dead_held"
)

// FixKinds is every kind CheckPlan accepts.
var FixKinds = []string{FixPR, FixCleanupWorktrees, FixCancelDeadHeld}

// Area is one forbidden area of the denylist: repo paths (prefixes, or
// exact files) and, for plan text, keywords. An auto-fix touching any of
// them becomes a proposal.
type Area struct {
	Name     string
	Paths    []string
	Keywords []string
}

// Denylist is the forbidden-area list (spec §6.12). It covers spend caps,
// budgets and usage-throttle limits; secrets; auth and permissions; the
// merge/deploy/rollback procedure (and the CI definition gating it); and
// the auditor's own rules, guardrails, wiring and schedule. It is code,
// and its own file is on it.
var Denylist = []Area{
	{
		Name: "spend caps and budgets",
		Paths: []string{
			"internal/budgets/", "internal/jobs/spend.go",
		},
		Keywords: []string{"spend cap", "spend_cap", "daily_spend", "max_cost_usd", "budget"},
	},
	{
		Name: "usage throttle",
		Paths: []string{
			"internal/throttle/", "internal/usagewin/", "internal/factoryapp/throttle.go",
			"internal/factoryapp/pause.go", "internal/factoryapp/usage.go", "internal/jobs/held.go",
		},
		Keywords: []string{"throttle", "usage limit", "usage window", "held queue", "priority 0", "p0"},
	},
	{
		Name: "secrets",
		Paths: []string{
			"internal/secrets/", "internal/jobs/secretgrants.go", "internal/foremanapp/secrets.go",
			"internal/factoryapp/secrets_handout.go", "internal/factoryapp/secrets_http.go",
		},
		Keywords: []string{"secret", "credential", "api key", "api_key", "password"},
	},
	{
		Name: "auth and permissions",
		Paths: []string{
			"internal/auth/", "internal/inbox/auth.go", "internal/foremanapp/pairing.go",
		},
		Keywords: []string{"auth", "token", "permission", "pairing", "scope"},
	},
	{
		Name: "merge, deploy and rollback procedure",
		Paths: []string{
			"docs/merging.md", "docs/operator-runbook.md", "internal/jobs/merges.go",
			"internal/jobs/mergetask.go", "internal/factoryapp/mergequeue.go",
			"internal/foremanapp/updater.go", "internal/foremanapp/drain.go",
			"internal/factoryapp/install.go", "internal/factoryapp/install.sh",
			"internal/factoryapp/install-server.sh", "internal/registry/advert.go",
			"Makefile", ".github/", "go.mod", "go.sum", "dist/",
		},
		Keywords: []string{"deploy", "rollback", "roll back", "release", "merge queue", "systemd", "make ci"},
	},
	{
		Name: "auditor rules, guardrails and schedule",
		Paths: []string{
			"internal/auditor/", "internal/factoryapp/auditor.go", "internal/factoryapp/auditor_test.go",
			"cmd/factory/audit.go", "cmd/auditcheck/", "internal/jobs/schedules.go",
		},
		Keywords: []string{"auditor", "guardrail", "denylist", "audit schedule", "schedule"},
	},
	{
		Name: "normative contracts",
		Paths: []string{
			"docs/spec.md", "AGENTS.md",
		},
	},
}

// ErrGuardrail wraps every guardrail refusal.
var ErrGuardrail = errors.New("guardrail")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrGuardrail, fmt.Sprintf(format, args...))
}

// cleanRepoPath normalizes a repo-relative path; it reports false for
// paths that escape the repo or are absolute.
func cleanRepoPath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return p, false
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return c, false
	}
	return c, true
}

// DeniedArea returns the forbidden area a repo-relative path falls in, or
// "" when the path is allowed. Matching is case-insensitive; an entry
// ending in "/" is a directory prefix, any other entry an exact file.
// Unparseable paths (absolute, escaping) are denied.
func DeniedArea(p string) string {
	c, ok := cleanRepoPath(p)
	if !ok {
		return "path outside the repository"
	}
	lc := strings.ToLower(c)
	for _, a := range Denylist {
		for _, d := range a.Paths {
			d = strings.ToLower(d)
			if strings.HasSuffix(d, "/") {
				if strings.HasPrefix(lc+"/", d) {
					return a.Name
				}
			} else if lc == d {
				return a.Name
			}
		}
	}
	return ""
}

var wordRe = regexp.MustCompile(`[a-z0-9_]+`)

// deniedKeyword returns the area whose keyword text mentions, matching on
// word boundaries ("auth" does not match "author").
func deniedKeyword(text string) (area, kw string) {
	words := " " + strings.Join(wordRe.FindAllString(strings.ToLower(text), -1), " ") + " "
	for _, a := range Denylist {
		for _, k := range a.Keywords {
			nk := " " + strings.Join(wordRe.FindAllString(strings.ToLower(k), -1), " ") + " "
			if strings.Contains(words, nk) {
				return a.Name, k
			}
		}
	}
	return "", ""
}

// FileStat is one file of a diff (git diff --numstat): Added/Deleted are
// -1 for binary files.
type FileStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// ChangedLines sums added+deleted lines; binary files count as -1.
func ChangedLines(files []FileStat) (int, bool) {
	n := 0
	for _, f := range files {
		if f.Added < 0 || f.Deleted < 0 {
			return 0, false
		}
		n += f.Added + f.Deleted
	}
	return n, true
}

// CheckDiff is the code-enforced gate on a fix branch's real diff against
// master: non-empty, text-only, at most maxLines changed lines, and no
// denylisted path (either side of a rename). maxLines is clamped to
// HardMaxChangedLines. It runs in CI for auto/audit-* branches
// (cmd/auditcheck built from master, .github/workflows/audit-guardrail.yml) - never
// in the factory service, which does no git work.
func CheckDiff(files []FileStat, maxLines int) error {
	if maxLines <= 0 || maxLines > HardMaxChangedLines {
		maxLines = HardMaxChangedLines
	}
	if len(files) == 0 {
		return refuse("empty diff")
	}
	for _, f := range files {
		if area := DeniedArea(f.Path); area != "" {
			return refuse("%s touches a forbidden area (%s)", f.Path, area)
		}
	}
	n, ok := ChangedLines(files)
	if !ok {
		return refuse("diff contains binary files")
	}
	if n > maxLines {
		return refuse("diff changes %d lines, cap is %d", n, maxLines)
	}
	return nil
}

// Plan is an auto-fix candidate (spec §6.12).
type Plan struct {
	Kind string `json:"kind"`
	// Description says what to change; it becomes the fix job's brief.
	Description string `json:"description"`
	// Paths are the repo-relative files a FixPR intends to touch.
	Paths []string `json:"paths,omitempty"`
	// EstLines is the expected changed-line count of a FixPR.
	EstLines int `json:"est_lines,omitempty"`
	// Targets are what a cleanup acts on: worktree paths
	// (FixCleanupWorktrees) or job ids (FixCancelDeadHeld).
	Targets []string `json:"targets,omitempty"`
	// Revert says exactly how to undo the fix.
	Revert string `json:"revert"`
}

// PlanContext carries what CheckPlan validates cleanup targets against:
// only targets the structured snapshot classified as dead/stale are
// acceptable - never a target named by LLM prose. Worktrees come from the
// audit job's reported scan, validated by ValidateWorktrees (under the
// root) and admitted only when flagged merged AND clean.
type PlanContext struct {
	MaxLines     int
	WorktreeRoot string
	// StaleWorktrees: worktree path -> true for clean worktrees whose
	// branch is merged into master.
	StaleWorktrees map[string]bool
	// DeadHeld: job id -> true for held jobs whose task is closed and
	// that the operator did not create directly.
	DeadHeld map[string]bool
}

// CheckPlan is the code-enforced gate BEFORE an auto-fix starts. It
// checks the kind, the revert instructions, the denylist (paths and plan
// text) and the line estimate; cleanup targets must be ones the snapshot
// classified. Every refusal turns the finding into a proposal.
func CheckPlan(p Plan, pc PlanContext) error {
	if strings.TrimSpace(p.Revert) == "" {
		return refuse("no revert instructions")
	}
	if strings.TrimSpace(p.Description) == "" {
		return refuse("no description")
	}
	switch p.Kind {
	case FixPR:
		max := pc.MaxLines
		if max <= 0 || max > HardMaxChangedLines {
			max = HardMaxChangedLines
		}
		if len(p.Paths) == 0 {
			return refuse("pr plan names no files")
		}
		for _, f := range p.Paths {
			if area := DeniedArea(f); area != "" {
				return refuse("%s touches a forbidden area (%s)", f, area)
			}
		}
		if p.EstLines <= 0 || p.EstLines > max {
			return refuse("estimated %d changed lines, cap is %d", p.EstLines, max)
		}
		if area, kw := deniedKeyword(p.Description); area != "" {
			return refuse("plan mentions %q (%s)", kw, area)
		}
	case FixCleanupWorktrees:
		if len(p.Targets) == 0 {
			return refuse("cleanup names no worktrees")
		}
		root := filepath.Clean(pc.WorktreeRoot)
		for _, t := range p.Targets {
			c := filepath.Clean(t)
			if pc.WorktreeRoot == "" || !strings.HasPrefix(c, root+"/") {
				return refuse("worktree %s is outside %s", t, pc.WorktreeRoot)
			}
			if !pc.StaleWorktrees[c] {
				return refuse("worktree %s is not a clean, merged worktree", t)
			}
		}
	case FixCancelDeadHeld:
		if len(p.Targets) == 0 {
			return refuse("cancel names no jobs")
		}
		for _, t := range p.Targets {
			if !pc.DeadHeld[t] {
				return refuse("job %s is not a held job of a closed task", t)
			}
		}
	default:
		return refuse("kind %q is not an auto-fix kind (%v)", p.Kind, FixKinds)
	}
	return nil
}

// RunCap is the per-run auto-fix counter (hard ceiling HardMaxAutoFixes).
type RunCap struct {
	Max  int
	Used int
}

// Take claims one auto-fix slot.
func (c *RunCap) Take() error {
	max := c.Max
	if max < 0 || max > HardMaxAutoFixes {
		max = HardMaxAutoFixes
	}
	if c.Used >= max {
		return refuse("per-run auto-fix cap reached (%d)", max)
	}
	c.Used++
	return nil
}

// RunJob is one job of an audit run as the meter sees it.
type RunJob struct {
	ID       string
	CostUSD  float64
	Terminal bool
	// ReserveUSD is what an unfinished job counts at minimum.
	ReserveUSD float64
}

// Meter is the per-run cost_usd budget (spec §6.12). Spent counts every
// job of the run: finished jobs at their real cost_usd, unfinished ones at
// max(cost so far, reserve).
type Meter struct {
	BudgetUSD float64
	SpentUSD  float64
}

// NewMeter totals jobs against budget.
func NewMeter(budget float64, jobs []RunJob) Meter {
	m := Meter{BudgetUSD: budget}
	for _, j := range jobs {
		c := j.CostUSD
		if !j.Terminal && j.ReserveUSD > c {
			c = j.ReserveUSD
		}
		m.SpentUSD += c
	}
	return m
}

// Reserve admits new work expected to cost up to reserve: refused once the
// meter has reached the budget, or when the reservation would cross it.
// On success the reservation is added to SpentUSD.
func (m *Meter) Reserve(reserve float64) error {
	if m.SpentUSD >= m.BudgetUSD {
		return refuse("run budget spent: $%.2f of $%.2f", m.SpentUSD, m.BudgetUSD)
	}
	if m.SpentUSD+reserve > m.BudgetUSD {
		return refuse("run budget: $%.2f spent + $%.2f reserve exceeds $%.2f", m.SpentUSD, reserve, m.BudgetUSD)
	}
	m.SpentUSD += reserve
	return nil
}

// Remaining is the unspent budget (never negative).
func (m Meter) Remaining() float64 {
	if r := m.BudgetUSD - m.SpentUSD; r > 0 {
		return r
	}
	return 0
}
