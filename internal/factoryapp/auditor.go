package factoryapp

// Factory auditor wiring (spec §6.12): the daily schedule, the fire-time
// snapshot, and the side effects of a finished audit run - proposal
// tasks, auto-fix jobs, merge requests for fix branches, and turning a
// failed auto-fix merge back into a proposal. Every decision (dedup,
// guardrails, budget meter, per-run cap) is made by internal/auditor; this
// file only acts on it. Both are on the auto-fix denylist
// (auditor.Denylist).
//
// The factory service does NO git work: the worktree scan runs inside the
// audit job (its result carries the scan), cleanups and fix branches run
// in fix jobs, and a fix branch's real diff is checked by CI's required
// audit-guardrail check (.github/workflows/audit-guardrail.yml: master's workflow
// and master's cmd/auditcheck) - not here. The merge procedure
// (docs/merging.md) merges only when every check is green.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/pricec/factory/internal/auditor"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// auditFrom is the sender of the auditor's task notes.
const auditFrom = "factory:auditor"

// auditRuntime is the auditor's state on the App.
type auditRuntime struct {
	cfg   auditor.Config
	store *auditor.FindingStore
	// mu serializes run/fix processing; wg tracks it (tests wait on it).
	mu sync.Mutex
	wg sync.WaitGroup
}

// initAuditor wires the auditor: config (cfg.Audit; nil = disabled -
// cmd/factory passes auditor.LoadConfig of the FACTORY_AUDIT_* env), the
// fingerprint store in jobs.db, and the scheduler's fire-time hook.
func (a *App) initAuditor(cfg Config) {
	ac := auditor.DefaultConfig()
	ac.Enabled = false
	if cfg.Audit != nil {
		ac = *cfg.Audit
	}
	a.audit = &auditRuntime{cfg: ac, store: &auditor.FindingStore{DB: a.jobs.DB(), Now: a.jobs.Now}}
	a.scheduler.Prepare = a.prepareScheduled
}

// EnsureAuditSchedule makes the schedules table hold exactly one audit
// schedule matching the config (none when disabled). The factory owns it:
// a changed FACTORY_AUDIT_CRON or budget takes effect on the next start.
func (a *App) EnsureAuditSchedule(ctx context.Context) error {
	cfg := a.audit.cfg
	want := auditor.AuditTemplate(cfg)
	if err := wire.ValidateTemplate(&want); err != nil {
		return fmt.Errorf("audit template: %w", err)
	}
	all, err := a.jobs.Schedules(ctx)
	if err != nil {
		return err
	}
	kept := false
	for _, sch := range all {
		if !auditor.IsAuditTemplate(sch.JobTemplate) {
			continue
		}
		if cfg.Enabled && !kept && sch.Enabled && sch.CronSpec == cfg.CronSpec() && sameTemplate(sch.JobTemplate, want) {
			kept = true
			continue
		}
		if err := a.jobs.DeleteSchedule(ctx, sch.ID); err != nil {
			return err
		}
		log.Printf("factoryapp: auditor: removed schedule %s (%s)", sch.ID, sch.CronSpec)
	}
	if !cfg.Enabled || kept {
		return nil
	}
	sch, err := a.jobs.CreateScheduleNotify(ctx, cfg.CronSpec(), want, "", false)
	if err != nil {
		return err
	}
	log.Printf("factoryapp: auditor: schedule %s %q (priority %d, tier %s, budget $%.2f/run)",
		sch.ID, sch.CronSpec, want.Priority, want.Tier, cfg.BudgetUSD)
	return nil
}

func sameTemplate(a, b wire.JobSpec) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// auditSnapshot collects the current snapshot from the databases. It has
// no worktrees: the scan arrives with the audit job's result.
func (a *App) auditSnapshot(ctx context.Context) (auditor.Snapshot, error) {
	snap, err := auditor.Collect(ctx, a.jobs.DB(), a.inbox.DB(), a.jobs.Now())
	snap.WorktreeErr = "scanned by the audit job"
	return snap, err
}

func (a *App) detectOptions() auditor.DetectOptions {
	c := a.audit.cfg
	return auditor.DetectOptions{WorktreeRoot: c.WorktreeRoot, StaleAfter: c.StaleAfter, MaxLines: c.MaxChangedLines}
}

