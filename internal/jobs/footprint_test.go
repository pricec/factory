package jobs

import (
	"context"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func TestGlobsOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"*", "internal/jobs/jobs.go", true},
		{"**", "x", true},
		{"internal/jobs/jobs.go", "internal/jobs/jobs.go", true},
		{"internal/jobs/jobs.go", "internal/jobs/held.go", false},
		{"internal/jobs/**", "internal/jobs/jobs.go", true},
		{"internal/jobs/**", "internal/factoryapp/**", false},
		{"internal/**", "internal/jobs/jobs.go", true},
		{"internal/**/*.go", "internal/jobs/jobs.go", true},
		{"internal/**/*.go", "internal/jobs/jobs.md", false},
		{"**/*.go", "**/*.md", false},
		{"**/*.go", "cmd/**", true},
		{"*.go", "internal/x.go", false}, // `*` stays within one segment
		{"*.go", "x.go", true},
		{"internal/*", "internal/jobs/jobs.go", true}, // * may be a directory
		{"internal/*.go", "internal/jobs/jobs.go", false},
		{"internal/jobs", "internal/jobs/jobs.go", true}, // plain path may be a directory
		{"internal/jobs/", "internal/jobs/held.go", true},
		{"internal/jobs/jobs.go", "internal/jobs/jobs_test.go", false},
		{"internal/jobs/jobs*.go", "internal/jobs/*_test.go", true},
		{"internal/jobs/j?bs.go", "internal/jobs/jobs.go", true},
		{"internal/jobs/[jh]*.go", "internal/jobs/held.go", true},
		{"a/b/c", "a/b", true},
		{"a/b/c", "a/x", false},
		{"./docs/spec.md", "docs/spec.md", true},
		{"docs/**", "**", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/c.go", false},
		{"a/**/b", "a/x/c", true}, // c may be a directory holding b
		{"a*b", "a*c", false},
		{"a*b", "ab*", true},
	} {
		if got := GlobsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("GlobsOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := GlobsOverlap(tc.b, tc.a); got != tc.want {
			t.Errorf("GlobsOverlap(%q, %q) (swapped) = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestIsDocGlob(t *testing.T) {
	for g, want := range map[string]bool{
		"docs/spec.md": true, "docs/**": true, "README.md": true, "**/*.md": true,
		"internal/jobs/**": false, "*": false, "**": false, "internal/x.go": false,
	} {
		if IsDocGlob(g) != want {
			t.Errorf("IsDocGlob(%q) = %v, want %v", g, !want, want)
		}
	}
}

func TestCheckConflict(t *testing.T) {
	fp := func(id, created, state string, prio int, touches ...string) Footprint {
		return Footprint{JobID: id, CreatedAt: created, State: state, Priority: prio, Touches: touches}
	}
	cand := func(touches ...string) Candidate {
		return Candidate{JobID: "job_C", Touches: touches, CreatedAt: "2026-11-18T10:00:00Z", Priority: 5}
	}
	for _, tc := range []struct {
		name     string
		c        Candidate
		active   []Footprint
		wantHard string // blocker id, "" for none
		wantGlob string
		wantSoft int
	}{
		{
			"disjoint admitted", cand("internal/a/**"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5, "internal/b/**")},
			"", "", 0,
		},
		{
			"overlapping held", cand("internal/jobs/**"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5, "internal/jobs/jobs.go")},
			"job_A", "internal/jobs/jobs.go", 0,
		},
		{
			"docs-only overlap is soft", cand("docs/spec.md"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5, "docs/**")},
			"", "", 1,
		},
		{
			"md overlap is soft", cand("**/*.md"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5, "README.md")},
			"", "", 1,
		},
		{
			"hard beats soft in the same job", cand("docs/spec.md", "internal/x.go"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5, "docs/spec.md", "internal/x.go")},
			"job_A", "internal/x.go", 0,
		},
		{
			"no touches is never held", cand(),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5, "*")},
			"", "", 0,
		},
		{
			"blocker without touches ignored", cand("internal/x.go"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5)},
			"", "", 0,
		},
		{
			"star blocks everything with touches", cand("internal/x.go"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5, "*")},
			"job_A", "*", 0,
		},
		{
			"star candidate waits on any declared job", cand("*"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5, "docs/spec.md")},
			"job_A", "docs/spec.md", 0,
		},
		{
			"star is not soft vs docs", cand("docs/spec.md"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5, "*")},
			"job_A", "*", 0,
		},
		{
			"never waits on a newer job", cand("internal/x.go"),
			[]Footprint{fp("job_N", "2026-11-18T11:00:00Z", StateRunning, 5, "internal/x.go")},
			"", "", 0,
		},
		{
			"same second: older id first", cand("internal/x.go"),
			[]Footprint{fp("job_D", "2026-11-18T10:00:00Z", StateRunning, 5, "internal/x.go")},
			"", "", 0,
		},
		{
			"same second: lower id blocks", cand("internal/x.go"),
			[]Footprint{fp("job_B", "2026-11-18T10:00:00Z", StateRunning, 5, "internal/x.go")},
			"job_B", "internal/x.go", 0,
		},
		{
			"older held job blocks (FIFO)", cand("internal/x.go"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateHeld, 5, "internal/x.go")},
			"job_A", "internal/x.go", 0,
		},
		{
			"held job of lower urgency does not block", cand("internal/x.go"),
			[]Footprint{fp("job_A", "2026-11-18T09:00:00Z", StateHeld, 7, "internal/x.go")},
			"", "", 0,
		},
		{
			"held job of higher urgency blocks even if newer",
			Candidate{JobID: "job_C", Touches: []string{"internal/x.go"}, CreatedAt: "2026-11-18T10:00:00Z", Priority: 5},
			[]Footprint{fp("job_Z", "2026-11-18T12:00:00Z", StateHeld, 2, "internal/x.go")},
			"job_Z", "internal/x.go", 0,
		},
		{
			"oldest blocker is reported", cand("internal/x.go"),
			[]Footprint{
				fp("job_B", "2026-11-18T09:30:00Z", StateRunning, 5, "internal/x.go"),
				fp("job_A", "2026-11-18T09:00:00Z", StateRunning, 5, "internal/**"),
			},
			"job_A", "internal/**", 0,
		},
		{
			"itself ignored", cand("internal/x.go"),
			[]Footprint{fp("job_C", "2026-11-18T09:00:00Z", StateRunning, 5, "internal/x.go")},
			"", "", 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hard, soft := CheckConflict(tc.c, tc.active)
			switch {
			case tc.wantHard == "" && hard != nil:
				t.Fatalf("unexpected hold on %s (%s)", hard.JobID, hard.Glob)
			case tc.wantHard != "" && hard == nil:
				t.Fatalf("expected a hold on %s", tc.wantHard)
			case hard != nil && (hard.JobID != tc.wantHard || hard.Glob != tc.wantGlob):
				t.Fatalf("hold on %s (%s), want %s (%s)", hard.JobID, hard.Glob, tc.wantHard, tc.wantGlob)
			}
			if len(soft) != tc.wantSoft {
				t.Fatalf("soft = %d, want %d", len(soft), tc.wantSoft)
			}
		})
	}
}

