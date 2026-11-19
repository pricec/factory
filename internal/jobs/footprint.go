package jobs

import (
	"strings"
)

// Conflict coordinator (spec §6.10, 2026-11-18 amendment): jobs may declare
// a file footprint (JobSpec.touches, repo-relative path globs). This file
// holds the pure pieces: glob overlap and the admission decision. The store
// query (ActiveFootprints) and the wiring into the dispatch gate live in
// footprint_store.go and factoryapp/conflict.go.

// TouchAll is the exclusivity footprint: it overlaps every other declared
// footprint, including itself.
const TouchAll = "*"

// segment tokens of a single path segment pattern.
const (
	tokLit  = iota // a literal byte
	tokOne         // `?` or a `[...]` class: exactly one byte (class treated conservatively as any byte)
	tokStar        // `*`: any run of bytes within the segment
)

type segTok struct {
	kind int
	b    byte
}

func tokenizeSeg(seg string) []segTok {
	var out []segTok
	for i := 0; i < len(seg); i++ {
		switch c := seg[i]; c {
		case '*':
			if n := len(out); n == 0 || out[n-1].kind != tokStar {
				out = append(out, segTok{kind: tokStar})
			}
		case '?':
			out = append(out, segTok{kind: tokOne})
		case '[':
			if j := strings.IndexByte(seg[i:], ']'); j > 1 {
				out = append(out, segTok{kind: tokOne})
				i += j
			} else {
				out = append(out, segTok{kind: tokLit, b: c})
			}
		default:
			out = append(out, segTok{kind: tokLit, b: c})
		}
	}
	return out
}

// segOverlap reports whether some string matches both segment patterns.
func segOverlap(a, b string) bool {
	if a == b {
		return true
	}
	ta, tb := tokenizeSeg(a), tokenizeSeg(b)
	memo := map[[2]int]bool{}
	seen := map[[2]int]bool{}
	var rec func(i, j int) bool
	rec = func(i, j int) bool {
		k := [2]int{i, j}
		if seen[k] {
			return memo[k]
		}
		seen[k] = true
		var r bool
		switch {
		case i == len(ta) && j == len(tb):
			r = true
		case i < len(ta) && ta[i].kind == tokStar:
			r = rec(i+1, j) || (j < len(tb) && rec(i, j+1))
		case j < len(tb) && tb[j].kind == tokStar:
			r = rec(i, j+1) || (i < len(ta) && rec(i+1, j))
		case i == len(ta) || j == len(tb):
			r = false
		default:
			x, y := ta[i], tb[j]
			r = (x.kind == tokOne || y.kind == tokOne || x.b == y.b) && rec(i+1, j+1)
		}
		memo[k] = r
		return r
	}
	return rec(0, 0)
}

// dirCapable reports whether a final path segment may name a directory: it
// has no file extension (a "." after the first byte). "jobs" and "*" may be
// directories; "jobs.go", "*.md" and "spec.md" are files.
func dirCapable(seg string) bool { return !strings.Contains(seg[min(1, len(seg)):], ".") }

// normGlob splits a glob into path segments. A leading "./" is dropped; a
// trailing "/" means the directory's whole subtree; a pattern whose last
// segment may name a directory (dirCapable) also covers everything beneath
// it (conservative).
func normGlob(g string) []string {
	g = strings.TrimPrefix(g, "./")
	dir := strings.HasSuffix(g, "/")
	segs := strings.Split(strings.Trim(g, "/"), "/")
	if dir || (dirCapable(segs[len(segs)-1]) && segs[len(segs)-1] != "**") {
		segs = append(segs, "**")
	}
	// collapse repeated ** segments
	out := segs[:0]
	for _, s := range segs {
		if s == "**" && len(out) > 0 && out[len(out)-1] == "**" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func isAll(g string) bool { return g == TouchAll || g == "**" }

// GlobsOverlap reports whether some repo path could match both globs. It is
// conservative: a character class is treated as any single character, and a
// pattern naming a plain path also covers that path as a directory. "*"
// alone (TouchAll) and "**" overlap everything. Otherwise `*`/`?`/`[..]`
// work within one path segment and a whole `**` segment matches any number
// of segments (including none).
func GlobsOverlap(a, b string) bool {
	if isAll(a) || isAll(b) {
		return true
	}
	sa, sb := normGlob(a), normGlob(b)
	seen := map[[2]int]bool{}
	memo := map[[2]int]bool{}
	var rec func(i, j int) bool
	rec = func(i, j int) bool {
		k := [2]int{i, j}
		if seen[k] {
			return memo[k]
		}
		seen[k] = true
		var r bool
		switch {
		case i == len(sa) && j == len(sb):
			r = true
		case i < len(sa) && sa[i] == "**":
			r = rec(i+1, j) || (j < len(sb) && rec(i, j+1))
		case j < len(sb) && sb[j] == "**":
			r = rec(i, j+1) || (i < len(sa) && rec(i+1, j))
		case i == len(sa) || j == len(sb):
			r = false
		default:
			r = segOverlap(sa[i], sb[j]) && rec(i+1, j+1)
		}
		memo[k] = r
		return r
	}
	return rec(0, 0)
}

// IsDocGlob reports whether g only matches documentation: anything under
// docs/ or a Markdown file. Overlap on such globs is soft (never holds).
// TouchAll is never a doc glob.
func IsDocGlob(g string) bool {
	if isAll(g) {
		return false
	}
	g = strings.TrimPrefix(g, "./")
	segs := strings.Split(strings.Trim(g, "/"), "/")
	if segs[0] == "docs" {
		return true
	}
	return strings.HasSuffix(segs[len(segs)-1], ".md")
}

// Footprint is one job's declared footprint as the coordinator sees it.
type Footprint struct {
	JobID   string
	Touches []string
	// Repo is the normalized target repo ("" = unknown).
	Repo      string
	CreatedAt string
	Priority  int
	// State is the job state; Held jobs count as blockers only for jobs
	// that release after them (release order).
	State string
	// MergeRequestID is set for a done job still waiting on its merge.
	MergeRequestID string
}

// Candidate is the job being admitted.
type Candidate struct {
	JobID   string
	Touches []string
	// Repo is the normalized target repo ("" = unknown).
	Repo      string
	CreatedAt string
	Priority  int
}

// NormalizeRepo returns the canonical OWNER/REPO form of a repo name: a
// bare name ("video-chat") gets the default owner, case is folded, and a
// ".git" suffix or surrounding space is dropped. "" stays "" (unknown).
func NormalizeRepo(r string) string {
	r = strings.ToLower(strings.TrimSpace(r))
	r = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(r, "https://github.com/"), "github.com/"), ".git")
	r = strings.Trim(r, "/")
	if r == "" {
		return ""
	}
	if !strings.Contains(r, "/") {
		r = defaultRepoOwner + "/" + r
	}
	return r
}

