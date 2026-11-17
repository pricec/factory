package factoryapp

// Factory auditor wiring (spec §6.12): the daily schedule, the fire-time
// snapshot, and the side effects of a finished audit run - proposal
// tasks, auto-fix jobs, guardrail-checked merge requests. Every decision
// (dedup, guardrails, budget meter, per-run cap) is made by
// internal/auditor; this file only acts on it. Both are on the auto-fix
// denylist (auditor.Denylist).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
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
	// diff and scan are seams over git (tests stub them).
	diff func(ctx context.Context, branch, head string) ([]auditor.FileStat, error)
	scan func(ctx context.Context) ([]auditor.Worktree, error)
}

// initAuditor wires the auditor: config (cfg.Audit; nil = disabled -
// cmd/factory passes auditor.LoadConfig of the FACTORY_AUDIT_* env), the
// fingerprint store in jobs.db, the git seams, and the scheduler's
// fire-time hook.
func (a *App) initAuditor(cfg Config) {
	ac := auditor.DefaultConfig()
	ac.Enabled = false
	if cfg.Audit != nil {
		ac = *cfg.Audit
	}
	rt := &auditRuntime{cfg: ac, store: &auditor.FindingStore{DB: a.jobs.DB(), Now: a.jobs.Now}}
	mirror := filepath.Join(cfg.DataDir, "auditor", "mirror.git")
	rt.diff = func(ctx context.Context, branch, head string) ([]auditor.FileStat, error) {
		return auditor.DiffStat(ctx, mirror, ac.RepoURL, branch, head)
	}
	rt.scan = func(ctx context.Context) ([]auditor.Worktree, error) {
		if ac.RepoDir == "" {
			return nil, errors.New("no repo dir configured")
		}
		return auditor.ScanWorktrees(ctx, ac.RepoDir, ac.WorktreeRoot)
	}
	a.audit = rt
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
	sch, err := a.jobs.CreateSchedule(ctx, cfg.CronSpec(), want)
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

// auditSnapshot collects the current snapshot (databases + git scan).
func (a *App) auditSnapshot(ctx context.Context) (auditor.Snapshot, error) {
	snap, err := auditor.Collect(ctx, a.jobs.DB(), a.inbox.DB(), a.jobs.Now())
	if err != nil {
		return snap, err
	}
	if a.audit.scan != nil {
		wts, err := a.audit.scan(ctx)
		if err != nil {
			snap.WorktreeErr = err.Error()
		}
		snap.Worktrees = wts
	}
	return snap, nil
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
// off the transition path (it may run git), serialized.
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
// fire-time findings plus the sweep's result go through dedup, then the
// guardrails (against a FRESH snapshot, so cleanup targets are still
// valid), the per-run cap and the run's budget meter.
func (a *App) processAuditRun(ctx context.Context, job jobs.Job) error {
	cfg := a.audit.cfg
	p, err := auditor.ParseAuditPayload(job.Spec.Payload)
	if err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	findings := p.Findings
	if job.State == jobs.StateDone {
		if findings, err = auditor.MergeSweep(p.Findings, lastResult(job)); err != nil {
			log.Printf("factoryapp: auditor: run %s: %v (detector findings only)", job.ID, err)
		}
	}
	fresh, err := a.auditSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("fresh snapshot: %w", err)
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
		err = a.createPinnedJob(ctx, spec)
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

// finishFix handles a fix job's terminal state. A PR fix's branch enters
// the merge queue ONLY after its real diff passes auditor.CheckDiff (line
// cap + denylist); any failure turns the auto-fix task into a proposal.
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
	files, err := a.audit.diff(ctx, r.Branch, r.Head)
	if err != nil {
		a.fixToProposal(ctx, p, fmt.Sprintf("could not verify %s@%s: %v", r.Branch, r.Head, err))
		return
	}
	lines, _ := auditor.ChangedLines(files)
	if err := auditor.CheckDiff(files, p.MaxLines); err != nil {
		a.fixToProposal(ctx, p, fmt.Sprintf("branch %s@%s failed the guardrails: %v", r.Branch, shortHead(r.Head), err))
		return
	}
	mr, err := a.jobs.CreateMergeRequest(ctx, jobs.NewMergeRequest{
		Repo: a.audit.cfg.Repo, Branch: r.Branch, Head: r.Head, RequestedBy: "auditor",
		Note: clipString("auto-fix: "+strings.TrimPrefix(taskTitleOr(ctx, a, p.TaskID), auditor.AutoFixPrefix), 200),
	})
	if err != nil {
		a.fixToProposal(ctx, p, "could not queue the merge: "+err.Error())
		return
	}
	a.kickMergeQueue()
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	a.postAuditNote(ctx, p.TaskID, fmt.Sprintf(
		"Fix job %s pushed `%s` @ %s. The factory checked the real diff: %d changed lines in %s (cap %d, no forbidden areas). Queued as merge request %s; the merge job opens the PR, merges after CI and deploys through the normal flow.\n\n**What changed:** %s\n\n**How to revert:** before it lands, `POST /v1/merge-requests/%s/cancel`. After it lands: %s",
		job.ID, r.Branch, shortHead(r.Head), lines, strings.Join(paths, ", "), p.MaxLines, mr.ID, r.Change, mr.ID, p.Plan.Revert))
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
