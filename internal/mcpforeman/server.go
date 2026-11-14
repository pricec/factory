package mcpforeman

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
	"github.com/pricec/factory/internal/wire"
)

// Config wires the foreman brain MCP server (spec §9.3).
type Config struct {
	// Auth resolves bearers to identities (spec §7.3).
	Auth *auth.Store
	// Inbox is the local inbox service, used as a library (no HTTP).
	Inbox *inbox.Service
	// ForemanInboxID is the foreman's own inbox, e.g. "foreman:alpha":
	// inbox_next/inbox_ack operate here and brain-posted envelopes
	// originate from it (spec §3.2, §9.3).
	ForemanInboxID string
	// ParentInboxID is the factory's processing inbox, "factory:main":
	// post_to_factory lands here (spec §7.3, §9.3).
	ParentInboxID string
	// Spawn enqueues a JobSpec to the worker harness queue (spec §9.3);
	// the integration wires workerharness. It must return immediately -
	// spawn_worker never blocks - and its errors surface as tool errors.
	Spawn func(ctx context.Context, spec wire.JobSpec) error
}

// Handler builds the foreman brain MCP endpoint (spec §9.3): the
// pinned five-tool MCP server behind bearer auth that accepts only
// foreman-scoped credentials.
func Handler(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Auth == nil:
		return nil, fmt.Errorf("mcpforeman: Config.Auth is required")
	case cfg.Inbox == nil:
		return nil, fmt.Errorf("mcpforeman: Config.Inbox is required")
	case cfg.ForemanInboxID == "":
		return nil, fmt.Errorf("mcpforeman: Config.ForemanInboxID is required")
	case cfg.ParentInboxID == "":
		return nil, fmt.Errorf("mcpforeman: Config.ParentInboxID is required")
	case cfg.Spawn == nil:
		return nil, fmt.Errorf("mcpforeman: Config.Spawn is required")
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "factory-foreman", Version: "v1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "inbox_next",
		Description: "Oldest unacked inbox message; waits up to wait_seconds.",
	}, inboxNextTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "inbox_ack",
		Description: "Acknowledge inbox messages by id.",
	}, inboxAckTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "post_to_factory",
		Description: "Post an envelope to the factory inbox.",
	}, postToFactoryTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "spawn_worker",
		Description: "Queue a local worker job; returns its job_id. tier: flash (GLM 5.3 Flash, cheapest - default), default (GLM 5.3 FlashX), hardest (GLM 5.3). Pick the cheapest tier that can do the job.",
	}, spawnWorkerTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "answer_worker",
		Description: "Deliver an answer to a worker's question.",
	}, answerWorkerTool(cfg))

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return bearerAuth(cfg, h), nil
}

// credKey keys the per-request foreman identity in the request context
// (spec §9.1: the bearer middleware resolves the token; tools read the
// inbox ids from Config, which is pinned per mount).
type credKey struct{}

// bearerAuth wraps next: 401 on a missing/unknown/revoked bearer (spec
// §10), 403 when the credential is not foreman-scoped (spec §9.3) -
// worker tokens carry a job scope, foreman credentials never do (spec
// §7.3). The authenticated identity rides the request context.
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
		if !strings.HasPrefix(identity, "foreman:") || jobID != "" {
			writeError(w, http.StatusForbidden, "forbidden", "not a foreman credential")
			return
		}
		ctx := context.WithValue(r.Context(), credKey{}, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerOf extracts the bearer token from an Authorization header. A
// package-local twin of mcpworker's (unexported there); mcpworker is
// frozen by #17, so the shared helper waits for #19 to hoist.
func bearerOf(h string) string {
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// writeError emits the normative error model (spec §10). Package-local
// twin of mcpworker's, for the same reason as bearerOf.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message, "retryable": false},
	})
}
