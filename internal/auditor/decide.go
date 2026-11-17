package auditor

import (
	"context"
	"path/filepath"
	"time"
)

// Modes of an Action.
const (
	ModeSkip     = "skip"
	ModeProposal = "proposal"
	ModeAutoFix  = "autofix"
)

// Action is what a run does with one finding.
type Action struct {
	Finding Finding  `json:"finding"`
	Dedup   Decision `json:"dedup"`
	Mode    string   `json:"mode"`
	// Why explains a proposal that had an auto-fix candidate.
	Why string `json:"why,omitempty"`
	// Reserve is the meter reservation of an auto-fix job.
	Reserve float64 `json:"reserve,omitempty"`
}

// PriorFunc returns a fingerprint's record and the state of its task.
type PriorFunc func(ctx context.Context, fp string) (*Record, TaskState, error)

// RunInputs are what DecideRun gates on.
type RunInputs struct {
	Config Config
	Plan   PlanContext
	Meter  *Meter
	Cap    *RunCap
	Prior  PriorFunc
}

// DeadHeldAfter is how long a job must have been held before the auditor
// may cancel it (operator decision 2026-11-17).
const DeadHeldAfter = 6 * time.Hour

// heldSince is when a held job entered the held queue: jobs.updated_at is
// the hold-since stamp while held (internal/jobs/held.go), falling back to
// created_at.
func heldSince(j JobRow) time.Time {
	if !j.UpdatedAt.IsZero() {
		return j.UpdatedAt
	}
	return j.CreatedAt
}

// isDeadHeld is the code definition of a "held-but-dead" job: held for
// over 6h, filed under a task that is now closed, and not created by the
// operator directly (created_by ui). Planner-created jobs under operator
// tasks qualify (operator decision 2026-11-17). Only these may be
// cancelled without asking.
func isDeadHeld(j JobRow, now time.Time) bool {
	return j.State == "held" && j.TaskID != "" && !j.TaskOpen && j.CreatedBy != "ui" &&
		now.Sub(heldSince(j)) > DeadHeldAfter
}

// ContextFromSnapshot derives the cleanup targets CheckPlan accepts.
func ContextFromSnapshot(s Snapshot, c Config) PlanContext {
	pc := PlanContext{
		MaxLines: c.MaxChangedLines, WorktreeRoot: c.WorktreeRoot,
		StaleWorktrees: map[string]bool{}, DeadHeld: map[string]bool{},
	}
	for _, w := range s.Worktrees {
		if w.Merged && w.Clean && !w.Prunable {
			pc.StaleWorktrees[filepath.Clean(w.Path)] = true
		}
	}
	for _, j := range s.Jobs {
		if isDeadHeld(j, s.Now) {
			pc.DeadHeld[j.ID] = true
		}
	}
	return pc
}

// DecideRun applies dedup, then the guardrails, to every finding in order
// (callers pass them most-severe first, SortFindings). An auto-fix
// candidate becomes an auto-fix only when auto-fixing is enabled, the
// plan passes CheckPlan, the per-run cap has room and the budget meter
// admits its reservation; otherwise it is filed as a proposal and Why
// says which condition failed.
func DecideRun(ctx context.Context, fs []Finding, in RunInputs) ([]Action, error) {
	var out []Action
	for _, f := range fs {
		rec, ts, err := in.Prior(ctx, f.Fingerprint)
		if err != nil {
			return out, err
		}
		a := Action{Finding: f, Dedup: Decide(f, rec, ts)}
		switch {
		case !a.Dedup.File:
			a.Mode = ModeSkip
		case f.AutoFix == nil:
			a.Mode = ModeProposal
		default:
			a.Mode, a.Why, a.Reserve = gate(f, in)
		}
		out = append(out, a)
	}
	return out, nil
}

func gate(f Finding, in RunInputs) (mode, why string, reserve float64) {
	if !in.Config.AutoFix {
		return ModeProposal, "auto-fixing is disabled (" + EnvAutoFix + ")", 0
	}
	if err := CheckPlan(*f.AutoFix, in.Plan); err != nil {
		return ModeProposal, err.Error(), 0
	}
	probe := *in.Cap
	if err := probe.Take(); err != nil {
		return ModeProposal, err.Error(), 0
	}
	if f.AutoFix.Kind != FixCancelDeadHeld { // runs in the factory, costs nothing
		reserve = in.Config.FixReserveUSD
		if err := in.Meter.Reserve(reserve); err != nil {
			return ModeProposal, err.Error(), 0
		}
	}
	_ = in.Cap.Take()
	return ModeAutoFix, "", reserve
}
