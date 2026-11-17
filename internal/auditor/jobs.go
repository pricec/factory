package auditor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// AuditPayload is the audit job's payload: the deterministic findings
// computed when the schedule fired (code-derived evidence) for the sweep
// to verify and extend.
type AuditPayload struct {
	Kind         string    `json:"kind"`
	AuditRun     string    `json:"audit_run"`
	SnapshotAt   string    `json:"snapshot_at"`
	WorktreeRoot string    `json:"worktree_root"`
	RepoDir      string    `json:"repo_dir"`
	Findings     []Finding `json:"findings"`
}

// FixPayload is a fix job's payload.
type FixPayload struct {
	Kind        string `json:"kind"`
	AuditRun    string `json:"audit_run"`
	Fingerprint string `json:"fingerprint"`
	TaskID      string `json:"task_id"`
	Plan        Plan   `json:"plan"`
	Branch      string `json:"branch,omitempty"`
	MaxLines    int    `json:"max_lines,omitempty"`
}

// AuditTemplate is the schedule's job template (spec §6.12): LOW priority,
// the cheapest tier, pinned to the integration foreman. Its payload is
// filled in when the schedule fires (FillAuditSpec).
func AuditTemplate(c Config) wire.JobSpec {
	spec := wire.NewJobSpec(auditTask, CreatedBy)
	spec.ID, spec.CreatedAt = "", ""
	spec.Tier = c.SweepTier
	spec.Priority = c.JobPriority
	spec.Foreman = c.Foreman
	spec.TimeoutS = 1800
	spec.ResultSchema = json.RawMessage(sweepSchema)
	spec.Budget.MaxCostUSD = c.BudgetUSD
	spec.Budget.MaxPromptBytes = 48 * 1024
	spec.Payload = json.RawMessage(`{"kind":"` + PayloadKindAudit + `"}`)
	return spec
}

// IsAuditTemplate reports whether a schedule template is the auditor's.
func IsAuditTemplate(spec wire.JobSpec) bool {
	if spec.CreatedBy != CreatedBy {
		return false
	}
	var p struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(spec.Payload, &p)
	return p.Kind == PayloadKindAudit
}

// payloadBudget keeps task + payload inside max_prompt_bytes with room for
// the harness boilerplate.
const payloadBudget = 36 * 1024

// FillAuditSpec writes the run's payload into a fired audit job: the run id
// is the job id, the findings are trimmed until the payload fits.
func FillAuditSpec(spec *wire.JobSpec, c Config, snapAt time.Time, fs []Finding) error {
	p := AuditPayload{
		Kind: PayloadKindAudit, AuditRun: spec.ID, SnapshotAt: snapAt.UTC().Format(time.RFC3339),
		WorktreeRoot: c.WorktreeRoot, RepoDir: c.RepoDir, Findings: fs,
	}
	for {
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if len(b) <= payloadBudget {
			spec.Payload = b
			return nil
		}
		// Trim the largest parts first: evidence and job id lists, then
		// whole findings from the least severe end.
		trimmed := false
		for i := range p.Findings {
			if len(p.Findings[i].JobIDs) > 5 {
				p.Findings[i].JobIDs = p.Findings[i].JobIDs[:5]
				trimmed = true
			}
			if len(p.Findings[i].Evidence) > 4 {
				p.Findings[i].Evidence = p.Findings[i].Evidence[:4]
				trimmed = true
			}
		}
		if !trimmed {
			if len(p.Findings) == 0 {
				return fmt.Errorf("audit payload does not fit")
			}
			p.Findings = p.Findings[:len(p.Findings)-1]
		}
	}
}

// ParseAuditPayload reads a fired audit job's payload.
func ParseAuditPayload(raw json.RawMessage) (AuditPayload, error) {
	var p AuditPayload
	err := json.Unmarshal(raw, &p)
	return p, err
}

