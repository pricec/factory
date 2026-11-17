// Factory auditor wiring (spec §6.12): the owned daily schedule, the
// fire-time snapshot, proposal filing with dedup, auto-fixes inside the
// code-enforced guardrails, the job-reported worktree scan (the factory
// runs no git), and a failed auto-fix merge (e.g. the CI audit-guardrail
// check) turning back into a proposal.
package factoryapp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/auditor"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time        { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration)   { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func auditCfg() auditor.Config             { c := auditor.DefaultConfig(); c.TZ = "UTC"; c.RepoDir = ""; return c }
func ctxBG() context.Context               { return context.Background() }
func mustJSONT(t *testing.T, v any) []byte { t.Helper(); b, _ := json.Marshal(v); return b }

func newAuditApp(t *testing.T, cfg auditor.Config) (*App, *testClock) {
	t.Helper()
	t.Setenv(EnvMergeForeman, "")
	clk := &testClock{t: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)}
	a, err := New(Config{DataDir: t.TempDir(), Addr: ":8093", AdvertiseURL: "http://factory.test", Now: clk.Now, Audit: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.WaitAudit(); _ = a.Close() })
	if err := a.jobs.UpsertForeman(ctxBG(), "foreman:server", json.RawMessage(`{"build":"b1"}`)); err != nil {
		t.Fatal(err)
	}
	return a, clk
}

func auditSchedules(t *testing.T, a *App) []jobs.Schedule {
	t.Helper()
	all, err := a.jobs.Schedules(ctxBG())
	if err != nil {
		t.Fatal(err)
	}
	var out []jobs.Schedule
	for _, s := range all {
		if auditor.IsAuditTemplate(s.JobTemplate) {
			out = append(out, s)
		}
	}
	return out
}

func TestEnsureAuditSchedule(t *testing.T) {
	a, _ := newAuditApp(t, auditCfg())
	for i := 0; i < 2; i++ { // idempotent
		if err := a.EnsureAuditSchedule(ctxBG()); err != nil {
			t.Fatal(err)
		}
	}
	got := auditSchedules(t, a)
	if len(got) != 1 {
		t.Fatalf("schedules = %d, want 1", len(got))
	}
	s := got[0]
	if s.CronSpec != "CRON_TZ=UTC 0 6 * * *" || s.JobTemplate.Priority != 1 || s.JobTemplate.Tier != wire.TierFlash ||
		s.JobTemplate.Foreman != "foreman:server" || s.JobTemplate.Budget.MaxCostUSD != 10 {
		t.Fatalf("schedule = %+v", s)
	}
	// A config change replaces it; disabling removes it.
	a.audit.cfg.Cron = "30 5 * * *"
	if err := a.EnsureAuditSchedule(ctxBG()); err != nil {
		t.Fatal(err)
	}
	if got := auditSchedules(t, a); len(got) != 1 || got[0].CronSpec != "CRON_TZ=UTC 30 5 * * *" {
		t.Fatalf("after cron change: %+v", got)
	}
	a.audit.cfg.Enabled = false
	if err := a.EnsureAuditSchedule(ctxBG()); err != nil {
		t.Fatal(err)
	}
	if got := auditSchedules(t, a); len(got) != 0 {
		t.Fatalf("disabled: %d schedules left", len(got))
	}
}

