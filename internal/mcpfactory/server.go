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
	"github.com/pricec/factory/internal/budgets"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/registry"
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
	// Budget carries the daily spend cap and FACTORY_SPEND_OVERRIDE for the
	// create_job breaker (spec §6.5).
	Budget budgets.Config
	// Tasks is the task store (spec §9.5.2). nil disables the task tools
	// and task_id handling (notify_operator then posts unthreaded).
	Tasks *tasks.Store
	// TaskChanged, when set, is told about every task a tool creates or
	// changes (the factory publishes task.changed on SSE, I5).
	TaskChanged func(ctx context.Context, taskID string)
	// RaiseAlert backs the raise_alert tool (spec §9.5.3); nil -> tool errors.
	RaiseAlert RaiseAlertFunc
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
		Name:        "list_foremen",
		Description: "List registered foremen: {foremen: [{id, online, capabilities}]}.",
	}, listForemenTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_jobs",
		Description: "List job summaries: {jobs: [{id, state, summary, usage, over_budget, held_reason?}]}. " +
			"state held = parked by the usage throttle; it dispatches automatically when usage allows.",
	}, listJobsTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_job",
		Description: "Create and dispatch a job; schedule_cron also creates a schedule. " +
			"requires entries are capability tag NAMES - accepted forms: os:linux, arch:amd64, " +
			"tools:<tool>, or a bare tag name (os, arch, tools); NEVER bare values like linux " +
			"or amd64 (rejected as unknown_requires). " +
			"foreman: optional explicit pin (bare or foreman:-prefixed id) - wins over requires " +
			"matching; unknown foreman errors; offline pin pends until it returns. " +
			"tier: flash (cheapest, default), default (Flash max-thinking), hardest (strongest); " +
			"pick the cheapest that works. Refused past the spend cap. " +
			"When subscription usage is high the job may come back state=held with held_reason: " +
			"it is queued and dispatches automatically (task priority 0 is never held); cancel_job works on it.",
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
		Description: "Reply to the operator on a task thread (task_id defaults to the woken task); ALWAYS answer this way.",
	}, notifyOperatorTool(cfg))
	addTaskTools(srv, cfg) // spec §9.5.2 (tasks_tools.go)

	addRaiseAlertTool(srv, cfg)

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