// prepareScheduled is the scheduler's fire-time hook: an audit firing
// gets the snapshot's deterministic findings as its payload.
func (a *App) prepareScheduled(ctx context.Context, sch jobs.Schedule, spec *wire.JobSpec) error {
	if !auditor.IsAuditTemplate(sch.JobTemplate) {
		return nil
	}
	snap, err := a.auditSnapshot(ctx)
	if err != nil {
		return err
	}
	return auditor.FillAuditSpec(spec, a.audit.cfg, snap.Now, auditor.Detect(snap, a.detectOptions()))
}

// auditTerminal is the onTerminal branch for auditor jobs: processing runs
// off the transition path, serialized.
func (a *App) auditTerminal(ctx context.Context, jobID, state string) {
	if a.audit == nil {
		return
	}
	job, ok, err := a.jobs.Get(ctx, jobID)
	if err != nil || !ok || job.Spec.CreatedBy != auditor.CreatedBy {
		return
	}
	a.audit.wg.Add(1)
	go func() {
		defer a.audit.wg.Done()
		a.audit.mu.Lock()
		defer a.audit.mu.Unlock()
		ctx := context.Background()
		var p struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(job.Spec.Payload, &p)
		switch p.Kind {
		case auditor.PayloadKindAudit:
			if state == jobs.StateDone || state == jobs.StateTimedOut || state == jobs.StateDead {
				if err := a.processAuditRun(ctx, job); err != nil {
					log.Printf("factoryapp: auditor: run %s: %v", job.ID, err)
				}
			}
		case auditor.PayloadKindFix:
			a.finishFix(ctx, job, state)
		}
	}()
}

// WaitAudit blocks until in-flight audit processing finishes (tests).
func (a *App) WaitAudit() {
	if a.audit != nil {
		a.audit.wg.Wait()
	}
}

// lastResult returns the result object of a job's newest JobResult.
func lastResult(job jobs.Job) json.RawMessage {
	var rs []wire.JobResult
	if err := json.Unmarshal(job.Results, &rs); err != nil || len(rs) == 0 {
		return nil
	}
	return rs[len(rs)-1].Result
}

// processAuditRun turns a finished audit job into tasks and fixes: the
// fire-time findings, the worktree findings derived from the job's
// reported scan, and the sweep's prose go through dedup, then the
// guardrails (against a FRESH database snapshot plus the job's validated
// scan, so cleanup targets are exactly what the scan classified as merged
// and clean under the root), the per-run cap and the run's budget meter.
func (a *App) processAuditRun(ctx context.Context, job jobs.Job) error {
	cfg := a.audit.cfg
	p, err := auditor.ParseAuditPayload(job.Spec.Payload)
	if err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	fresh, err := auditor.Collect(ctx, a.jobs.DB(), a.inbox.DB(), a.jobs.Now())
	if err != nil {
		return fmt.Errorf("fresh snapshot: %w", err)
	}
	var sweep auditor.SweepResult
	if job.State == jobs.StateDone {
		sweep, err = auditor.ParseSweep(lastResult(job))
		if err != nil {
			log.Printf("factoryapp: auditor: run %s: %v", job.ID, err)
		}
		if rej := auditor.ApplySweepScan(&fresh, sweep, cfg.WorktreeRoot); len(rej) > 0 {
			log.Printf("factoryapp: auditor: run %s: rejected %d reported worktrees: %s", job.ID, len(rej), strings.Join(rej, "; "))
		}
	} else {
		fresh.WorktreeErr = "audit job ended " + job.State
	}
	if fresh.WorktreeErr != "" {
		log.Printf("factoryapp: auditor: run %s: no worktree scan (%s); no worktree findings or cleanups", job.ID, fresh.WorktreeErr)
	}
	var findings []auditor.Finding
	for _, f := range p.Findings {
		if !auditor.IsWorktreeFinding(f.Fingerprint) {
			findings = append(findings, f)
		}
	}
	findings = append(findings, auditor.DetectWorktrees(fresh, a.detectOptions())...)
	auditor.SortFindings(findings)
	if job.State == jobs.StateDone {
		if merged, err := auditor.MergeSweep(findings, lastResult(job)); err != nil {
			log.Printf("factoryapp: auditor: run %s: %v (detector findings only)", job.ID, err)
		} else {
			findings = merged
		}
	}
	runJobs, err := auditor.RunJobs(ctx, a.jobs.DB(), job.ID)
	if err != nil {
		return err
	}
	meter := auditor.NewMeter(cfg.BudgetUSD, runJobs)
	runCap := &auditor.RunCap{Max: cfg.MaxAutoFixes}
	actions, err := auditor.DecideRun(ctx, findings, auditor.RunInputs{
		Config: cfg, Plan: auditor.ContextFromSnapshot(fresh, cfg), Meter: &meter, Cap: runCap, Prior: a.auditPrior,
	})
	if err != nil {
		return err
	}
	n, filed, fixed, skipped := 0, 0, 0, 0
	for _, act := range actions {
		switch act.Mode {
		case auditor.ModeSkip:
			skipped++
			if err := a.audit.store.Seen(ctx, act.Finding); err != nil {
				log.Printf("factoryapp: auditor: seen %s: %v", act.Finding.Fingerprint, err)
			}
		case auditor.ModeProposal:
			filed++
			if _, err := a.fileProposal(ctx, act.Finding, act.Why); err != nil {
				log.Printf("factoryapp: auditor: file %s: %v", act.Finding.Fingerprint, err)
			}
		case auditor.ModeAutoFix:
			n++
			fixed++
			if err := a.startAutoFix(ctx, job.ID, n, act); err != nil {
				log.Printf("factoryapp: auditor: auto-fix %s: %v (filed as proposal)", act.Finding.Fingerprint, err)
				fixed--
				filed++
				if _, err := a.fileProposal(ctx, act.Finding, "the auto-fix could not start: "+err.Error()); err != nil {
					log.Printf("factoryapp: auditor: file %s: %v", act.Finding.Fingerprint, err)
				}
			}
		}
	}
	log.Printf("factoryapp: auditor: run %s: %d findings, %d proposals, %d auto-fixes, %d deduped; meter $%.2f of $%.2f",
		job.ID, len(actions), filed, fixed, skipped, meter.SpentUSD, meter.BudgetUSD)
	return nil
}

