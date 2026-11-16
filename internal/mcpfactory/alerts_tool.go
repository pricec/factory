package mcpfactory

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RaiseAlertInput is the shared raise_alert argument set (planner MCP tool,
// foreman MCP tool and the alert.raise envelope payload, spec §9.5.2).
type RaiseAlertInput struct {
	Priority int    `json:"priority"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	TTLS     int    `json:"ttl_s,omitempty"`
	NoExpiry bool   `json:"no_expiry,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
}

// RaisedAlert is what the raise backend reports back to the tool caller.
type RaisedAlert struct {
	ID        string
	State     string
	ExpiresAt *time.Time
}

// RaiseAlertFunc creates an alert from the planner (Config.RaiseAlert).
type RaiseAlertFunc func(ctx context.Context, in RaiseAlertInput) (RaisedAlert, error)

// raiseAlertDescription stays short: the factory MCP description budget is 2KB.
const raiseAlertDescription = "Raise an operator alert (left pane). priority 0=most urgent. " +
	"Default expiry: P0/P1 never, P2 24h, P3+ 4h; override with ttl_s or no_expiry. " +
	"Optional task_id links a task."

func addRaiseAlertTool(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{Name: "raise_alert", Description: raiseAlertDescription}, raiseAlertTool(cfg))
}

func raiseAlertTool(cfg Config) mcp.ToolHandlerFor[RaiseAlertInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in RaiseAlertInput) (*mcp.CallToolResult, any, error) {
		if cfg.RaiseAlert == nil {
			return nil, nil, fmt.Errorf("raise_alert: not configured")
		}
		got, err := cfg.RaiseAlert(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		out := map[string]any{"ok": true, "id": got.ID, "state": got.State, "expires_at": nil}
		if got.ExpiresAt != nil {
			out["expires_at"] = got.ExpiresAt.UTC().Format(time.RFC3339)
		}
		return structuredResult(out), nil, nil
	}
}
