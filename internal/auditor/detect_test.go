package auditor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

func TestNormalizeError(t *testing.T) {
	a := NormalizeError("job exceeded timeout_s=300; process group killed\nmore")
	b := NormalizeError("job exceeded timeout_s=1200; process group killed")
	if a != b || strings.Contains(a, "more") {
		t.Fatalf("%q vs %q", a, b)
	}
	if NormalizeError("job_01M7N1DR3ZT3ZS1QVTZKKECH1P failed at deadbeef1") != "job_<id> failed at <sha>" {
		t.Fatal(NormalizeError("job_01M7N1DR3ZT3ZS1QVTZKKECH1P failed at deadbeef1"))
	}
}

func TestDetect(t *testing.T) {
	now := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)
	ago := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	snap := Snapshot{
		Now: now,
		Jobs: []JobRow{
			{ID: "job_t1", State: "timed_out", Tier: wire.TierDefault, TimeoutS: 300, CreatedAt: ago(3), UpdatedAt: ago(2), Errors: []string{"job exceeded timeout_s=300; process group killed"}},
			{ID: "job_t2", State: "timed_out", Tier: wire.TierHardest, TimeoutS: 300, CreatedAt: ago(5), UpdatedAt: ago(4), Errors: []string{"job exceeded timeout_s=300; process group killed"}},
			{ID: "job_h1", State: "held", CreatedBy: "factory", TaskID: "task_c", TaskOpen: false, CreatedAt: ago(30), HeldReason: "usage RED"},
			{ID: "job_h2", State: "held", CreatedBy: "ui", TaskID: "task_c", TaskOpen: false, CreatedAt: ago(30), HeldReason: "usage RED"},
			{ID: "job_h3", State: "held", CreatedBy: "factory", TaskID: "task_o", TaskOpen: true, CreatedAt: ago(30), HeldReason: "usage RED"},
			{ID: "job_e1", State: "dead", ForemanID: "foreman:a", CreatedAt: ago(40), UpdatedAt: ago(40), Errors: []string{`exec: "opencode": executable file not found in $PATH`}},
			{ID: "job_e2", State: "dead", ForemanID: "foreman:a", CreatedAt: ago(10), UpdatedAt: ago(10), Errors: []string{`exec: "opencode": executable file not found in $PATH`}},
			{ID: "job_r1", State: "failed", CreatedAt: ago(1), UpdatedAt: ago(1), Errors: []string{"report_result: error: required when status is failed"}},
			{ID: "job_r2", State: "failed", CreatedAt: ago(1), UpdatedAt: ago(1), Errors: []string{"report_result: error: required when status is failed"}},
		},
		Merges: []MergeRow{
			{ID: "mr_1", Branch: "feat/a", Head: "abcdef1", State: "landed", Result: "job x done: I couldn't merge this request: PR #30 conflicts with master", JobID: "job_m1"},
			{ID: "mr_2", Branch: "feat/b", Head: "abcdef2", State: "landed", Result: "landed: PR #31 merged", JobID: "job_m2"},
		},
		Foremen: []ForemanRow{
			{ID: "foreman:a", Online: true, Build: "b2"},
			{ID: "foreman:b", Online: true, Build: "b2"},
			{ID: "foreman:c", Online: true},
			{ID: "foreman:d", Online: false, LastSeen: ago(3)},
		},
		Worktrees: []Worktree{
			{Path: "/w/merged", Branch: "x", Head: "1111111", Merged: true, Clean: true},
			{Path: "/w/dirty", Branch: "y", Head: "2222222", Merged: true, Clean: false},
			{Path: "/w/old", Branch: "z", Head: "3333333", LastCommit: ago(24 * 10)},
		},
	}
	fs := Detect(snap, DetectOptions{WorktreeRoot: "/w"})
	by := map[string]Finding{}
	for _, f := range fs {
		if _, dup := by[f.Fingerprint]; dup {
			t.Fatalf("duplicate fingerprint %s", f.Fingerprint)
		}
		by[f.Fingerprint] = f
		if f.Title == "" || f.Cause == "" || f.Fix == "" || f.Benefit == "" || f.Effort == "" || f.MetricLabel == "" {
			t.Errorf("%s: incomplete finding %+v", f.Fingerprint, f)
		}
	}
	want := map[string]float64{
		"timeout:below_tier_ceiling": 2,
		"held:dead":                  1, // only the factory-created job of a closed task
		"merge:false_landed":         1,
		"foreman:offline":            1,
		"foreman:build_drift":        1,
		"worktree:merged_clean":      1,
		"worktree:stale_unmerged":    1,
	}
	for fp, m := range want {
		f, ok := by[fp]
		if !ok {
			t.Errorf("missing %s (have %v)", fp, keys(by))
			continue
		}
		if f.Metric != m {
			t.Errorf("%s metric = %v, want %v", fp, f.Metric, m)
		}
	}
	if f := by["held:dead"]; f.AutoFix == nil || len(f.AutoFix.Targets) != 1 || f.AutoFix.Targets[0] != "job_h1" {
		t.Errorf("dead held plan: %+v", f.AutoFix)
	}
	if by["merge:false_landed"].Known == "" || by["merge:false_landed"].AutoFix != nil {
		t.Error("false_landed must be marked as the known conflict issue and never auto-fixed")
	}
	// The timeout error cluster is covered by the timeout finding.
	for fp := range by {
		if strings.HasPrefix(fp, "error:") && strings.Contains(by[fp].Title, "timeout_s") {
			t.Errorf("timeout error cluster not folded: %s", fp)
		}
	}
	// report_result errors: reported, flagged known, never auto-fixed.
	var sawRR, sawExec bool
	for _, f := range fs {
		if strings.Contains(f.Title, "report_result") {
			sawRR = f.Known != "" && f.AutoFix == nil
		}
		if strings.Contains(f.Title, "executable file not found") {
			sawExec = strings.Contains(f.Fix, "matcher")
		}
	}
	if !sawRR || !sawExec {
		t.Errorf("recurring errors: report_result known=%v exec hint=%v", sawRR, sawExec)
	}
	// Every detector plan passes the guardrails against the snapshot's own context.
	pc := ContextFromSnapshot(snap, Config{MaxChangedLines: 150, WorktreeRoot: "/w"})
	for _, f := range fs {
		if f.AutoFix != nil {
			if err := CheckPlan(*f.AutoFix, pc); err != nil {
				t.Errorf("%s: detector plan refused: %v", f.Fingerprint, err)
			}
		}
	}
}

