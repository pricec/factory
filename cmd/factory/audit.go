package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pricec/factory/internal/auditor"
	_ "modernc.org/sqlite"
)

// runAudit implements `factory audit -dry-run` (spec §6.12): it opens the
// factory's databases READ-ONLY, runs the same snapshot, detectors, dedup
// and guardrail decisions as the scheduled audit, and prints the findings
// and what a run would do - without filing tasks, starting jobs or
// writing anything. The live audit only runs from the factory's schedule.
//
// Like the factory service, the CLI runs no git: the worktree scan is
// done by the audit job on the foreman. By default the dry run reports the
// scan as skipped; -worktrees FILE feeds it a scan in the audit job's
// result format ({"worktrees": [...]} or a bare array, e.g. the result of
// a past audit job) so the cleanup decisions can be previewed.
func runAudit(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	dataDir := fs.String("data", "./factory-data", "factory data directory (jobs.db, inbox.db)")
	dryRun := fs.Bool("dry-run", false, "print findings and decisions; file nothing, fix nothing")
	asJSON := fs.Bool("json", false, "print the decisions as JSON")
	wtFile := fs.String("worktrees", "", "a worktree scan in the audit job's result format to decide on (default: skipped; the scan runs inside the audit job)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*dryRun {
		return fmt.Errorf("audit: only -dry-run is supported from the CLI; the live audit runs from the factory's daily schedule")
	}
	cfg, warns := auditor.LoadConfig(os.Getenv)
	ctx := context.Background()
	jobsDB, err := openRO(filepath.Join(*dataDir, "jobs.db"))
	if err != nil {
		return err
	}
	defer jobsDB.Close()
	inboxDB, err := openRO(filepath.Join(*dataDir, "inbox.db"))
	if err != nil {
		return err
	}
	defer inboxDB.Close()

	now := time.Now().UTC()
	snap, err := auditor.Collect(ctx, jobsDB, inboxDB, now)
	if err != nil {
		return fmt.Errorf("audit: collect: %w", err)
	}
	var rejected []string
	if *wtFile == "" {
		snap.WorktreeErr = "skipped: the scan runs inside the audit job on " + cfg.Foreman + " (pass -worktrees FILE to preview one)"
	} else {
		sweep, err := readScan(*wtFile)
		if err != nil {
			return err
		}
		rejected = auditor.ApplySweepScan(&snap, sweep, cfg.WorktreeRoot)
	}
	findings := auditor.Detect(snap, auditor.DetectOptions{
		WorktreeRoot: cfg.WorktreeRoot, StaleAfter: cfg.StaleAfter, MaxLines: cfg.MaxChangedLines,
	})
	store := &auditor.FindingStore{DB: jobsDB, ReadOnly: true}
	meter := auditor.NewMeter(cfg.BudgetUSD, nil)
	runCap := &auditor.RunCap{Max: cfg.MaxAutoFixes}
	actions, err := auditor.DecideRun(ctx, findings, auditor.RunInputs{
		Config: cfg, Plan: auditor.ContextFromSnapshot(snap, cfg), Meter: &meter, Cap: runCap,
		Prior: func(ctx context.Context, fp string) (*auditor.Record, auditor.TaskState, error) {
			rec, err := store.Get(ctx, fp)
			if err != nil || rec == nil {
				return rec, auditor.TaskState{}, err
			}
			return rec, taskState(ctx, inboxDB, rec.TaskID), nil
		},
	})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"config": cfg, "warnings": warns, "actions": actions})
	}
	printDryRun(stdout, cfg, warns, snap, actions, meter, runCap)
	for _, rj := range rejected {
		fmt.Fprintf(stdout, "rejected reported worktree: %s\n", rj)
	}
	return nil
}

// readScan reads a worktree scan file: an audit job result object or a
// bare array of worktrees.
func readScan(path string) (auditor.SweepResult, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return auditor.SweepResult{}, err
	}
	if t := strings.TrimSpace(string(b)); strings.HasPrefix(t, "[") {
		b = []byte(`{"findings":[],"worktrees":` + t + `}`)
	}
	return auditor.ParseSweep(b)
}

