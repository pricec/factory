// Package mcpworker exposes the worker's MCP tool surface (spec §9.2):
// report_result, ask_parent, and - 2026-11-15 sub-spawning amendment -
// spawn_child and collect_children: a streamable-HTTP MCP server authed by
// per-job worker tokens (spec §7.3), mounted on the foreman at
// /v1/mcp/worker.
package mcpworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/wire"
)

// Pinned constants (spec §9.2, §11).
const (
	// DefaultWaitSeconds is the ask_parent default wait (spec §11).
	DefaultWaitSeconds = 60
	// MaxWaitSeconds caps ask_parent's wait_seconds (spec §4.1 long-poll cap).
	MaxWaitSeconds = 60
	// DefaultAnswer is returned on ask_parent timeout (spec §11).
	DefaultAnswer = "proceed with your best judgment"
	// resultFlag is the flag file the harness checks (spec §8.5).
	resultFlag = ".result_posted"
)

// Config wires the worker MCP server (spec §9.2).
type Config struct {
	// Auth resolves bearers to identities and job scopes (spec §7.3).
	Auth *auth.Store
	// Inbox is the local inbox service, used as a library (no HTTP).
	Inbox *inbox.Service
	// ForemanInboxID is the foreman's own inbox, e.g. "foreman:alpha";
	// job.result and job.question land here (spec §3.2).
	ForemanInboxID string
	// WorkDir is the flag-file root: WorkDir/jobs/{job_id}/.result_posted
	// (spec §8.2, §8.5).
	WorkDir string
	// Now is the clock; injectable for tests. Nil means time.Now.
	Now func() time.Time
	// Spawn enqueues a child JobSpec to the worker harness queue (spec
	// §9.2, 2026-11-15 sub-spawning amendment) - the same seam the
	// foreman brain's spawn_worker uses (§9.3); the integration wires
	// foremanapp.SpawnWorker. It must return immediately - spawn_child
	// never blocks on the child - and its errors surface as tool errors.
	Spawn func(ctx context.Context, spec wire.JobSpec) error
	// ParentSpec resolves a job's persisted JobSpec (the spec the spawner
	// writes before the harness starts): spawn_child reads the caller's
	// own spec for the depth cap and the tier/timeout/budget inheritance
	// (spec §9.2). ok is false when no spec is persisted for the job.
	ParentSpec func(jobID string) (spec wire.JobSpec, ok bool, err error)
	// ChildJobs reports the child job ids the given parent spawned in its
	// current attempt - the spawn state lives foreman-side, next to the
	// spawner; collect_children filters the per-job inbox to these.
	ChildJobs func(parentJobID string) []string
}

func (c *Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Handler builds the worker MCP endpoint (spec §9.2): the four-tool MCP
// server behind bearer auth that resolves the per-job worker token to a
// job_id and rejects tokens without a job scope.
func Handler(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Auth == nil:
		return nil, fmt.Errorf("mcpworker: Config.Auth is required")
	case cfg.Inbox == nil:
		return nil, fmt.Errorf("mcpworker: Config.Inbox is required")
	case cfg.ForemanInboxID == "":
		return nil, fmt.Errorf("mcpworker: Config.ForemanInboxID is required")
	case cfg.WorkDir == "":
		return nil, fmt.Errorf("mcpworker: Config.WorkDir is required")
	case cfg.Spawn == nil:
		return nil, fmt.Errorf("mcpworker: Config.Spawn is required")
	case cfg.ParentSpec == nil:
		return nil, fmt.Errorf("mcpworker: Config.ParentSpec is required")
	case cfg.ChildJobs == nil:
		return nil, fmt.Errorf("mcpworker: Config.ChildJobs is required")
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "factory-worker", Version: "v1"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "report_result",
		Description: reportResultDescription,
		InputSchema: reportResultSchema(),
	}, reportResultTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ask_parent",
		Description: "Ask the parent foreman a question; returns its answer.",
	}, askParentTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "spawn_child",
		Description: "Spawn one child worker job; returns its job_id. Tier/timeout/budget inherit this job's; timeout and budget are caps.",
	}, spawnChildTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "collect_children",
		Description: "Long-poll this job's inbox for child job results; returns the pending ones.",
	}, collectChildrenTool(cfg))

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return bearerAuth(cfg, h), nil
}

// requestCred is the resolved per-request credential, stashed in the
// request context by bearerAuth for the tool handlers.
type requestCred struct {
	identity string
	jobID    string
}

type credKey struct{}

func credFromContext(ctx context.Context) (requestCred, bool) {
	c, ok := ctx.Value(credKey{}).(requestCred)
	return c, ok
}

// bearerAuth wraps next: 401 on a missing/unknown/revoked bearer (spec
// §10), 403 when the credential has no job scope (spec §9.2). Worker tokens
// resolve job_id + identity; both become the per-request context the tools
// read.
func bearerAuth(cfg Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, jobID, err := cfg.Auth.Identify(r.Context(), bearerOf(r.Header.Get("Authorization")))
		var ue *auth.UnauthorizedError
		if errors.As(err, &ue) {
			writeError(w, http.StatusUnauthorized, "unauthorized", ue.Reason)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if jobID == "" {
			writeError(w, http.StatusForbidden, "forbidden", "token has no job scope")
			return
		}
		ctx := context.WithValue(r.Context(), credKey{}, requestCred{identity: identity, jobID: jobID})
		next.ServeHTTP(w, r.WithContext(ctx))
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

// writeResultFlag writes the flag file the harness checks (spec §8.5):
// WorkDir/jobs/{job_id}/.result_posted, 0600, parents created first.
func writeResultFlag(cfg Config, jobID string) error {
	dir := filepath.Join(cfg.WorkDir, "jobs", jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, resultFlag), nil, 0o600)
}
