package mcpforeman

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/wire"
)

// TypeAlertRaise is the envelope type raise_alert posts to factory:main;
// the factory consumer turns it into an alert with source "foreman:<id>".
const TypeAlertRaise = "alert.raise"

// raiseAlertArgs is raise_alert's argument set (spec §9.3, §9.5.2).
type raiseAlertArgs struct {
	Priority int    `json:"priority"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	TTLS     int    `json:"ttl_s,omitempty"`
	NoExpiry bool   `json:"no_expiry,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
}

func addRaiseAlertTool(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "raise_alert",
		Description: "Raise an operator alert via the factory. priority 0=most urgent; " +
			"expiry defaults P0/P1 never, P2 24h, P3+ 4h (override ttl_s or no_expiry). " +
			"Validation happens factory-side.",
	}, raiseAlertTool(cfg))
}

// raiseAlertTool posts an alert.raise envelope to the parent inbox. Only
// local sanity checks run here; the factory consumer validates for real.
func raiseAlertTool(cfg Config) mcp.ToolHandlerFor[raiseAlertArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in raiseAlertArgs) (*mcp.CallToolResult, any, error) {
		if in.Priority < 0 {
			return nil, nil, fmt.Errorf("priority: must be >= 0")
		}
		if strings.TrimSpace(in.Title) == "" {
			return nil, nil, fmt.Errorf("title: required")
		}
		if in.TTLS < 0 || (in.TTLS > 0 && in.NoExpiry) {
			return nil, nil, fmt.Errorf("ttl_s must be > 0 and exclusive with no_expiry")
		}
		payload, err := json.Marshal(in)
		if err != nil {
			return nil, nil, err
		}
		env := wire.MessageEnvelope{Type: TypeAlertRaise, From: cfg.ForemanInboxID, Payload: payload}
		if err := env.Validate(); err != nil {
			return nil, nil, err
		}
		posted, err := cfg.Inbox.Post(ctx, cfg.ParentInboxID, env)
		if err != nil {
			return nil, nil, err
		}
		return structuredResult(map[string]any{"ok": true, "id": posted.ID}), nil, nil
	}
}
