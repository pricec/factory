package mcpfactory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/secrets"
	"github.com/pricec/factory/internal/tasks"
)

// Config wires the factory planner MCP server (spec §9.4).
type Config struct {
	// Auth resolves bearers to identities (spec §7.3); the surface admits
	// only planner credentials (identity factory:planner).
	Auth *auth.Store
	// Store is the factory's jobs/schedules/spend store.
	Store *jobs.Store
	// Matcher selects the foreman for create_job dispatch (spec §6.7);
	// the CapabilityMatcher is wired by integration. Nil means every
	// created job stays pending (spec §6.7).
	Matcher registry.Matcher
	// Inbox is the factory's own inbox service, used as a library: message_
	// foreman posts to foreman:{id} inboxes on the factory.
	Inbox *inbox.Service
	// Secrets is the credentials store; create_job rejects secret names
	// absent from it (spec §13.2). Nil rejects any non-empty secrets.
	Secrets *secrets.Store
	// Tasks is the task store (spec §9.5.2). nil disables the task tools
	// and task_id handling (notify_operator then posts unthreaded).
	Tasks *tasks.Store
	// TaskChanged, when set, is told about every task a tool creates or
	// changes (the factory publishes task.changed on SSE, I5).
	TaskChanged func(ctx context.Context, taskID string)
	// RaiseAlert backs the raise_alert tool (spec §9.5.3); nil -> tool errors.
	RaiseAlert RaiseAlertFunc
	// AlertChanged, when set, is told about alerts ack_alert transitions
	// (the factory publishes alert.changed on SSE).
	AlertChanged func(tasks.Alert)
	// MarkSelfCancel, when set, is called before cancel_job cancels a job
	// and returns a func that withdraws the mark if the cancel fails; the
	// factory uses it so the planner's own cancels do not wake it.
	MarkSelfCancel func(jobID string) (undo func())
	// FactoryForeman is the foreman factory-repo work must be pinned to;
	// empty means DefaultFactoryForeman. FactoryRepoMarkers are the task
	// substrings that identify factory-repo work; nil means
	// DefaultFactoryRepoMarkers, an empty non-nil slice disables the guard.
	FactoryForeman     string
	FactoryRepoMarkers []string
	// MergeQueued, when set, is called after request_merge queues a merge
	// request (the factory kicks its merge-queue loop, spec §6.11).
	MergeQueued func()
	// DismissMerge backs the dismiss_merge tool (spec §6.11): closes a
	// needs_attention merge request. nil -> the tool errors.
	DismissMerge DismissMergeFunc
	// TaskPriorityChanged, when set, is called after update_task changes a
	// task's priority (the factory re-evaluates that task's held jobs).
	TaskPriorityChanged func(ctx context.Context, taskID string)
	// JobPriorityChanged, when set, is called after set_job_priority
	// changed a job's priority: the factory records the event with reason
	// and re-evaluates the throttle hold.
	JobPriorityChanged func(ctx context.Context, jobID string, oldPrio, newPrio int, reason string)
}