// auditPrior resolves a fingerprint's record and its task's state.
func (a *App) auditPrior(ctx context.Context, fp string) (*auditor.Record, auditor.TaskState, error) {
	rec, err := a.audit.store.Get(ctx, fp)
	if err != nil || rec == nil || rec.TaskID == "" {
		return rec, auditor.TaskState{}, err
	}
	t, err := a.tasks.GetTask(ctx, rec.TaskID)
	if errors.Is(err, tasks.ErrNotFound) {
		return rec, auditor.TaskState{}, nil
	}
	if err != nil {
		return rec, auditor.TaskState{}, err
	}
	return rec, auditor.TaskState{Exists: true, Open: t.Status == tasks.StatusOpen, CloseReason: t.CloseReason}, nil
}

// postAuditNote posts an operator-visible note into a task. brain.reply
// never wakes the planner; the operator's reply does.
func (a *App) postAuditNote(ctx context.Context, taskID, text string) {
	data, _ := json.Marshal(map[string]any{"text": text})
	if _, err := a.postToTask(ctx, taskID, wire.MessageEnvelope{
		Type: "brain.reply", From: auditFrom, TS: time.Now().UTC().Format(time.RFC3339), Payload: data,
	}); err != nil {
		log.Printf("factoryapp: auditor: note into %s: %v", taskID, err)
	}
}

// fileProposal opens a "[proposal]" task (needs_operator, never P0).
func (a *App) fileProposal(ctx context.Context, f auditor.Finding, why string) (string, error) {
	t, err := a.tasks.CreateTask(ctx, auditor.ProposalTitle(f), a.audit.cfg.TaskPriority)
	if err != nil {
		return "", err
	}
	needs := true
	if _, err := a.tasks.UpdateTask(ctx, t.ID, tasks.TaskUpdate{NeedsOperator: &needs}); err != nil {
		log.Printf("factoryapp: auditor: flag %s: %v", t.ID, err)
	}
	a.postAuditNote(ctx, t.ID, auditor.ProposalBody(f, why))
	return t.ID, a.audit.store.Filed(ctx, f, t.ID, auditor.ModeProposal)
}