func openRO(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, db.Ping()
}

func taskState(ctx context.Context, db *sql.DB, id string) auditor.TaskState {
	var status, reason string
	err := db.QueryRowContext(ctx, `SELECT status, close_reason FROM tasks WHERE id = ?`, id).Scan(&status, &reason)
	if err != nil {
		return auditor.TaskState{}
	}
	return auditor.TaskState{Exists: true, Open: status == "open", CloseReason: reason}
}

func printDryRun(w io.Writer, cfg auditor.Config, warns []string, snap auditor.Snapshot, actions []auditor.Action, meter auditor.Meter, runCap *auditor.RunCap) {
	fmt.Fprintf(w, "factory audit DRY RUN at %s (nothing filed, nothing fixed)\n", snap.Now.Format(time.RFC3339))
	fmt.Fprintf(w, "schedule %q, job priority %d, task priority %d, sweep tier %s, fix tier %s, auto-fix %v\n",
		cfg.CronSpec(), cfg.JobPriority, cfg.TaskPriority, cfg.SweepTier, cfg.FixTier, cfg.AutoFix)
	fmt.Fprintf(w, "guardrails: budget $%.2f/run (fix reserve $%.2f), max %d auto-fixes/run, max %d changed lines/PR\n",
		cfg.BudgetUSD, cfg.FixReserveUSD, cfg.MaxAutoFixes, cfg.MaxChangedLines)
	for _, wn := range warns {
		fmt.Fprintf(w, "config warning: %s\n", wn)
	}
	fmt.Fprintf(w, "snapshot: %d jobs (7d + live), %d merge requests, %d foremen, %d worktrees", len(snap.Jobs), len(snap.Merges), len(snap.Foremen), len(snap.Worktrees))
	if snap.WorktreeErr != "" {
		fmt.Fprintf(w, " (worktree scan: %s)", snap.WorktreeErr)
	}
	fmt.Fprintln(w)
	counts := map[string]int{}
	for _, a := range actions {
		counts[a.Mode]++
	}
	fmt.Fprintf(w, "%d findings: %d would be filed as proposals, %d auto-fixed, %d skipped (dedup); meter would reserve $%.2f; auto-fix slots used %d\n\n",
		len(actions), counts[auditor.ModeProposal], counts[auditor.ModeAutoFix], counts[auditor.ModeSkip], meter.SpentUSD, runCap.Used)
	for i, a := range actions {
		f := a.Finding
		title := auditor.ProposalTitle(f)
		if a.Mode == auditor.ModeAutoFix {
			title = auditor.AutoFixTitle(f)
		}
		fmt.Fprintf(w, "%d. [%s] %s\n", i+1, strings.ToUpper(a.Mode), title)
		fmt.Fprintf(w, "   fingerprint %s; %s = %.2f; dedup: %s\n", f.Fingerprint, f.MetricLabel, f.Metric, a.Dedup.Reason)
		for _, e := range f.Evidence {
			fmt.Fprintf(w, "   - %s\n", e)
		}
		if len(f.JobIDs) > 0 {
			ids := f.JobIDs
			if len(ids) > 8 {
				ids = append(append([]string{}, ids[:8]...), fmt.Sprintf("+%d more", len(f.JobIDs)-8))
			}
			fmt.Fprintf(w, "   jobs: %s\n", strings.Join(ids, ", "))
		}
		fmt.Fprintf(w, "   cause: %s\n   fix: %s\n   benefit: %s | effort: %s\n", f.Cause, f.Fix, f.Benefit, f.Effort)
		if f.Known != "" {
			fmt.Fprintf(w, "   known issue: %s\n", f.Known)
		}
		if f.AutoFix != nil {
			fmt.Fprintf(w, "   auto-fix candidate (%s): %s\n", f.AutoFix.Kind, f.AutoFix.Description)
		}
		if a.Why != "" {
			fmt.Fprintf(w, "   not auto-fixed: %s\n", a.Why)
		}
		fmt.Fprintln(w)
	}
}
