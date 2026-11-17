package auditor

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestDecide(t *testing.T) {
	f := Finding{Fingerprint: "error:x", Metric: 6}
	rec := &Record{Fingerprint: "error:x", TaskID: "task_1", Metric: 3}
	cases := []struct {
		name  string
		prior *Record
		task  TaskState
		f     float64
		file  bool
	}{
		{"new", nil, TaskState{}, 6, true},
		{"record without task", &Record{Fingerprint: "error:x"}, TaskState{}, 6, true},
		{"open task: never refile, however bad", rec, TaskState{Exists: true, Open: true}, 600, false},
		{"closed, same", rec, TaskState{Exists: true, CloseReason: "operator"}, 3, false},
		{"closed, 1.9x", rec, TaskState{Exists: true, CloseReason: "operator"}, 5.7, false},
		{"closed, exactly 2x", rec, TaskState{Exists: true, CloseReason: "operator"}, 6, true},
		{"closed, better", rec, TaskState{Exists: true, CloseReason: "rejected"}, 1, false},
		{"closed by planner, 3x", rec, TaskState{Exists: true, CloseReason: "planner"}, 9, true},
		{"zero baseline needs 2", &Record{TaskID: "task_1"}, TaskState{Exists: true}, 1, false},
		{"zero baseline at 2", &Record{TaskID: "task_1"}, TaskState{Exists: true}, 2, true},
		{"task deleted", rec, TaskState{Exists: false}, 1, true},
	}
	for _, c := range cases {
		f.Metric = c.f
		d := Decide(f, c.prior, c.task)
		if d.File != c.file {
			t.Errorf("%s: Decide = %+v, want file=%v", c.name, d, c.file)
		}
		if d.Reason == "" {
			t.Errorf("%s: empty reason", c.name)
		}
	}
}

func TestFindingStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)
	s := &FindingStore{DB: db, Now: func() time.Time { return now }}
	if r, err := s.Get(ctx, "fp"); err != nil || r != nil {
		t.Fatalf("empty get: %v %v", r, err)
	}
	f := Finding{Fingerprint: "fp", Title: "t", Metric: 4}
	if err := s.Filed(ctx, f, "task_a", ModeProposal); err != nil {
		t.Fatal(err)
	}
	f.Metric = 7
	if err := s.Seen(ctx, f); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(ctx, "fp")
	if err != nil || r == nil {
		t.Fatalf("get: %v %v", r, err)
	}
	if r.Metric != 4 || r.LastMetric != 7 || r.TaskID != "task_a" || r.TimesFiled != 1 {
		t.Fatalf("after seen: %+v (baseline must stay at the filed metric)", r)
	}
	// Closed task, 7 < 2x4: not refiled; 8 is.
	closed := TaskState{Exists: true}
	if d := Decide(f, r, closed); d.File {
		t.Fatalf("7 vs baseline 4 refiled: %+v", d)
	}
	f.Metric = 8
	if d := Decide(f, r, closed); !d.File {
		t.Fatalf("8 vs baseline 4 not refiled: %+v", d)
	}
	if err := s.Filed(ctx, f, "task_b", ModeAutoFix); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(ctx, "fp")
	if r.Metric != 8 || r.TaskID != "task_b" || r.TimesFiled != 2 || r.Mode != ModeAutoFix {
		t.Fatalf("after refile: %+v", r)
	}
	if err := s.Rebind(ctx, "fp", "task_b", ModeProposal); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Get(ctx, "fp"); r.Mode != ModeProposal {
		t.Fatalf("rebind: %+v", r)
	}
	// A read-only store on a never-audited database reads as empty.
	ro := &FindingStore{DB: db, ReadOnly: true}
	if r, err := ro.Get(ctx, "other"); err != nil || r != nil {
		t.Fatalf("read-only get: %v %v", r, err)
	}
}

func TestDecideRunGuardrails(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	pr := func(fp string, lines int) Finding {
		return Finding{Fingerprint: fp, Metric: 1, AutoFix: &Plan{
			Kind: FixPR, Description: "fix the docs typo", Paths: []string{"docs/x.md"}, EstLines: lines, Revert: "git revert",
		}}
	}
	var fs []Finding
	for i := 0; i < 8; i++ {
		fs = append(fs, pr("pr:"+string(rune('a'+i)), 5))
	}
	fs = append(fs, pr("pr:big", 400), Finding{Fingerprint: "plain", Metric: 1})
	fs = append(fs, Finding{Fingerprint: "held:dead", Metric: 1, AutoFix: &Plan{Kind: FixCancelDeadHeld, Description: "c", Targets: []string{"job_d"}, Revert: "r"}})
	open := map[string]bool{"pr:c": true}
	prior := func(ctx context.Context, fp string) (*Record, TaskState, error) {
		if open[fp] {
			return &Record{Fingerprint: fp, TaskID: "task_open"}, TaskState{Exists: true, Open: true}, nil
		}
		return nil, TaskState{}, nil
	}

	run := func(budget float64, autofix bool) map[string]int {
		cfg.BudgetUSD, cfg.AutoFix = budget, autofix
		m := NewMeter(cfg.BudgetUSD, []RunJob{{ID: "audit", CostUSD: 0.5, Terminal: true}})
		rc := &RunCap{Max: cfg.MaxAutoFixes}
		acts, err := DecideRun(ctx, fs, RunInputs{Config: cfg, Plan: PlanContext{MaxLines: 150, DeadHeld: map[string]bool{"job_d": true}}, Meter: &m, Cap: rc, Prior: prior})
		if err != nil {
			t.Fatal(err)
		}
		n := map[string]int{}
		for _, a := range acts {
			n[a.Mode]++
			if a.Mode == ModeProposal && a.Finding.AutoFix != nil && a.Why == "" {
				t.Errorf("%s downgraded without a reason", a.Finding.Fingerprint)
			}
		}
		if m.SpentUSD > cfg.BudgetUSD {
			t.Errorf("meter overshot: %v > %v", m.SpentUSD, cfg.BudgetUSD)
		}
		return n
	}
	// $10: 0.5 + 6 x 1.5 = 9.5 would fit, but the cap stops at 5 auto-fixes.
	if n := run(10, true); n[ModeAutoFix] != 5 || n[ModeSkip] != 1 || n[ModeProposal] != 5 {
		t.Fatalf("budget 10: %v", n)
	}
	// $4: 0.5 + 2 x 1.5 = 3.5; a third would cross; the zero-cost held cancel still fits.
	if n := run(4, true); n[ModeAutoFix] != 3 {
		t.Fatalf("budget 4: %v", n)
	}
	// Auto-fix disabled: everything filed is a proposal.
	if n := run(10, false); n[ModeAutoFix] != 0 || n[ModeProposal] != 10 {
		t.Fatalf("autofix off: %v", n)
	}
}
