package auditor

import (
	"errors"
	"strings"
	"testing"
)

func TestDeniedArea(t *testing.T) {
	cases := []struct {
		path string
		deny bool
	}{
		{"internal/mcpfactory/tools.go", false},
		{"docs/foo.md", false},
		{"internal/jobs/retries.go", false},
		{"internal/budgets/config.go", true},
		{"internal/jobs/spend.go", true},
		{"internal/throttle/gate.go", true},
		{"internal/usagewin/x.go", true},
		{"internal/factoryapp/throttle.go", true},
		{"internal/secrets/store.go", true},
		{"internal/factoryapp/secrets_http.go", true},
		{"internal/auth/auth.go", true},
		{"docs/merging.md", true},
		{"internal/foremanapp/updater.go", true},
		{"Makefile", true},
		{".github/workflows/ci.yml", true},
		{"go.mod", true},
		{"internal/auditor/guardrails.go", true},
		{"internal/auditor/new_file.go", true},
		{"internal/factoryapp/auditor.go", true},
		{"cmd/factory/audit.go", true},
		{"internal/jobs/schedules.go", true},
		{"docs/spec.md", true},
		{"./internal/auditor/../auditor/config.go", true},
		{"internal/x/../../internal/auth/http.go", true},
		{"INTERNAL/Auth/auth.go", true},
		{"/etc/systemd/system/factory.service", true},
		{"../outside.go", true},
		{"", true},
		{"internal/authors.go", false}, // prefix match is per path segment
	}
	for _, c := range cases {
		got := DeniedArea(c.path) != ""
		if got != c.deny {
			t.Errorf("DeniedArea(%q) = %q, want denied=%v", c.path, DeniedArea(c.path), c.deny)
		}
	}
}

func TestCheckDiff(t *testing.T) {
	ok := []FileStat{{"internal/mcpfactory/tools.go", 8, 2}, {"docs/x.md", 40, 0}}
	cases := []struct {
		name  string
		files []FileStat
		max   int
		want  string // "" = pass, else substring of the refusal
	}{
		{"small allowed diff", ok, 150, ""},
		{"exactly at cap", []FileStat{{"a.go", 100, 50}}, 150, ""},
		{"one over cap", []FileStat{{"a.go", 100, 51}}, 150, "151 lines"},
		{"config cap lower", ok, 20, "cap is 20"},
		{"config cap cannot exceed hard cap", []FileStat{{"a.go", 200, 0}}, 1000, "cap is 150"},
		{"zero cap means hard cap", []FileStat{{"a.go", 151, 0}}, 0, "cap is 150"},
		{"denied path", []FileStat{{"internal/budgets/budgets.go", 1, 0}}, 150, "forbidden area"},
		{"own guardrails", []FileStat{{"internal/auditor/config.go", 1, 1}}, 150, "auditor rules"},
		{"rename into denied side", ParseNumstat("1\t1\tinternal/{x => auth}/a.go\n"), 150, "forbidden area"},
		{"binary", []FileStat{{"img.png", -1, -1}}, 150, "binary"},
		{"empty", nil, 150, "empty diff"},
	}
	for _, c := range cases {
		err := CheckDiff(c.files, c.max)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected refusal %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) || !errors.Is(err, ErrGuardrail) {
			t.Errorf("%s: got %v, want refusal containing %q", c.name, err, c.want)
		}
	}
}