// startAutoFix opens the informational "[auto-fix]" task and starts the
// fix: held-job cancels run here, everything else as a pinned fix job
// through the normal admission (throttle) path.
func (a *App) startAutoFix(ctx context.Context, run string, n int, act auditor.Action) error {
	cfg := a.audit.cfg
	f := act.Finding
	t, err := a.tasks.CreateTask(ctx, auditor.AutoFixTitle(f), cfg.TaskPriority)
	if err != nil {
		return err
	}
	if f.AutoFix.Kind == auditor.FixCancelDeadHeld {
		var done, skipped []string
		for _, id := range f.AutoFix.Targets {
			undo := a.markSelfCancel(id)
			if _, err := a.jobs.Cancel(ctx, id, jobs.StateCancelled, "auditor: held job of a closed task (audit run "+run+")"); err != nil {
				undo()
				skipped = append(skipped, id+" ("+err.Error()+")")
				continue
			}
			done = append(done, id)
		}
		body := auditor.AutoFixBody(f, "", run) + fmt.Sprintf("\n\nCancelled: %s", orNone(done))
		if len(skipped) > 0 {
			body += fmt.Sprintf("\nSkipped: %s", strings.Join(skipped, "; "))
		}
		a.postAuditNote(ctx, t.ID, body)
		return a.audit.store.Filed(ctx, f, t.ID, auditor.ModeAutoFix)
	}
	spec, err := auditor.FixJobSpec(cfg, run, n, f, t.ID, act.Reserve)
	if err == nil {
		err = a.createPinnedJob(ctx, spec, "")
	}
	if err != nil {
		closed, reason := tasks.StatusClosed, "auditor: auto-fix could not start"
		_, _ = a.tasks.UpdateTask(ctx, t.ID, tasks.TaskUpdate{Status: &closed, CloseReason: &reason})
		return err
	}
	a.postAuditNote(ctx, t.ID, auditor.AutoFixBody(f, spec.ID, run))
	return a.audit.store.Filed(ctx, f, t.ID, auditor.ModeAutoFix)
}

func orNone(ss []string) string {
	if len(ss) == 0 {
		return "none"
	}
	return strings.Join(ss, ", ")
}

// finishFix handles a fix job's terminal state. A PR fix that reports its
// assigned branch enters the merge queue (requested_by auditor). The
// factory does not diff the branch (it runs no git): CI's required
// audit-guardrail check runs auditor.CheckDiff from master's code and the
// merge procedure merges only on all-green checks, so a failing branch
// never merges; that
// failure comes back through auditMergeOutcome and turns the auto-fix
// task into a proposal. A job that did not finish done, or reports a
// different branch, becomes a proposal at once.
func (a *App) finishFix(ctx context.Context, job jobs.Job, state string) {
	var p auditor.FixPayload
	if err := json.Unmarshal(job.Spec.Payload, &p); err != nil || p.TaskID == "" {
		log.Printf("factoryapp: auditor: fix job %s: bad payload: %v", job.ID, err)
		return
	}
	if state != jobs.StateDone {
		a.fixToProposal(ctx, p, fmt.Sprintf("fix job %s ended %s: %s", job.ID, state, orDefault(job.LastError, "no error recorded")))
		return
	}
	if p.Plan.Kind != auditor.FixPR {
		a.postAuditNote(ctx, p.TaskID, fmt.Sprintf("Cleanup job %s finished: %s\n\n**How to revert:** %s",
			job.ID, lastJobSummary(job.Results), p.Plan.Revert))
		return
	}
	var r auditor.PRFixResult
	if err := json.Unmarshal(lastResult(job), &r); err != nil {
		a.fixToProposal(ctx, p, "fix job "+job.ID+" returned no readable result: "+err.Error())
		return
	}
	if r.Branch != p.Branch {
		a.fixToProposal(ctx, p, fmt.Sprintf("fix job %s pushed %q, not the assigned branch %q", job.ID, r.Branch, p.Branch))
		return
	}
	mr, err := a.jobs.CreateMergeRequest(ctx, jobs.NewMergeRequest{
		Repo: a.audit.cfg.Repo, Branch: r.Branch, Head: r.Head, RequestedBy: auditor.RequestedByAuditor,
		Note: clipString("auto-fix: "+strings.TrimPrefix(taskTitleOr(ctx, a, p.TaskID), auditor.AutoFixPrefix), 200),
	})
	if err != nil {
		a.fixToProposal(ctx, p, "could not queue the merge: "+err.Error())
		return
	}
	a.kickMergeQueue()
	a.postAuditNote(ctx, p.TaskID, fmt.Sprintf(
		"Fix job %s pushed `%s` @ %s (reported files: %s). Queued as merge request %s; the merge job opens the PR and merges only after CI is green, including the required `audit-guardrail` check, which runs the auditor's diff guardrail from master's code (at most %d changed lines, no binaries, no forbidden areas). If it or any other check fails, this task becomes a proposal.\n\n**What changed:** %s\n\n**How to revert:** before it lands, `POST /v1/merge-requests/%s/cancel`. After it lands: %s",
		job.ID, r.Branch, shortHead(r.Head), orNone(r.Files), mr.ID, p.MaxLines, r.Change, mr.ID, p.Plan.Revert))
}