const defaultRepoOwner = "pricec"

// reposMayConflict scopes conflicts per repo: different known repos never
// conflict; two unknown repos are compared as usual; a known repo against
// an unknown one conflicts only through a TouchAll ("*") footprint on
// either side.
func reposMayConflict(a, b string, aTouches, bTouches []string) bool {
	switch {
	case a == b:
		return true
	case a != "" && b != "":
		return false
	}
	return hasTouchAll(aTouches) || hasTouchAll(bTouches)
}

func hasTouchAll(touches []string) bool {
	for _, t := range touches {
		if isAll(t) {
			return true
		}
	}
	return false
}

// Conflict says which active job a candidate overlaps, and on which globs.
type Conflict struct {
	JobID     string
	Glob      string // the blocker's glob
	OwnGlob   string // the candidate's glob
	Footprint Footprint
}

// before reports whether f sorts before c in admission order: FIFO by
// created_at then id; a held blocker must also not be less urgent.
func (f Footprint) before(c Candidate) bool {
	if f.State == StateHeld && f.Priority != c.Priority {
		return f.Priority < c.Priority
	}
	if f.CreatedAt != c.CreatedAt {
		return f.CreatedAt < c.CreatedAt
	}
	return f.JobID < c.JobID
}

// CheckConflict is the pure admission decision. It returns the first hard
// conflict (a blocker the candidate must wait for) and every soft one
// (docs-only overlap: noted, never holding). Rules:
//   - a candidate with no touches conflicts with nothing; a blocker with
//     none is ignored;
//   - a candidate never waits on a job that sorts after it;
//   - conflicts are per repo: jobs in different known repos never
//     conflict; a job of unknown repo conflicts with another unknown-repo
//     job, and with a known-repo job only when one side declares "*";
//   - overlap where either glob is a doc glob is soft, unless either is
//     TouchAll (exclusivity is never soft).
//
// active may be in any order; the blocker returned is the one that sorts
// first (oldest), so the reason is stable between passes.
func CheckConflict(c Candidate, active []Footprint) (hard *Conflict, soft []Conflict) {
	if len(c.Touches) == 0 {
		return nil, nil
	}
	for _, f := range active {
		if f.JobID == c.JobID || len(f.Touches) == 0 || (IsTerminalState(f.State) && f.State != StateDone) || !f.before(c) || !reposMayConflict(c.Repo, f.Repo, c.Touches, f.Touches) {
			continue
		}
		cf, ok := overlapBetween(c.Touches, f)
		if !ok {
			continue
		}
		if cf.Glob != TouchAll && cf.OwnGlob != TouchAll && (IsDocGlob(cf.Glob) || IsDocGlob(cf.OwnGlob)) {
			soft = append(soft, cf)
			continue
		}
		if hard == nil || less(cf.Footprint, hard.Footprint) {
			cp := cf
			hard = &cp
		}
	}
	return hard, soft
}

func less(a, b Footprint) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt < b.CreatedAt
	}
	return a.JobID < b.JobID
}

// overlapBetween finds the best overlapping pair, preferring a hard (non
// doc) pair over a soft one, so a job with both kinds of overlap is held.
func overlapBetween(own []string, f Footprint) (Conflict, bool) {
	var softPair *Conflict
	for _, og := range own {
		for _, fg := range f.Touches {
			if !GlobsOverlap(og, fg) {
				continue
			}
			c := Conflict{JobID: f.JobID, Glob: fg, OwnGlob: og, Footprint: f}
			if og != TouchAll && fg != TouchAll && (IsDocGlob(og) || IsDocGlob(fg)) {
				if softPair == nil {
					softPair = &c
				}
				continue
			}
			return c, true
		}
	}
	if softPair != nil {
		return *softPair, true
	}
	return Conflict{}, false
}

// ConflictHoldPrefix marks a held_reason as a conflict hold (the usage
// holds read "usage ..."; claude-auth holds "claude auth: ").
const ConflictHoldPrefix = "conflict: "

// ConflictReason formats the held_reason of a conflict hold.
func ConflictReason(blocker, glob string) string {
	return ConflictHoldPrefix + "waits for " + blocker + " (" + glob + ")"
}

// HeldKind classifies a held_reason: "conflict", "claude_auth", "fable" or "usage"
// (everything else the gate produces).
func HeldKind(reason string) string {
	switch {
	case strings.HasPrefix(reason, ConflictHoldPrefix):
		return "conflict"
	case strings.HasPrefix(reason, "claude auth: "):
		return "claude_auth"
	case strings.HasPrefix(reason, FableHoldPrefix):
		return "fable"
	}
	return "usage"
}