// Handler builds the factory planner MCP endpoint (spec §9.4): the five-tool
// MCP server behind bearer auth that admits only planner credentials.
func Handler(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Auth == nil:
		return nil, fmt.Errorf("mcpfactory: Config.Auth is required")
	case cfg.Store == nil:
		return nil, fmt.Errorf("mcpfactory: Config.Store is required")
	case cfg.Inbox == nil:
		return nil, fmt.Errorf("mcpfactory: Config.Inbox is required")
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "factory-planner", Version: "v1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_foremen",
		Description: "List foremen: {foremen: [{id, online, capabilities, host?}], hosts: [{host_id, hostname, ips, os, ..., foremen: [ids]}]}. " +
			"hosts groups foremen sharing one machine. A foreman's host is already reachable: pin a job to that foreman " +
			"(create_job foreman=) - never ask the operator for SSH to a host or IP listed here.",
	}, listForemenTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_jobs",
		Description: "List job summaries: {jobs: [{id, state, summary, usage, over_budget, held_reason?}]}. " +
			"state held = parked (held_reason: usage throttle, or conflict: waits for job_X); it dispatches automatically.",
	}, listJobsTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_job",
		Description: "Create and dispatch a job; schedule_cron also creates a schedule. " +
			"requires entries are capability tag NAMES - accepted forms: os:linux, arch:amd64, " +
			"tools:<tool>, or a bare tag name (os, arch, tools); NEVER bare values like linux " +
			"or amd64 (rejected as unknown_requires). " +
			"secrets: optional credential NAMES (must exist in the store); reach the worker as env vars. " +
			"foreman: optional pin (bare or foreman:-prefixed id), wins over requires; " +
			"unknown foreman errors; offline pin pends till it returns. " +
			"Work on the factory repo (pricec/factory or its worktrees) MUST pin foreman=foreman:server; " +
			"an unpinned job that mentions it is rejected (factory_repo_needs_pin). " +
			"tier: flash (cheapest, default), default (max-think), hardest (strongest); " +
			"pick the cheapest that works. model: explicit model id overrides tier. " +
			"model=claude-fable-5-2 (Fable): opt-in, rare; only if the operator asks or a hardest job clearly failed, and ALWAYS ask first. " +
			"It is created held (fable: awaiting operator approval) until the operator approves; cancel_job rejects it. " +
			"timeout_s: leave unset for code/build jobs (tier ceiling applies); never <1800 if builds/tests run. " +
			"touches: REQUIRED on code jobs: narrowest repo-relative path globs it edits (** ok), e.g. [\"internal/jobs/**\"]; " +
			"[\"*\"] = exclusive. Overlapping jobs run one after another (held: \"conflict: waits for job_X\"). " +
			"When subscription usage is high the job may come back state=held with held_reason: " +
			"it dispatches automatically (priority 0 is never held); cancel_job works on it.",
	}, createJobTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_result",
		Description: "Read one job's full results and spec.",
	}, readResultTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "cancel_job",
		Description: "Terminal-cancel a non-terminal job: cancelled (default) or superseded=true. Returns {job_id, state}; terminal jobs error.",
	}, cancelJobTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "message_foreman",
		Description: "Post to a foreman's inbox; foreman_id accepts bare or foreman:-prefixed ids.",
	}, messageForemanTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "notify_operator",
		Description: "Reply to the operator on a task thread (task_id defaults to the woken task); ALWAYS answer this way. ALWAYS also fill `spoken`: a 1-3 sentence plain-text conversational summary of the reply (no markdown, no code, no URLs or ids, max 300 chars) that is read aloud to the operator.",
	}, notifyOperatorTool(cfg))
	addTaskTools(srv, cfg) // spec §9.5.2 (tasks_tools.go)

	addSetJobPriorityTool(srv, cfg)
	addRaiseAlertTool(srv, cfg)
	addAlertTriageTools(srv, cfg) // list_alerts, ack_alert (spec §9.5.3)
	addDismissMergeTool(srv, cfg) // merge_dismiss_tool.go
	addRequestMergeTool(srv, cfg) // spec §6.11 merge queue (merge_tool.go)

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return plannerAuth(cfg, h), nil
}

// plannerAuth wraps next: 401 on a missing/unknown/revoked bearer (spec
// §10); 403 for any non-planner identity (foreman and worker credentials
// may not use the planner surface). The check is on identity, not scopes -
// planner credentials carry none (spec §9.4).
func plannerAuth(cfg Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Woken-task plumbing (spec §9.5.2): the planner session's MCP
		// URL carries ?task_id=<id>; tools read it as a header, which the
		// SDK hands them (RequestExtra.Header).
		if id := r.URL.Query().Get("task_id"); id != "" {
			r.Header.Set(WokenTaskHeader, id)
		}
		identity, _, err := cfg.Auth.Identify(r.Context(), bearerOf(r.Header.Get("Authorization")))
		var ue *auth.UnauthorizedError
		if errors.As(err, &ue) {
			writeError(w, http.StatusUnauthorized, "unauthorized", ue.Reason)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if identity != auth.FactoryPlannerIdentity {
			writeError(w, http.StatusForbidden, "forbidden", "credential "+identity+" may not use the planner surface")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerOf extracts the bearer token from an Authorization header.
func bearerOf(h string) string {
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// writeError emits the normative error model (spec §10).
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message, "retryable": false},
	})
}
