package auditor

import (
	"strings"
	"testing"
	"time"
)

func TestParseSweepAndValidateWorktrees(t *testing.T) {
	raw := []byte(`{"findings":[],"worktrees":[
{"path":"/home/price/work/a","branch":"refs/heads/feat/a","head":"ABCDEF1","merged":true,"clean":true,"last_commit":"2026-11-01T10:00:00Z"},
{"path":"/home/price/work/../temp/factory","head":"abcdef2","merged":true,"clean":true},
{"path":"/home/price/temp/factory","head":"abcdef3","merged":true,"clean":true},
{"path":"/home/price/work","head":"abcdef4","merged":true,"clean":true},
{"path":"/home/price/work/a","head":"abcdef5","merged":true,"clean":true},
{"path":"/home/price/work/b","head":"not-a-sha","merged":true,"clean":true},
{"path":"/home/price/work/c","branch":"x;rm -rf /","head":"abcdef6","merged":true,"clean":true},
{"path":"/home/price/work/p","prunable":true,"merged":true,"clean":true},
{"path":"/home/price/work/d","head":"abcdef7","merged":true,"clean":false,"last_commit":"garbage"}]}`)
	r, err := ParseSweep(raw)
	if err != nil {
		t.Fatal(err)
	}
	kept, rejected := ValidateWorktrees(r.Worktrees, "/home/price/work/")
	var paths []string
	for _, w := range kept {
		paths = append(paths, w.Path)
	}
	if got := strings.Join(paths, ","); got != "/home/price/work/a,/home/price/work/p,/home/price/work/d" {
		t.Fatalf("kept = %s (rejected %v)", got, rejected)
	}
	if len(rejected) != 6 {
		t.Fatalf("rejected = %d: %v", len(rejected), rejected)
	}
	if kept[0].Branch != "feat/a" || kept[0].Head != "abcdef1" || kept[0].LastCommit.IsZero() {
		t.Fatalf("normalised entry = %+v", kept[0])
	}
	if kept[1].Merged || kept[1].Clean {
		t.Fatalf("a prunable worktree must never be a cleanup target: %+v", kept[1])
	}
	if !kept[2].LastCommit.IsZero() {
		t.Fatalf("bad timestamp parsed: %+v", kept[2])
	}
	// Only merged AND clean worktrees under the root become cleanup targets.
	snap := Snapshot{Now: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	if rej := ApplySweepScan(&snap, r, "/home/price/work"); len(rej) != 6 {
		t.Fatalf("ApplySweepScan rejected %d", len(rej))
	}
	pc := ContextFromSnapshot(snap, DefaultConfig())
	for _, c := range []struct {
		target string
		ok     bool
	}{{"/home/price/work/a", true}, {"/home/price/work/d", false}, {"/home/price/work/p", false}, {"/home/price/temp/factory", false}} {
		err := CheckPlan(Plan{Kind: FixCleanupWorktrees, Description: "rm", Revert: "re-add", Targets: []string{c.target}}, pc)
		if (err == nil) != c.ok {
			t.Errorf("cleanup of %s: err=%v, want ok=%v", c.target, err, c.ok)
		}
	}
}

func TestApplySweepScanErrors(t *testing.T) {
	var snap Snapshot
	ApplySweepScan(&snap, SweepResult{WorktreeScanError: "not a git repo"}, "/home/price/work")
	if !strings.Contains(snap.WorktreeErr, "not a git repo") || snap.Worktrees != nil {
		t.Fatalf("scan error: %+v", snap)
	}
	ApplySweepScan(&snap, SweepResult{}, "/home/price/work")
	if snap.WorktreeErr == "" {
		t.Fatal("a sweep without a worktrees key must count as no scan")
	}
	if fs := DetectWorktrees(snap, DetectOptions{WorktreeRoot: "/home/price/work"}); len(fs) != 0 {
		t.Fatalf("worktree findings without a scan: %+v", fs)
	}
	if _, rej := ValidateWorktrees([]Worktree{{Path: "/x/y", Head: "abcdef1"}}, "/"); len(rej) != 1 {
		t.Fatal("root / must be refused")
	}
}

func TestParseNumstatZ(t *testing.T) {
	out := []byte("3\t1\tdocs/a.md\x00-\t-\tbin.png\x002\t0\t\x00docs/old.md\x00internal/auth/new.md\x00")
	fs := ParseDiff(out)
	if len(fs) != 4 || fs[0].Path != "docs/a.md" || fs[1].Added != -1 || fs[2].Path != "docs/old.md" || fs[3].Path != "internal/auth/new.md" {
		t.Fatalf("parsed = %+v", fs)
	}
	if n, _ := ChangedLines([]FileStat{fs[0], fs[2], fs[3]}); n != 6 {
		t.Fatalf("a rename's lines must count once: %d", n)
	}
	if err := CheckDiff(fs, 150); err == nil {
		t.Fatal("binary + rename into a forbidden area must be refused")
	}
	trunc := ParseNumstatZ([]byte("1\t1\t\x00only-src"))
	if len(trunc) != 1 || trunc[0].Added != -1 {
		t.Fatalf("truncated rename record must be refused: %+v", trunc)
	}
	if err := CheckDiff(ParseDiff(nil), 150); err == nil {
		t.Fatal("an empty diff must be refused")
	}
}

func TestIsDeadHeld(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	base := JobRow{
		State: "held", TaskID: "task_1", TaskOpen: false, CreatedBy: "factory",
		CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-7 * time.Hour),
	}
	for _, c := range []struct {
		name string
		mut  func(*JobRow)
		want bool
	}{
		{"planner job under a closed operator task", func(*JobRow) {}, true},
		{"operator-created job", func(j *JobRow) { j.CreatedBy = "ui" }, false},
		{"task still open", func(j *JobRow) { j.TaskOpen = true }, false},
		{"no task", func(j *JobRow) { j.TaskID = "" }, false},
		{"held under 6h (old job, recent hold)", func(j *JobRow) { j.UpdatedAt = now.Add(-5 * time.Hour) }, false},
		{"not held", func(j *JobRow) { j.State = "queued" }, false},
		{"no hold stamp falls back to created_at", func(j *JobRow) { j.UpdatedAt = time.Time{} }, true},
	} {
		j := base
		c.mut(&j)
		if got := isDeadHeld(j, now); got != c.want {
			t.Errorf("%s: isDeadHeld = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDefaultsOperatorDecisions(t *testing.T) {
	c := DefaultConfig()
	if !c.Enabled || !c.AutoFix || c.MaxAutoFixes != 5 || c.BudgetUSD != 10 || c.MaxChangedLines != 150 || c.WorktreeRoot != "/home/price/work" {
		t.Fatalf("defaults = %+v", c)
	}
	if !strings.HasPrefix(FixBranch("job_0123456789ABCDEF", 1), FixBranchPrefix) {
		t.Fatal("fix branches must use the CI-checked prefix")
	}
	for _, p := range []string{".github/workflows/audit-guardrail.yml", ".github/workflows/ci.yml", "cmd/auditcheck/main.go", "Makefile"} {
		if err := CheckDiff([]FileStat{{Path: p, Added: 1}}, 150); err == nil {
			t.Errorf("%s must be on the denylist", p)
		}
	}
}