const auditTask = `You are the factory auditor's daily sweep (spec §6.12). READ-ONLY: do not edit, commit, push, fetch, delete, cancel or deploy anything.

The payload holds deterministic findings the factory computed from its own databases (jobs, usage, merge queue, foremen) for the last 24h with a 7-day trend. The factory service runs no git, so YOU run the worktree scan. Your job:

1. Worktree scan (required, read-only, no fetch). REPO=payload.repo_dir, ROOT=payload.worktree_root. If REPO is empty or not a git repo, set worktree_scan_error and skip. Otherwise run ` + "`git -C $REPO worktree list --porcelain`" + ` and, for EVERY worktree whose path is under ROOT/ (skip the main checkout and anything else), report one entry:
   - path, branch (without refs/heads/; "" when detached), head (full sha), prunable (true if the porcelain output says prunable; then skip the other checks);
   - merged: true only if ` + "`git -C $REPO merge-base --is-ancestor <head> origin/master`" + ` exits 0 (compare with the existing origin/master ref; do not fetch);
   - clean: true only if ` + "`git -C <path> --no-optional-locks status --porcelain`" + ` succeeds with empty output;
   - last_commit: ` + "`git -C $REPO log -1 --format=%cI <head>`" + ` (RFC 3339).
   Report the raw facts only; never guess merged/clean. If a command fails for a worktree, report merged=false, clean=false. If the scan as a whole fails, set worktree_scan_error to the error instead of worktrees.
2. Sanity-check each payload finding. Where you can add a sharper likely cause or a more concrete fix (file names, functions), return it under the SAME fingerprint.
3. Look for what the databases cannot see, on this host only and read-only: recurring error strings in job outputs under /var/lib/factory-foreman/jobs/*/ (e.g. grep the last lines of logs). Report new findings with a short kebab-case fingerprint. Do not file worktree findings yourself: the factory derives them from your scan.
4. Optionally attach an autofix plan to a finding ONLY when the fix is one small reversible code/doc/prompt/config change (kind "pr": list the repo-relative paths and an honest est_lines <= 150, plus exact revert steps). Never propose anything touching spend caps, budgets, the usage throttle, secrets, auth/permissions, the merge/deploy/rollback procedure, CI, or the auditor itself - the factory enforces these rules in code and will downgrade such plans to proposals anyway.

Every finding needs: fingerprint, title (one line), evidence (job ids, token/cost numbers, paths), cause, fix, benefit, effort, metric (a number where higher is worse, e.g. an occurrence count). Keep it to the 10 most valuable findings. Report with report_result status done; result = {"findings": [...], "worktrees": [...], "worktree_scan_error": "", "notes": "..."}.`

const sweepSchema = `{"type":"object","required":["findings","worktrees"],"properties":{
"findings":{"type":"array","items":{"type":"object","required":["fingerprint","title","evidence","cause","fix","metric"],"properties":{
"fingerprint":{"type":"string"},"title":{"type":"string"},"evidence":{"type":"array","items":{"type":"string"}},
"cause":{"type":"string"},"fix":{"type":"string"},"benefit":{"type":"string"},"effort":{"type":"string"},
"metric":{"type":"number"},"job_ids":{"type":"array","items":{"type":"string"}},
"autofix":{"type":"object","required":["kind","description","revert"],"properties":{
"kind":{"type":"string"},"description":{"type":"string"},"paths":{"type":"array","items":{"type":"string"}},
"est_lines":{"type":"integer"},"revert":{"type":"string"}}}}}},
"worktrees":{"type":"array","items":{"type":"object","required":["path","head","merged","clean"],"properties":{
"path":{"type":"string"},"branch":{"type":"string"},"head":{"type":"string"},"merged":{"type":"boolean"},
"clean":{"type":"boolean"},"prunable":{"type":"boolean"},"last_commit":{"type":"string"}}}},
"worktree_scan_error":{"type":"string"},
"notes":{"type":"string"}}}`

// maxSweepFindings bounds what one sweep may add.
const maxSweepFindings = 10

// MergeSweep folds the sweep's result into the detector findings. For a
// known fingerprint only the prose (cause, fix, benefit, effort) may be
// sharpened - evidence, metric and the auto-fix plan stay code-derived.
// New sweep findings are namespaced "sweep:", capped, and may carry only
// a "pr" plan (cleanup targets must come from the snapshot).
func MergeSweep(detected []Finding, result json.RawMessage) ([]Finding, error) {
	out := append([]Finding(nil), detected...)
	if len(result) == 0 {
		return out, nil
	}
	var r struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return out, fmt.Errorf("sweep result: %w", err)
	}
	idx := map[string]int{}
	known := map[string]bool{}
	for i, f := range out {
		idx[f.Fingerprint] = i
		known[f.Fingerprint] = true
	}
	added := 0
	for _, s := range r.Findings {
		fp := SweepFingerprint(s.Fingerprint, known)
		if i, ok := idx[fp]; ok {
			d := &out[i]
			setIf(&d.Cause, s.Cause)
			setIf(&d.Fix, s.Fix)
			setIf(&d.Benefit, s.Benefit)
			setIf(&d.Effort, s.Effort)
			continue
		}
		if added >= maxSweepFindings || strings.TrimSpace(s.Title) == "" {
			continue
		}
		s.Fingerprint, s.Source = fp, "sweep"
		s.Title = clip(strings.Join(strings.Fields(s.Title), " "), 160)
		if s.MetricLabel == "" {
			s.MetricLabel = "sweep metric"
		}
		if s.Metric < 0 {
			s.Metric = 0
		}
		s.Evidence = limit(s.Evidence, 12)
		if s.AutoFix != nil && s.AutoFix.Kind != FixPR {
			s.AutoFix = nil
		}
		if s.AutoFix != nil {
			s.AutoFix.Targets = nil
		}
		s.Known = knownNote(append([]string{s.Title}, s.Evidence...)...)
		if s.Known != "" {
			s.AutoFix = nil
		}
		idx[fp] = len(out)
		out = append(out, s)
		added++
	}
	SortFindings(out)
	return out, nil
}

