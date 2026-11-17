package auditor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Finding is one audit observation (spec §6.12). Fingerprint identifies it
// across runs (dedup); Metric is its "how bad" number - comparable between
// runs of the same fingerprint - that the 2x re-file rule reads.
type Finding struct {
	Fingerprint string   `json:"fingerprint"`
	Title       string   `json:"title"`
	Evidence    []string `json:"evidence"`
	JobIDs      []string `json:"job_ids,omitempty"`
	Metric      float64  `json:"metric"`
	MetricLabel string   `json:"metric_label"`
	Cause       string   `json:"cause"`
	Fix         string   `json:"fix"`
	Benefit     string   `json:"benefit"`
	Effort      string   `json:"effort"`
	// Known names an issue already being handled elsewhere: the finding
	// is still reported, but never auto-fixed.
	Known string `json:"known,omitempty"`
	// AutoFix is the candidate fix; nil means proposal-only.
	AutoFix *Plan `json:"autofix,omitempty"`
	// Source is "detector" (deterministic) or "sweep" (the audit job).
	Source string `json:"source,omitempty"`
}

// fingerprint builds "<kind>:<key>" with the key hashed when long.
func fingerprint(kind, key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	if len(key) > 40 || strings.ContainsAny(key, " \t\n") {
		sum := sha256.Sum256([]byte(key))
		key = hex.EncodeToString(sum[:6])
	}
	return kind + ":" + key
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// SweepFingerprint normalizes a fingerprint the audit job proposes: a
// known detector fingerprint is kept verbatim, anything else is slugged
// under "sweep:".
func SweepFingerprint(fp string, known map[string]bool) string {
	if known[fp] {
		return fp
	}
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(fp), "-"), "-")
	s = strings.TrimPrefix(s, "sweep-")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		s = "unnamed"
	}
	return "sweep:" + s
}

// ProposalTitle is the task title of a filed finding.
func ProposalTitle(f Finding) string { return clip(ProposalPrefix+f.Title, 200) }

// AutoFixTitle is the informational task title of an auto-fix.
func AutoFixTitle(f Finding) string { return clip(AutoFixPrefix+f.Title, 200) }

// ProposalBody renders the operator-facing proposal (evidence, likely
// cause, proposed fix, benefit, effort) plus how to act on it.
func ProposalBody(f Finding, why string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Audit proposal:** %s\n\n", f.Title)
	b.WriteString("**Evidence**\n")
	for _, e := range f.Evidence {
		fmt.Fprintf(&b, "- %s\n", e)
	}
	if len(f.JobIDs) > 0 {
		fmt.Fprintf(&b, "- Jobs: %s\n", strings.Join(limit(f.JobIDs, 12), ", "))
	}
	fmt.Fprintf(&b, "- Metric: %s = %s\n\n", f.MetricLabel, trimFloat(f.Metric))
	fmt.Fprintf(&b, "**Likely cause:** %s\n\n", f.Cause)
	fmt.Fprintf(&b, "**Proposed fix:** %s\n\n", f.Fix)
	fmt.Fprintf(&b, "**Estimated benefit:** %s\n\n", f.Benefit)
	fmt.Fprintf(&b, "**Estimated effort:** %s\n\n", f.Effort)
	if f.Known != "" {
		fmt.Fprintf(&b, "**Known issue:** %s\n\n", f.Known)
	}
	if why != "" {
		fmt.Fprintf(&b, "**Why this is a proposal, not an auto-fix:** %s\n\n", why)
	}
	b.WriteString("Reply to approve or edit it; the planner then dispatches the fix through branch -> CI -> merge queue -> deploy. Close the task to reject it (it is only re-filed if the metric at least doubles).\n\n")
	fmt.Fprintf(&b, "`fingerprint: %s`", f.Fingerprint)
	return b.String()
}

// AutoFixBody renders the informational task opened when an auto-fix
// starts: what will change, the job, and exactly how to revert.
func AutoFixBody(f Finding, jobID, run string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Auto-fix started** by audit run %s (no action needed).\n\n", run)
	fmt.Fprintf(&b, "**Finding:** %s\n", f.Title)
	for _, e := range limit(f.Evidence, 6) {
		fmt.Fprintf(&b, "- %s\n", e)
	}
	p := f.AutoFix
	fmt.Fprintf(&b, "\n**Change (%s):** %s\n", p.Kind, p.Description)
	if len(p.Paths) > 0 {
		fmt.Fprintf(&b, "- Files: %s (est. %d lines)\n", strings.Join(p.Paths, ", "), p.EstLines)
	}
	if len(p.Targets) > 0 {
		fmt.Fprintf(&b, "- Targets: %s\n", strings.Join(p.Targets, ", "))
	}
	if jobID != "" {
		fmt.Fprintf(&b, "- Fix job: %s\n", jobID)
	}
	fmt.Fprintf(&b, "\n**How to revert:** %s\n\n", p.Revert)
	if p.Kind == FixPR {
		b.WriteString("The branch only enters the merge queue after the factory checks its real diff against the guardrails; the outcome is posted here.\n\n")
	}
	fmt.Fprintf(&b, "`fingerprint: %s`", f.Fingerprint)
	return b.String()
}

// SortFindings orders findings most-severe first (by metric within the
// auto-fixable ones first, so the per-run cap goes to fixable findings).
func SortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ai, aj := fs[i].AutoFix != nil, fs[j].AutoFix != nil
		if ai != aj {
			return ai
		}
		if fs[i].Metric != fs[j].Metric {
			return fs[i].Metric > fs[j].Metric
		}
		return fs[i].Fingerprint < fs[j].Fingerprint
	})
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func limit(ss []string, n int) []string {
	if len(ss) <= n {
		return ss
	}
	out := append([]string{}, ss[:n]...)
	return append(out, fmt.Sprintf("(+%d more)", len(ss)-n))
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}