func keys(m map[string]Finding) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMergeSweep(t *testing.T) {
	det := []Finding{{Fingerprint: "merge:false_landed", Title: "t", Metric: 2, Cause: "old", Evidence: []string{"code"}}}
	res, _ := json.Marshal(map[string]any{"findings": []map[string]any{
		{
			"fingerprint": "merge:false_landed", "title": "x", "cause": "sharper cause", "metric": 99, "evidence": []string{"llm"},
			"autofix": map[string]any{"kind": "pr", "description": "d", "paths": []string{"a.go"}, "est_lines": 1, "revert": "r"},
		},
		{
			"fingerprint": "Stale Branches!", "title": "stale branches", "metric": 4, "evidence": []string{"e"}, "cause": "c", "fix": "f",
			"autofix": map[string]any{"kind": "cleanup_worktrees", "description": "d", "revert": "r"},
		},
	}})
	out, err := MergeSweep(det, res)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d findings", len(out))
	}
	var known, added Finding
	for _, f := range out {
		if f.Fingerprint == "merge:false_landed" {
			known = f
		} else {
			added = f
		}
	}
	if known.Cause != "sharper cause" || known.Metric != 2 || known.Evidence[0] != "code" || known.AutoFix != nil {
		t.Fatalf("sweep may only sharpen prose of a detector finding: %+v", known)
	}
	if added.Fingerprint != "sweep:stale-branches" || added.AutoFix != nil || added.Source != "sweep" {
		t.Fatalf("sweep finding: %+v", added)
	}
	if _, err := MergeSweep(det, json.RawMessage(`not json`)); err == nil {
		t.Fatal("bad sweep result accepted")
	}
}

func TestFillAuditSpecFits(t *testing.T) {
	var fs []Finding
	for i := 0; i < 200; i++ {
		fs = append(fs, Finding{
			Fingerprint: "f" + strings.Repeat("x", i%10), Title: strings.Repeat("t", 150),
			Evidence: []string{strings.Repeat("e", 300), strings.Repeat("e", 300), strings.Repeat("e", 300), strings.Repeat("e", 300), "x"},
			JobIDs:   []string{"a", "b", "c", "d", "e", "f", "g"},
		})
	}
	spec := AuditTemplate(DefaultConfig())
	spec.ID = wire.NewJobID()
	if err := FillAuditSpec(&spec, DefaultConfig(), time.Now(), fs); err != nil {
		t.Fatal(err)
	}
	if len(spec.Payload) > payloadBudget || len(spec.Task)+len(spec.Payload) > spec.Budget.MaxPromptBytes {
		t.Fatalf("payload %d bytes", len(spec.Payload))
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
}
