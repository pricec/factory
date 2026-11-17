package auditor

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// The worktree scan runs INSIDE the audit job on the integration foreman
// (spec §6.12), never in the factory service: the job runs the read-only
// `git worktree list` / `merge-base --is-ancestor` / `status` / `log`
// commands and reports the result under "worktrees". The factory treats
// that report as untrusted input: ValidateWorktrees keeps only entries
// under the worktree root, and the cleanup gate (ContextFromSnapshot ->
// CheckPlan) only accepts targets the report flagged merged AND clean.
// The cleanup job re-checks both on disk before removing anything.

// MaxReportedWorktrees bounds what one sweep may report.
const MaxReportedWorktrees = 200

// SweepResult is the structured part of the audit job's result.
type SweepResult struct {
	Findings  []Finding  `json:"findings"`
	Worktrees []Worktree `json:"worktrees"`
	// WorktreeScanError is set when the job could not run the scan.
	WorktreeScanError string `json:"worktree_scan_error,omitempty"`
	Notes             string `json:"notes,omitempty"`
}

// reportedWorktree is one scan entry as the job reports it (last_commit
// is a string so one bad timestamp does not void the whole report).
type reportedWorktree struct {
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	Head       string `json:"head"`
	Merged     bool   `json:"merged"`
	Clean      bool   `json:"clean"`
	Prunable   bool   `json:"prunable"`
	LastCommit string `json:"last_commit"`
}

// ParseSweep reads the audit job's result. Worktrees is nil when the job
// reported no "worktrees" key at all.
func ParseSweep(result json.RawMessage) (SweepResult, error) {
	var raw struct {
		Findings          []Finding           `json:"findings"`
		Worktrees         *[]reportedWorktree `json:"worktrees"`
		WorktreeScanError string              `json:"worktree_scan_error"`
		Notes             string              `json:"notes"`
	}
	var r SweepResult
	if len(result) == 0 {
		return r, fmt.Errorf("sweep result: empty")
	}
	if err := json.Unmarshal(result, &raw); err != nil {
		return r, fmt.Errorf("sweep result: %w", err)
	}
	r.Findings, r.WorktreeScanError, r.Notes = raw.Findings, raw.WorktreeScanError, raw.Notes
	if raw.Worktrees != nil {
		r.Worktrees = []Worktree{}
		for _, w := range *raw.Worktrees {
			r.Worktrees = append(r.Worktrees, Worktree{
				Path: w.Path, Branch: w.Branch, Head: w.Head, Merged: w.Merged, Clean: w.Clean,
				Prunable: w.Prunable, LastCommit: parseTS(strings.TrimSpace(w.LastCommit)),
			})
		}
	}
	return r, nil
}

var headRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ValidateWorktrees filters a job-reported scan: a worktree is kept only
// when its path is absolute, already clean (no "..", no trailing junk) and
// strictly under root, and (unless prunable) it names a hex head.
// Duplicates keep the first entry. rejected explains every dropped entry.
func ValidateWorktrees(ws []Worktree, root string) (kept []Worktree, rejected []string) {
	root = filepath.Clean(root)
	if root == "" || root == "." || root == "/" || !filepath.IsAbs(root) {
		for _, w := range ws {
			rejected = append(rejected, w.Path+": no valid worktree root configured")
		}
		return nil, rejected
	}
	seen := map[string]bool{}
	for _, w := range ws {
		p := strings.TrimSpace(w.Path)
		switch {
		case len(kept) >= MaxReportedWorktrees:
			rejected = append(rejected, p+": over the report limit")
			continue
		case p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, "\x00\n"):
			rejected = append(rejected, p+": not a clean absolute path")
			continue
		case !strings.HasPrefix(p, root+"/"):
			rejected = append(rejected, p+": outside "+root)
			continue
		case seen[p]:
			rejected = append(rejected, p+": duplicate")
			continue
		}
		w.Path = p
		w.Head = strings.ToLower(strings.TrimSpace(w.Head))
		if !w.Prunable && !headRe.MatchString(w.Head) {
			rejected = append(rejected, p+": no valid head sha")
			continue
		}
		w.Branch = strings.TrimPrefix(strings.TrimSpace(w.Branch), "refs/heads/")
		if strings.ContainsAny(w.Branch, " \t\n\x00;&|`$") {
			rejected = append(rejected, p+": invalid branch name")
			continue
		}
		if w.Prunable {
			w.Merged, w.Clean = false, false // never a cleanup target
		}
		seen[p] = true
		kept = append(kept, w)
	}
	return kept, rejected
}

// ApplySweepScan puts the job-reported scan into snap: the validated
// worktrees, or WorktreeErr (which makes the worktree detectors skip)
// when the job reported none or an error.
func ApplySweepScan(snap *Snapshot, r SweepResult, root string) (rejected []string) {
	snap.Worktrees, snap.WorktreeErr = nil, ""
	if e := strings.TrimSpace(r.WorktreeScanError); e != "" {
		snap.WorktreeErr = "audit job: " + clip(e, 300)
		return nil
	}
	if r.Worktrees == nil {
		snap.WorktreeErr = "the audit job reported no worktree scan"
		return nil
	}
	snap.Worktrees, rejected = ValidateWorktrees(r.Worktrees, root)
	return rejected
}

// IsWorktreeFinding reports whether fp is a worktree detector's
// fingerprint (computed from the job's scan, not at fire time).
func IsWorktreeFinding(fp string) bool { return strings.HasPrefix(fp, "worktree:") }

// DetectWorktrees runs only the worktree detectors (the scan arrives with
// the audit job's result, after the fire-time detectors ran).
func DetectWorktrees(snap Snapshot, o DetectOptions) []Finding {
	if o.StaleAfter <= 0 {
		o.StaleAfter = DefaultConfig().StaleAfter
	}
	return detectWorktrees(snap, o)
}