// auditMergeOutcome is the merge-queue hook for auditor merge requests
// (requested_by auditor, branch auto/audit-*), called whenever a request
// reaches a terminal state or needs_attention (finishMerge,
// mergeNeedsAttention). needs_attention - a red CI check such as
// audit-guardrail, a conflict or a failed deploy - turns the auto-fix task into a proposal with the
// reason; landed and cancelled are noted on the task.
func (a *App) auditMergeOutcome(ctx context.Context, mr jobs.MergeRequest) {
	if a.audit == nil || !auditor.IsAuditMerge(mr.RequestedBy, mr.Branch) {
		return
	}
	a.audit.wg.Add(1)
	go func() {
		defer a.audit.wg.Done()
		a.audit.mu.Lock()
		defer a.audit.mu.Unlock()
		ctx := context.WithoutCancel(ctx)
		p, fixJob, ok, err := auditor.FixForBranch(ctx, a.jobs.DB(), mr.Branch)
		if err != nil || !ok {
			log.Printf("factoryapp: auditor: merge %s (%s): no fix job found (%v)", mr.ID, mr.Branch, err)
			return
		}
		switch mr.State {
		case jobs.MergeNeedsAttention:
			a.fixToProposal(ctx, p, auditor.MergeFailureReason(mr.ID, mr.Result))
		case jobs.MergeLanded:
			a.postAuditNote(ctx, p.TaskID, fmt.Sprintf("Merge request %s for `%s` (fix job %s) landed: %s\n\n**How to revert:** %s",
				mr.ID, mr.Branch, fixJob, orDefault(mr.Result, "merged"), p.Plan.Revert))
		case jobs.MergeCancelled:
			a.postAuditNote(ctx, p.TaskID, fmt.Sprintf("Merge request %s for `%s` was cancelled: %s",
				mr.ID, mr.Branch, orDefault(mr.Result, "no reason recorded")))
		}
	}()
}

// fixToProposal converts an auto-fix task into a proposal: retitled,
// flagged needs_operator, the reason posted, the fingerprint rebound.
func (a *App) fixToProposal(ctx context.Context, p auditor.FixPayload, why string) {
	title := auditor.ProposalPrefix + strings.TrimPrefix(taskTitleOr(ctx, a, p.TaskID), auditor.AutoFixPrefix)
	needs, open := true, tasks.StatusOpen
	if _, err := a.tasks.UpdateTask(ctx, p.TaskID, tasks.TaskUpdate{Title: &title, NeedsOperator: &needs, Status: &open}); err != nil {
		log.Printf("factoryapp: auditor: convert %s: %v", p.TaskID, err)
	}
	a.postAuditNote(ctx, p.TaskID, fmt.Sprintf(
		"**The auto-fix did not go through, so this is now a proposal.** Reason: %s\n\nPlanned change (%s): %s\n\nReply to approve or edit it; close the task to reject it.",
		why, p.Plan.Kind, p.Plan.Description))
	if err := a.audit.store.Rebind(ctx, p.Fingerprint, p.TaskID, auditor.ModeProposal); err != nil {
		log.Printf("factoryapp: auditor: rebind %s: %v", p.Fingerprint, err)
	}
}

func taskTitleOr(ctx context.Context, a *App, id string) string {
	if t, err := a.tasks.GetTask(ctx, id); err == nil {
		return t.Title
	}
	return id
}

func shortHead(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func clipString(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