// seedFindings creates data for two detector findings: a falsely landed
// merge (proposal) and a low-timeout job (PR auto-fix).
func seedFindings(t *testing.T, a *App) {
	t.Helper()
	ctx := ctxBG()
	mr, err := a.jobs.CreateMergeRequest(ctx, jobs.NewMergeRequest{Branch: "feat/x", Head: mqHead, RequestedBy: "planner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.jobs.DB().ExecContext(ctx, `UPDATE merge_requests SET state='landed', result='job done: I couldn''t merge this request' WHERE id=?`, mr.ID); err != nil {
		t.Fatal(err)
	}
	spec := wire.NewJobSpec("build things", "factory")
	spec.Tier, spec.TimeoutS = wire.TierDefault, 300
	spec.CreatedAt = a.jobs.Now().Format(time.RFC3339)
	if err := a.jobs.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := a.jobs.DB().ExecContext(ctx, `UPDATE jobs SET state='timed_out', last_error='job exceeded timeout_s=300; process group killed' WHERE id=?`, spec.ID); err != nil {
		t.Fatal(err)
	}
}

func fireAudit(t *testing.T, a *App, clk *testClock) jobs.Job {
	t.Helper()
	if err := a.EnsureAuditSchedule(ctxBG()); err != nil {
		t.Fatal(err)
	}
	clk.Add(24 * time.Hour)
	if _, err := a.scheduler.Pump(ctxBG()); err != nil {
		t.Fatal(err)
	}
	var audit []jobs.Job
	all, _ := a.jobs.List(ctxBG(), "", "", 500)
	for _, j := range all {
		if auditor.IsAuditTemplate(j.Spec) {
			audit = append(audit, j)
		}
	}
	if len(audit) != 1 {
		t.Fatalf("audit jobs = %d, want 1", len(audit))
	}
	return audit[0]
}

func listAuditTasks(t *testing.T, a *App) (proposals, fixes []tasks.Task) {
	t.Helper()
	all, err := a.tasks.ListTasks(ctxBG(), tasks.TaskFilter{Status: "all", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range all {
		switch {
		case strings.HasPrefix(tk.Title, auditor.ProposalPrefix):
			proposals = append(proposals, tk)
		case strings.HasPrefix(tk.Title, auditor.AutoFixPrefix):
			fixes = append(fixes, tk)
		}
	}
	return proposals, fixes
}

func auditFixJobs(t *testing.T, a *App) []jobs.Job {
	t.Helper()
	all, _ := a.jobs.List(ctxBG(), "", "", 500)
	var out []jobs.Job
	for _, j := range all {
		if j.Spec.CreatedBy == auditor.CreatedBy && !auditor.IsAuditTemplate(j.Spec) {
			out = append(out, j)
		}
	}
	return out
}

func TestAuditRunEndToEnd(t *testing.T) {
	a, clk := newAuditApp(t, auditCfg())
	seedFindings(t, a)
	job := fireAudit(t, a, clk)

	// Fire-time payload: the deterministic database findings (no worktree
	// findings: the job runs that scan), LOW priority, flash.
	p, err := auditor.ParseAuditPayload(job.Spec.Payload)
	if err != nil || p.AuditRun != job.ID || len(p.Findings) < 2 {
		t.Fatalf("payload: %v %+v", err, p)
	}
	if job.Spec.Priority != 1 || job.Spec.Tier != wire.TierFlash {
		t.Fatalf("audit spec: priority %d tier %s", job.Spec.Priority, job.Spec.Tier)
	}

	// The sweep adds one finding with a plan touching a forbidden area: it
	// must be downgraded to a proposal by code.
	// The job's worktree scan reports one merged clean worktree under the
	// root (a cleanup auto-fix), one dirty one and one outside the root
	// (neither may become a cleanup target).
	sweep := map[string]any{"findings": []map[string]any{{
		"fingerprint": "Raise spend cap", "title": "Daily cap too low", "evidence": []string{"x"},
		"cause": "c", "fix": "f", "metric": 3,
		"autofix": map[string]any{"kind": "pr", "description": "raise it", "paths": []string{"internal/budgets/config.go"}, "est_lines": 2, "revert": "git revert"},
	}}, "worktrees": []map[string]any{
		{"path": "/home/price/work/old", "branch": "feat/old", "head": "abc1234", "merged": true, "clean": true},
		{"path": "/home/price/work/dirty", "branch": "feat/dirty", "head": "abc1235", "merged": true, "clean": false},
		{"path": "/home/price/temp/factory", "branch": "master", "head": "abc1236", "merged": true, "clean": true},
	}}
	if err := a.jobs.Dispatch(ctxBG(), job.ID, "foreman:server"); err != nil && !strings.Contains(err.Error(), "illegal") {
		t.Log(err)
	}
	if err := a.jobs.ApplyEvent(ctxBG(), job.ID, "started"); err != nil {
		t.Fatal(err)
	}
	res := wire.JobResult{JobID: job.ID, Status: wire.StatusDone, Summary: "audit done", Result: mustJSONT(t, sweep)}
	a.handleEnvelope(ctxBG(), wire.MessageEnvelope{Type: "job.result", JobID: job.ID, Payload: mustJSONT(t, res)}, map[string]bool{})
	a.WaitAudit()

	proposals, fixes := listAuditTasks(t, a)
	if len(proposals) == 0 || len(fixes) != 2 {
		t.Fatalf("proposals=%d fixes=%d (%+v)", len(proposals), len(fixes), fixes)
	}
	var sawSweep bool
	for _, tk := range proposals {
		if !tk.NeedsOperator || tk.Priority == 0 {
			t.Errorf("proposal %q: needs_operator=%v priority=%d", tk.Title, tk.NeedsOperator, tk.Priority)
		}
		if strings.Contains(tk.Title, "Daily cap too low") {
			sawSweep = true
		}
	}
	if !sawSweep {
		t.Fatal("sweep finding with a forbidden plan was not filed as a proposal")
	}
	for _, tk := range fixes {
		if tk.NeedsOperator || tk.Priority == 0 {
			t.Errorf("auto-fix task %q: needs_operator=%v priority=%d", tk.Title, tk.NeedsOperator, tk.Priority)
		}
	}
	fixJobs := auditFixJobs(t, a)
	if len(fixJobs) != 2 {
		t.Fatalf("fix jobs = %d, want 2 (PR + worktree cleanup)", len(fixJobs))
	}
	for _, j := range fixJobs {
		if j.Spec.Priority != 1 || j.Spec.Foreman != "foreman:server" || j.Spec.Budget.MaxCostUSD != 1.5 {
			t.Errorf("fix job %s: %+v", j.ID, j.Spec)
		}
		var fp auditor.FixPayload
		_ = json.Unmarshal(j.Spec.Payload, &fp)
		if fp.Plan.Kind != auditor.FixPR {
			if strings.Join(fp.Plan.Targets, ",") != "/home/price/work/old" {
				t.Errorf("cleanup targets = %v, want only the merged clean worktree under the root", fp.Plan.Targets)
			}
			if strings.Contains(j.Spec.Task, "--force") && !strings.Contains(j.Spec.Task, "never --force") {
				t.Errorf("cleanup task may force: %s", j.Spec.Task)
			}
		}
	}

	// Dedup: the same run processed again files nothing new (tasks open).
	nProp, nFix := len(proposals), len(fixes)
	j2, _, _ := a.jobs.Get(ctxBG(), job.ID)
	if err := a.processAuditRun(ctxBG(), j2); err != nil {
		t.Fatal(err)
	}
	if p2, f2 := listAuditTasks(t, a); len(p2) != nProp || len(f2) != nFix {
		t.Fatalf("re-run filed again: %d/%d -> %d/%d", nProp, nFix, len(p2), len(f2))
	}
	// Operator closes (rejects) every proposal: same evidence is not re-filed.
	for _, tk := range proposals {
		st, why := tasks.StatusClosed, "operator"
		if _, err := a.tasks.UpdateTask(ctxBG(), tk.ID, tasks.TaskUpdate{Status: &st, CloseReason: &why}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.processAuditRun(ctxBG(), j2); err != nil {
		t.Fatal(err)
	}
	if p3, _ := listAuditTasks(t, a); len(p3) != nProp {
		t.Fatalf("closed proposals re-filed without worsening: %d -> %d", nProp, len(p3))
	}

	// The PR fix finishes: its real diff passes, so it enters the queue.
	var prJob jobs.Job
	for _, j := range fixJobs {
		var fp auditor.FixPayload
		_ = json.Unmarshal(j.Spec.Payload, &fp)
		if fp.Plan.Kind == auditor.FixPR {
			prJob = j
		}
	}
	if prJob.ID == "" {
		t.Fatal("no PR fix job")
	}
	finishFixJob(t, a, prJob, nil)
	mrs, _ := a.jobs.MergeRequests(ctxBG(), 50)
	var queued *jobs.MergeRequest
	for i := range mrs {
		if mrs[i].RequestedBy == "auditor" {
			queued = &mrs[i]
		}
	}
	if queued == nil || !strings.HasPrefix(queued.Branch, "auto/audit-") {
		t.Fatalf("no auditor merge request: %+v", mrs)
	}
}

// finishFixJob drives a PR fix job to done reporting its assigned branch.
func finishFixJob(t *testing.T, a *App, j jobs.Job, branch *string) {
	t.Helper()
	var fp auditor.FixPayload
	_ = json.Unmarshal(j.Spec.Payload, &fp)
	b := fp.Branch
	if branch != nil {
		b = *branch
	}
	if err := a.jobs.ApplyEvent(ctxBG(), j.ID, "started"); err != nil {
		t.Fatal(err)
	}
	r := auditor.PRFixResult{Branch: b, Head: mqHead, Files: []string{"internal/mcpfactory/tools.go"}, Change: "tool description"}
	res := wire.JobResult{JobID: j.ID, Status: wire.StatusDone, Summary: "pushed", Result: mustJSONT(t, r)}
	a.handleEnvelope(ctxBG(), wire.MessageEnvelope{Type: "job.result", JobID: j.ID, Payload: mustJSONT(t, res)}, map[string]bool{})
	a.WaitAudit()
}

// runToFixJob fires an audit with seeded findings, finishes it with an
// empty sweep, and returns the single PR fix job it started.
func runToFixJob(t *testing.T, a *App, clk *testClock) jobs.Job {
	t.Helper()
	seedFindings(t, a)
	job := fireAudit(t, a, clk)
	if err := a.jobs.ApplyEvent(ctxBG(), job.ID, "started"); err != nil {
		t.Fatal(err)
	}
	res := wire.JobResult{JobID: job.ID, Status: wire.StatusDone, Summary: "audit done", Result: json.RawMessage(`{"findings":[],"worktrees":[]}`)}
	a.handleEnvelope(ctxBG(), wire.MessageEnvelope{Type: "job.result", JobID: job.ID, Payload: mustJSONT(t, res)}, map[string]bool{})
	a.WaitAudit()
	fixJobs := auditFixJobs(t, a)
	if len(fixJobs) != 1 {
		t.Fatalf("fix jobs = %d", len(fixJobs))
	}
	return fixJobs[0]
}

func auditorMerges(t *testing.T, a *App) []jobs.MergeRequest {
	t.Helper()
	mrs, err := a.jobs.MergeRequests(ctxBG(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []jobs.MergeRequest
	for _, m := range mrs {
		if m.RequestedBy == auditor.RequestedByAuditor {
			out = append(out, m)
		}
	}
	return out
}

func assertConvertedToProposal(t *testing.T, a *App, want string) {
	t.Helper()
	proposals, fixes := listAuditTasks(t, a)
	if len(fixes) != 0 {
		t.Fatalf("auto-fix task not converted: %+v", fixes)
	}
	for _, p := range proposals {
		if strings.Contains(p.Title, "timeout_s") && p.NeedsOperator {
			if want == "" {
				return
			}
			msgs, err := a.inbox.ThreadMessages(ctxBG(), p.ID, 50)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range msgs {
				if strings.Contains(string(m.Payload), want) {
					return
				}
			}
			t.Fatalf("converted proposal has no note mentioning %q", want)
		}
	}
	t.Fatalf("no converted proposal: %+v", proposals)
}

// TestFixBranchToMergeQueue: the factory runs no git, so a fix job that
// reports its assigned branch is queued without a local diff (CI's
// required audit-guardrail check judges the diff); a job reporting any
// other branch becomes a proposal at once.
func TestFixBranchToMergeQueue(t *testing.T) {
	for _, c := range []struct {
		name   string
		branch string
		queued bool
	}{{"assigned branch", "", true}, {"wrong branch", "master", false}} {
		t.Run(c.name, func(t *testing.T) {
			a, clk := newAuditApp(t, auditCfg())
			fj := runToFixJob(t, a, clk)
			var br *string
			if c.branch != "" {
				br = &c.branch
			}
			finishFixJob(t, a, fj, br)
			if n := len(auditorMerges(t, a)); (n == 1) != c.queued {
				t.Fatalf("auditor merge requests = %d, want queued=%v", n, c.queued)
			}
			if !c.queued {
				assertConvertedToProposal(t, a, "")
			}
		})
	}
}

// TestFixMergeGuardrailFailure: when the merge of an auto-fix needs
// attention (the CI audit-guardrail check refused the diff), the auto-fix
// task becomes a proposal naming the guardrail; a landed merge does not.
func TestFixMergeGuardrailFailure(t *testing.T) {
	a, clk := newAuditApp(t, auditCfg())
	fj := runToFixJob(t, a, clk)
	finishFixJob(t, a, fj, nil)
	mrs := auditorMerges(t, a)
	if len(mrs) != 1 || !strings.HasPrefix(mrs[0].Branch, auditor.FixBranchPrefix) {
		t.Fatalf("auditor merges = %+v", mrs)
	}
	if _, fixes := listAuditTasks(t, a); len(fixes) != 1 {
		t.Fatalf("auto-fix tasks before the merge outcome = %d, want 1", len(fixes))
	}
	if ok, err := a.jobs.ClaimMerge(ctxBG(), mrs[0].ID, "job_merge"); err != nil || !ok {
		t.Fatalf("claim merge: %v %v", ok, err)
	}
	a.mergeNeedsAttention(ctxBG(), mrs[0].ID, "checks red: audit-guardrail (auditor guardrail: FAIL: 151 changed lines > 150)")
	a.WaitAudit()
	assertConvertedToProposal(t, a, "audit-guardrail")
}