func TestConflictReasonKinds(t *testing.T) {
	r := ConflictReason("job_X", "docs/**")
	if r != "conflict: waits for job_X (docs/**)" {
		t.Fatalf("reason %q", r)
	}
	for reason, want := range map[string]string{
		r: "conflict", "claude auth: foreman x logged out": "claude_auth", "usage RED: 5h 96%": "usage", "": "usage",
	} {
		if got := HeldKind(reason); got != want {
			t.Errorf("HeldKind(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestValidateTouches(t *testing.T) {
	long := make([]byte, wire.MaxTouchLen+1)
	for i := range long {
		long[i] = 'a'
	}
	for _, tc := range []struct {
		name string
		in   []string
		ok   bool
	}{
		{"none", nil, true},
		{"globs", []string{"internal/jobs/**", "docs/spec.md", "*"}, true},
		{"dir slash", []string{"internal/jobs/"}, true},
		{"empty entry", []string{""}, false},
		{"blank entry", []string{"  "}, false},
		{"absolute", []string{"/etc/passwd"}, false},
		{"dotdot", []string{"a/../b"}, false},
		{"leading dotdot", []string{"../x"}, false},
		{"empty segment", []string{"a//b"}, false},
		{"backslash", []string{`a\b`}, false},
		{"control char", []string{"a\x00b"}, false},
		{"too long", []string{string(long)}, false},
	} {
		if err := wire.ValidateTouches(tc.in); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
		}
	}
	many := make([]string, wire.MaxTouches+1)
	for i := range many {
		many[i] = "a"
	}
	if wire.ValidateTouches(many) == nil {
		t.Error("too many entries accepted")
	}
	spec := wire.NewJobSpec("t", "x")
	spec.ID = "job_1"
	spec.Touches = []string{"../x"}
	if err := spec.Validate(); err == nil {
		t.Error("spec.Validate accepted ../x")
	}
}

// TestActiveFootprints: which jobs count as active for conflicts, and when
// a blocker stops blocking (its merge landed / was cancelled / failed).
func TestActiveFootprints(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	mk := func(id string, state string, touches ...string) {
		t.Helper()
		spec := wire.NewJobSpec("t", "test")
		spec.ID = id
		spec.Touches = touches
		if err := s.Create(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE jobs SET state = ? WHERE id = ?`, state, id); err != nil {
			t.Fatal(err)
		}
	}
	mk("job_run", StateRunning, "a/**")
	mk("job_pend", StatePending, "b/**")
	mk("job_held", StateHeld, "c/**")
	mk("job_none", StateRunning) // no touches
	mk("job_failed_term", StateDead, "d/**")
	mk("job_done_nomr", StateDone, "e/**")
	mk("job_done_mr", StateDone, "f/**")
	mk("job_done_landed", StateDone, "g/**")
	mk("job_done_cancelled", StateDone, "h/**")
	mk("job_done_attn", StateDone, "i/**")
	mr := func(job, branch string) MergeRequest {
		t.Helper()
		m, err := s.CreateMergeRequest(ctx, NewMergeRequest{Branch: branch, Head: testHead, Note: "n", JobID: job})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	mr("job_done_mr", "br-queued")
	for job, to := range map[string]string{
		"job_done_landed": MergeLanded, "job_done_cancelled": MergeCancelled, "job_done_attn": MergeNeedsAttention,
	} {
		m := mr(job, "br-"+job)
		if _, err := s.db.Exec(`UPDATE merge_requests SET state = ? WHERE id = ?`, MergeMerging, m.ID); err != nil {
			t.Fatal(err)
		}
		if _, applied, err := s.TransitionMerge(ctx, m.ID, to, "x"); err != nil || !applied {
			t.Fatalf("transition %s: %v %v", to, applied, err)
		}
	}
	got, err := s.ActiveFootprints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]Footprint{}
	for _, f := range got {
		ids[f.JobID] = f
	}
	want := []string{"job_run", "job_pend", "job_held", "job_done_mr"}
	if len(ids) != len(want) {
		t.Fatalf("active = %v, want %v", ids, want)
	}
	for _, id := range want {
		if _, ok := ids[id]; !ok {
			t.Errorf("%s missing from active footprints", id)
		}
	}
	if ids["job_done_mr"].MergeRequestID == "" || ids["job_run"].Touches[0] != "a/**" {
		t.Errorf("bad footprint fields: %+v", ids)
	}
}

func TestConflictSinceAndHeldKinds(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	spec := wire.NewJobSpec("t", "test")
	spec.ID = "job_h"
	if err := s.CreateHeld(ctx, spec, ConflictReason("job_x", "a/**")); err != nil {
		t.Fatal(err)
	}
	spec2 := wire.NewJobSpec("t", "test")
	spec2.ID = "job_u"
	if err := s.CreateHeld(ctx, spec2, "usage ORANGE: x"); err != nil {
		t.Fatal(err)
	}
	by, err := s.CountHeldByKind(ctx)
	if err != nil || by["conflict"] != 1 || by["usage"] != 1 {
		t.Fatalf("by kind %v %v", by, err)
	}
	// Conflict holds do not count as "ahead" for the usage gate.
	if n, _ := s.CountHeldAhead(ctx, 5); n != 1 {
		t.Fatalf("held ahead = %d, want 1 (usage only)", n)
	}
	if err := s.MarkConflictSince(ctx, "job_h", "2026-11-18T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConflictSince(ctx, "job_h", "2026-11-18T11:00:00Z"); err != nil { // kept
		t.Fatal(err)
	}
	if since, _ := s.ConflictSince(ctx, "job_h"); since != "2026-11-18T10:00:00Z" {
		t.Fatalf("since %q", since)
	}
	if ok, err := s.Release(ctx, "job_h"); err != nil || !ok {
		t.Fatalf("release %v %v", ok, err)
	}
	if since, _ := s.ConflictSince(ctx, "job_h"); since != "" {
		t.Fatalf("since after release %q", since)
	}
}