func TestParseNumstat(t *testing.T) {
	got := ParseNumstat("3\t1\ta.go\n-\t-\tbin.png\n2\t2\tdocs/{old.md => new.md}\n5\t0\tx.go => y.go\n")
	want := []FileStat{
		{"a.go", 3, 1},
		{"bin.png", -1, -1},
		{"docs/old.md", 2, 2},
		{"docs/new.md", 0, 0},
		{"x.go", 5, 0},
		{"y.go", 0, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCheckPlan(t *testing.T) {
	pc := PlanContext{
		MaxLines: 150, WorktreeRoot: "/home/price/work",
		StaleWorktrees: map[string]bool{"/home/price/work/done": true},
		DeadHeld:       map[string]bool{"job_dead": true},
	}
	pr := func(mut func(*Plan)) Plan {
		p := Plan{Kind: FixPR, Description: "tune the retry count for validation errors", Paths: []string{"internal/jobs/retries.go"}, EstLines: 10, Revert: "git revert"}
		if mut != nil {
			mut(&p)
		}
		return p
	}
	cases := []struct {
		name string
		plan Plan
		ok   bool
	}{
		{"small pr", pr(nil), true},
		{"no revert", pr(func(p *Plan) { p.Revert = " " }), false},
		{"unknown kind", pr(func(p *Plan) { p.Kind = "push_master" }), false},
		{"too many lines", pr(func(p *Plan) { p.EstLines = 151 }), false},
		{"no estimate", pr(func(p *Plan) { p.EstLines = 0 }), false},
		{"no paths", pr(func(p *Plan) { p.Paths = nil }), false},
		{"denied path", pr(func(p *Plan) { p.Paths = []string{"internal/throttle/gate.go"} }), false},
		{"denied keyword budget", pr(func(p *Plan) { p.Description = "raise the daily spend cap" }), false},
		{"denied keyword deploy", pr(func(p *Plan) { p.Description = "change the deploy step" }), false},
		{"denied keyword auditor", pr(func(p *Plan) { p.Description = "relax the auditor guardrail" }), false},
		{"keyword is word-bounded", pr(func(p *Plan) { p.Description = "credit the author in docs" }), true},
		{"cleanup stale worktree", Plan{Kind: FixCleanupWorktrees, Description: "rm", Targets: []string{"/home/price/work/done/"}, Revert: "git worktree add"}, true},
		{"cleanup unknown worktree", Plan{Kind: FixCleanupWorktrees, Description: "rm", Targets: []string{"/home/price/work/live"}, Revert: "x"}, false},
		{"cleanup outside root", Plan{Kind: FixCleanupWorktrees, Description: "rm", Targets: []string{"/home/price/temp/factory"}, Revert: "x"}, false},
		{"cleanup traversal", Plan{Kind: FixCleanupWorktrees, Description: "rm", Targets: []string{"/home/price/work/../temp/factory"}, Revert: "x"}, false},
		{"cancel dead held", Plan{Kind: FixCancelDeadHeld, Description: "cancel", Targets: []string{"job_dead"}, Revert: "recreate"}, true},
		{"cancel live job", Plan{Kind: FixCancelDeadHeld, Description: "cancel", Targets: []string{"job_dead", "job_operator"}, Revert: "recreate"}, false},
	}
	for _, c := range cases {
		err := CheckPlan(c.plan, pc)
		if (err == nil) != c.ok {
			t.Errorf("%s: CheckPlan = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestRunCap(t *testing.T) {
	for _, c := range []struct{ max, want int }{{5, 5}, {2, 2}, {0, 0}, {50, HardMaxAutoFixes}, {-1, HardMaxAutoFixes}} {
		rc := RunCap{Max: c.max}
		n := 0
		for i := 0; i < 20; i++ {
			if rc.Take() == nil {
				n++
			}
		}
		if n != c.want {
			t.Errorf("RunCap{Max:%d} admitted %d, want %d", c.max, n, c.want)
		}
	}
}

func TestMeter(t *testing.T) {
	jobs := []RunJob{
		{ID: "audit", CostUSD: 0.4, Terminal: true},
		{ID: "fix1", CostUSD: 0.2, Terminal: false, ReserveUSD: 1.5}, // counts the reservation
		{ID: "fix2", CostUSD: 3.0, Terminal: false, ReserveUSD: 1.5}, // overran its reservation: real cost
		{ID: "fix3", CostUSD: 1.0, Terminal: true, ReserveUSD: 1.5},  // finished: real cost
	}
	m := NewMeter(10, jobs)
	if got, want := m.SpentUSD, 0.4+1.5+3.0+1.0; got != want {
		t.Fatalf("spent = %v, want %v", got, want)
	}
	if err := m.Reserve(1.5); err != nil { // 5.9 + 1.5 = 7.4
		t.Fatalf("reserve: %v", err)
	}
	if err := m.Reserve(1.5); err != nil { // 8.9
		t.Fatalf("reserve: %v", err)
	}
	if err := m.Reserve(1.5); err == nil { // 10.4 > 10
		t.Fatal("reservation over budget admitted")
	}
	if err := m.Reserve(1.0); err != nil { // 9.9: still fits
		t.Fatalf("reserve 1.0: %v", err)
	}
	if err := m.Reserve(0); err != nil { // 9.9 < 10: zero-cost work still allowed
		t.Fatalf("zero reserve below budget: %v", err)
	}
	m.SpentUSD = 10
	if err := m.Reserve(0); err == nil {
		t.Fatal("budget reached: even zero-cost work must not start")
	}
	if m.Remaining() != 0 {
		t.Fatalf("remaining = %v", m.Remaining())
	}
}

func TestLoadConfigClampsGuardrails(t *testing.T) {
	env := map[string]string{
		EnvMaxAutoFixes: "99", EnvMaxLines: "10000", EnvBudgetUSD: "25", EnvCron: "30 7 * * 1-5", EnvTZ: "UTC",
	}
	c, warns := LoadConfig(func(k string) string { return env[k] })
	if c.MaxAutoFixes != HardMaxAutoFixes || c.MaxChangedLines != HardMaxChangedLines {
		t.Fatalf("guardrails not clamped: %+v", c)
	}
	if c.BudgetUSD != 25 || c.CronSpec() != "CRON_TZ=UTC 30 7 * * 1-5" {
		t.Fatalf("budget/cron not applied: %v %q", c.BudgetUSD, c.CronSpec())
	}
	if len(warns) != 2 {
		t.Fatalf("warnings = %v", warns)
	}
	if c.JobPriority < 1 || c.JobPriority > MaxJobPriority || c.TaskPriority < 1 {
		t.Fatalf("priorities: %+v", c)
	}
	bad, warns := LoadConfig(func(k string) string {
		return map[string]string{EnvCron: "not a cron", EnvBudgetUSD: "-3"}[k]
	})
	if bad.Cron != DefaultConfig().Cron || bad.BudgetUSD != 10 || len(warns) != 2 {
		t.Fatalf("bad env not rejected: %+v %v", bad, warns)
	}
}
