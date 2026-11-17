package auditor

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Windows of the audit (spec §6.12): findings focus on the last 24h, with
// a 7-day trend.
const (
	Window24h = 24 * time.Hour
	Window7d  = 7 * 24 * time.Hour
)

// JobRow is one job as the audit sees it.
type JobRow struct {
	ID         string    `json:"id"`
	State      string    `json:"state"`
	ForemanID  string    `json:"foreman_id,omitempty"`
	Attempts   int       `json:"attempts"`
	LastError  string    `json:"last_error,omitempty"`
	OverBudget bool      `json:"over_budget,omitempty"`
	HeldReason string    `json:"held_reason,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Tier       string    `json:"tier"`
	CreatedBy  string    `json:"created_by"`
	Kind       string    `json:"kind"`
	Task       string    `json:"task"`
	TimeoutS   int       `json:"timeout_s"`
	Priority   int       `json:"priority"`
	InTokens   int       `json:"input_tokens"`
	OutTokens  int       `json:"output_tokens"`
	CostUSD    float64   `json:"cost_usd"`
	// Errors are the error messages of the job's results (plus
	// last_error), newest last.
	Errors  []string `json:"errors,omitempty"`
	Summary string   `json:"summary,omitempty"`
	// TaskID / TaskOpen: the task the job is filed under, if any.
	TaskID   string `json:"task_id,omitempty"`
	TaskOpen bool   `json:"task_open,omitempty"`
	// AuditRun is the audit run a factory:auditor job belongs to.
	AuditRun string `json:"audit_run,omitempty"`
}

// MergeRow is one merge request.
type MergeRow struct {
	ID          string    `json:"id"`
	Branch      string    `json:"branch"`
	Head        string    `json:"head"`
	State       string    `json:"state"`
	Result      string    `json:"result,omitempty"`
	RequestedBy string    `json:"requested_by"`
	JobID       string    `json:"job_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// ForemanRow is one registered foreman.
type ForemanRow struct {
	ID       string    `json:"id"`
	Online   bool      `json:"online"`
	Build    string    `json:"build,omitempty"`
	Tools    string    `json:"tools,omitempty"`
	LastSeen time.Time `json:"last_seen"`
}

// Worktree is one git worktree of the scanned repo.
type Worktree struct {
	Path       string    `json:"path"`
	Branch     string    `json:"branch,omitempty"`
	Head       string    `json:"head"`
	Merged     bool      `json:"merged"`
	Clean      bool      `json:"clean"`
	Prunable   bool      `json:"prunable,omitempty"`
	LastCommit time.Time `json:"last_commit"`
}

// Snapshot is everything the detectors read.
type Snapshot struct {
	Now       time.Time    `json:"now"`
	Jobs      []JobRow     `json:"jobs"`
	Merges    []MergeRow   `json:"merges"`
	Foremen   []ForemanRow `json:"foremen"`
	Worktrees []Worktree   `json:"worktrees,omitempty"`
	// WorktreeErr explains a skipped/failed git scan.
	WorktreeErr string `json:"worktree_err,omitempty"`
}

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// JobKind classifies a job by who created it.
func JobKind(createdBy string, parent bool) string {
	switch {
	case createdBy == "factory:merge_queue":
		return "merge"
	case createdBy == CreatedBy:
		return "audit"
	case createdBy == "ui":
		return "operator"
	case parent || strings.HasPrefix(createdBy, "worker:"):
		return "child"
	case createdBy == "factory":
		return "planner"
	case strings.HasPrefix(createdBy, "foreman"):
		return "brain"
	case createdBy == "":
		return "unknown"
	}
	return createdBy
}

// Collect reads the snapshot from the factory's jobs.db and (optionally,
// nil skips) inbox.db handles. It only runs SELECTs, so both handles may
// be read-only. Jobs: everything created in the last 7 days plus every
// non-terminal job, whatever its age.
func Collect(ctx context.Context, jobsDB, inboxDB *sql.DB, now time.Time) (Snapshot, error) {
	snap := Snapshot{Now: now.UTC()}
	since := now.Add(-Window7d).UTC().Format(time.RFC3339)
	rows, err := jobsDB.QueryContext(ctx, `
SELECT id, state, foreman_id, attempts, IFNULL(last_error, ''), IFNULL(usage, ''), over_budget,
       IFNULL(held_reason, ''), created_at, updated_at, spec, results
FROM jobs
WHERE created_at >= ? OR state NOT IN ('done', 'timed_out', 'dead', 'cancelled', 'superseded')
ORDER BY created_at`, since)
	if err != nil {
		return snap, err
	}
	defer rows.Close()
	for rows.Next() {
		var j JobRow
		var usage, specJSON, results, created, updated string
		var over int
		if err := rows.Scan(&j.ID, &j.State, &j.ForemanID, &j.Attempts, &j.LastError, &usage, &over,
			&j.HeldReason, &created, &updated, &specJSON, &results); err != nil {
			return snap, err
		}
		j.OverBudget = over != 0
		j.CreatedAt, j.UpdatedAt = parseTS(created), parseTS(updated)
		var spec struct {
			Task      string          `json:"task"`
			Tier      string          `json:"tier"`
			CreatedBy string          `json:"created_by"`
			TimeoutS  int             `json:"timeout_s"`
			Priority  int             `json:"priority"`
			Parent    string          `json:"parent_job_id"`
			Payload   json.RawMessage `json:"payload"`
		}
		_ = json.Unmarshal([]byte(specJSON), &spec)
		j.Task = clip(strings.Join(strings.Fields(spec.Task), " "), 120)
		j.Tier = spec.Tier
		if j.Tier == "" {
			j.Tier = "flash"
		}
		j.CreatedBy, j.TimeoutS, j.Priority = spec.CreatedBy, spec.TimeoutS, spec.Priority
		j.Kind = JobKind(spec.CreatedBy, spec.Parent != "")
		if j.Kind == "audit" {
			var p struct {
				Kind string `json:"kind"`
				Run  string `json:"audit_run"`
			}
			_ = json.Unmarshal(spec.Payload, &p)
			j.AuditRun = p.Run
			if p.Kind == PayloadKindAudit {
				j.AuditRun = j.ID
			} else {
				j.Kind = "audit_fix"
			}
		}
		var u struct {
			In   int     `json:"input_tokens"`
			Out  int     `json:"output_tokens"`
			Cost float64 `json:"cost_usd"`
		}
		if usage != "" {
			_ = json.Unmarshal([]byte(usage), &u)
		}
		j.InTokens, j.OutTokens, j.CostUSD = u.In, u.Out, u.Cost
		var res []struct {
			Summary string `json:"summary"`
			Error   *struct {
				Kind    string `json:"kind"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(results), &res)
		for _, r := range res {
			if r.Error != nil && r.Error.Message != "" {
				j.Errors = append(j.Errors, r.Error.Message)
			}
			j.Summary = r.Summary
		}
		if j.LastError != "" && (len(j.Errors) == 0 || j.Errors[len(j.Errors)-1] != j.LastError) {
			j.Errors = append(j.Errors, j.LastError)
		}
		snap.Jobs = append(snap.Jobs, j)
	}
	if err := rows.Err(); err != nil {
		return snap, err
	}
	rows.Close()
	if err := addUsageEvents(ctx, jobsDB, &snap, since); err != nil {
		return snap, err
	}
	if err := collectMerges(ctx, jobsDB, &snap, since); err != nil {
		return snap, err
	}
	if err := collectForemen(ctx, jobsDB, &snap); err != nil {
		return snap, err
	}
	if inboxDB != nil {
		if err := linkTasks(ctx, inboxDB, &snap); err != nil {
			return snap, err
		}
	}
	return snap, nil
}

// addUsageEvents raises each job's tokens/cost to its usage_events total
// when that is higher (events arrive while a job runs; jobs.usage is the
// rollup recorded with the result).
func addUsageEvents(ctx context.Context, db *sql.DB, snap *Snapshot, since string) error {
	rows, err := db.QueryContext(ctx, `
SELECT job_id, SUM(input), SUM(output), SUM(cost_usd) FROM usage_events
WHERE job_id != '' AND ts >= ? GROUP BY job_id`, since)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return err
	}
	defer rows.Close()
	idx := map[string]int{}
	for i, j := range snap.Jobs {
		idx[j.ID] = i
	}
	for rows.Next() {
		var id string
		var in, out int
		var cost float64
		if err := rows.Scan(&id, &in, &out, &cost); err != nil {
			return err
		}
		if i, ok := idx[id]; ok {
			j := &snap.Jobs[i]
			if cost > j.CostUSD {
				j.CostUSD = cost
			}
			if in > j.InTokens {
				j.InTokens = in
			}
			if out > j.OutTokens {
				j.OutTokens = out
			}
		}
	}
	return rows.Err()
}

func collectMerges(ctx context.Context, db *sql.DB, snap *Snapshot, since string) error {
	rows, err := db.QueryContext(ctx, `
SELECT id, branch, head, state, result, requested_by, job_id, created_at
FROM merge_requests WHERE created_at >= ? OR state IN ('queued', 'merging') ORDER BY created_at`, since)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m MergeRow
		var created string
		if err := rows.Scan(&m.ID, &m.Branch, &m.Head, &m.State, &m.Result, &m.RequestedBy, &m.JobID, &created); err != nil {
			return err
		}
		m.CreatedAt = parseTS(created)
		snap.Merges = append(snap.Merges, m)
	}
	return rows.Err()
}

func collectForemen(ctx context.Context, db *sql.DB, snap *Snapshot) error {
	rows, err := db.QueryContext(ctx, `SELECT id, capabilities, IFNULL(last_seen, ''), online FROM foremen ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f ForemanRow
		var caps, seen string
		var online int
		if err := rows.Scan(&f.ID, &caps, &seen, &online); err != nil {
			return err
		}
		if !strings.HasPrefix(f.ID, "foreman:") {
			continue // planner:main and other non-foreman identities
		}
		var c map[string]any
		_ = json.Unmarshal([]byte(caps), &c)
		f.Build, _ = c["build"].(string)
		f.Tools, _ = c["tools"].(string)
		f.Online, f.LastSeen = online != 0, parseTS(seen)
		snap.Foremen = append(snap.Foremen, f)
	}
	return rows.Err()
}

// linkTasks fills TaskID/TaskOpen from inbox.db's task_jobs (spec §9.5.1).
func linkTasks(ctx context.Context, db *sql.DB, snap *Snapshot) error {
	rows, err := db.QueryContext(ctx, `SELECT tj.job_id, tj.task_id, t.status FROM task_jobs tj JOIN tasks t ON t.id = tj.task_id`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return err
	}
	defer rows.Close()
	type link struct {
		task string
		open bool
	}
	links := map[string]link{}
	for rows.Next() {
		var job, task, status string
		if err := rows.Scan(&job, &task, &status); err != nil {
			return err
		}
		if l, ok := links[job]; !ok || (!l.open && status == "open") {
			links[job] = link{task, status == "open"}
		}
	}
	for i := range snap.Jobs {
		if l, ok := links[snap.Jobs[i].ID]; ok {
			snap.Jobs[i].TaskID, snap.Jobs[i].TaskOpen = l.task, l.open
		}
	}
	return rows.Err()
}