func setIf(dst *string, v string) {
	if v = strings.TrimSpace(v); v != "" {
		*dst = clip(v, 1200)
	}
}

// FixBranch is the branch name a PR fix job must push.
func FixBranch(run string, n int) string {
	r := strings.ToLower(strings.TrimPrefix(run, "job_"))
	if len(r) > 10 {
		r = r[len(r)-10:]
	}
	return fmt.Sprintf(FixBranchPrefix+"%s-%d", r, n)
}

// FixJobSpec builds a fix job: LOW priority, FixTier (flash for a
// worktree cleanup, which needs no reasoning), pinned to the integration
// foreman, max_cost_usd = the meter reservation.
func FixJobSpec(c Config, run string, n int, f Finding, taskID string, reserve float64) (wire.JobSpec, error) {
	p := FixPayload{Kind: PayloadKindFix, AuditRun: run, Fingerprint: f.Fingerprint, TaskID: taskID, Plan: *f.AutoFix}
	var task string
	tier := c.FixTier
	switch f.AutoFix.Kind {
	case FixPR:
		p.Branch, p.MaxLines = FixBranch(run, n), c.MaxChangedLines
		task = fmt.Sprintf(prFixTask, c.Repo, c.WorktreeRoot, p.Branch, strings.Join(f.AutoFix.Paths, ", "),
			p.Branch, c.MaxChangedLines, f.AutoFix.Description, f.Title)
	case FixCleanupWorktrees:
		tier = wire.TierFlash
		task = fmt.Sprintf(cleanupTask, c.RepoDir, strings.Join(f.AutoFix.Targets, "\n"))
	default:
		return wire.JobSpec{}, fmt.Errorf("fix kind %q runs in the factory, not as a job", f.AutoFix.Kind)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return wire.JobSpec{}, err
	}
	spec := wire.NewJobSpec(task, CreatedBy)
	spec.Payload = b
	spec.Tier = tier
	spec.Priority = c.JobPriority
	spec.Foreman = c.Foreman
	spec.TimeoutS = wire.TierCeilingS(tier)
	spec.Budget.MaxCostUSD = reserve
	if f.AutoFix.Kind == FixPR {
		spec.ResultSchema = json.RawMessage(prFixSchema)
	}
	return spec, spec.Validate()
}

const prFixTask = `You are an auto-fix job of the factory auditor (spec §6.12). Make ONE small change to %s and push it to a new branch; nothing else.

Steps:
1. Clone fresh into %s/<branch-name-with-slashes-as-dashes> (gh repo clone), create branch %s from origin/master. Never touch /home/price/temp/factory.
2. Make the change below. Touch only these files unless strictly necessary: %s. Run gofumpt and ` + "`make ci`" + `; it must pass.
3. Commit, then ` + "`git push -u origin %s`" + `. Never push to master, never force-push, never open or merge a PR, never deploy: the factory queues the merge itself, and CI's required "audit-guardrail" check (built from master, not your branch) refuses the diff unless it is at most %d changed lines, text only, and touches no spend/budget/throttle, secrets, auth, CI, deploy/merge procedure or auditor files. Never edit .github/, Makefile or the checker.
4. Remove your clone, then report status done with result {"branch": ..., "head": <full sha>, "files": [...], "change": "<one paragraph>"}. If the change cannot be done within these limits, report failed and explain.

The change: %s

Finding it fixes: %s`

const prFixSchema = `{"type":"object","required":["branch","head","change"],"properties":{"branch":{"type":"string"},"head":{"type":"string"},"files":{"type":"array","items":{"type":"string"}},"change":{"type":"string"}}}`

const cleanupTask = `You are a cleanup job of the factory auditor (spec §6.12). In the git repo at %s, remove ONLY these worktrees, one by one:

%s

For each: skip it (and say so) if ` + "`git -C <path> status --porcelain`" + ` is non-empty or its HEAD is not an ancestor of origin/master. Otherwise run ` + "`git worktree remove <path>`" + ` (never --force), then ` + "`git branch -d <branch>`" + ` for its branch (never -D). Do not touch anything else; do not push. Report done with a summary listing removed and skipped paths and the branch + sha of each removed worktree.`

// PRFixResult is a PR fix job's reported result.
type PRFixResult struct {
	Branch string   `json:"branch"`
	Head   string   `json:"head"`
	Files  []string `json:"files"`
	Change string   `json:"change"`
}
