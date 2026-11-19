package mcpfactory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pricec/factory/internal/tasks"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RaiseAlertInput is the shared raise_alert argument set (planner MCP tool,
// foreman MCP tool and the alert.raise envelope payload, spec §9.5.3).
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

type listAlertsArgs struct {
	Limit int `json:"limit,omitempty"`
}

type ackAlertArgs struct {
	IDs []string `json:"ids"`
}

func addAlertTriageTools(srv *mcp.Server, cfg Config) {
	if cfg.Tasks == nil {
		return
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_alerts",
		Description: "List open alerts (most urgent first).",
	}, listAlertsTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ack_alert",
		Description: "Ack/resolve open alerts by id once handled; unknown or already-closed ids are reported, not fatal.",
	}, ackAlertTool(cfg))
}

func listAlertsTool(cfg Config) mcp.ToolHandlerFor[listAlertsArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in listAlertsArgs) (*mcp.CallToolResult, any, error) {
		if cfg.Tasks == nil {
			return nil, nil, fmt.Errorf("list_alerts: not configured")
		}
		if _, err := cfg.Tasks.ExpireAlerts(ctx, cfg.Tasks.Now()); err != nil {
			return nil, nil, err
		}
		if in.Limit <= 0 || in.Limit > 50 {
			in.Limit = 50
		}
		list, err := cfg.Tasks.ListAlerts(ctx, tasks.AlertFilter{States: []string{tasks.AlertOpen}, Limit: in.Limit})
		if err != nil {
			return nil, nil, err
		}
		out := make([]map[string]any, 0, len(list))
		for _, al := range list {
			out = append(out, map[string]any{
				"id": al.ID, "priority": al.Priority, "title": al.Title,
				"body": al.Body, "source": al.Source, "task_id": al.TaskID,
				"created_at": al.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
		return structuredResult(map[string]any{"alerts": out}), nil, nil
	}
}

func ackAlertTool(cfg Config) mcp.ToolHandlerFor[ackAlertArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ackAlertArgs) (*mcp.CallToolResult, any, error) {
		if cfg.Tasks == nil {
			return nil, nil, fmt.Errorf("ack_alert: not configured")
		}
		if len(in.IDs) == 0 {
			return nil, nil, fmt.Errorf("ack_alert: ids: required")
		}
		acked := []string{}
		skipped := map[string]string{}
		for _, id := range in.IDs {
			al, err := cfg.Tasks.TransitionAlert(ctx, id, []string{tasks.AlertOpen}, tasks.AlertAcked)
			switch {
			case errors.Is(err, tasks.ErrNotFound):
				skipped[id] = "unknown"
			case errors.Is(err, tasks.ErrStateConflict):
				skipped[id] = "already " + al.State
			case err != nil:
				return nil, nil, err
			default:
				acked = append(acked, id)
				if cfg.AlertChanged != nil {
					cfg.AlertChanged(al)
				}
			}
		}
		return structuredResult(map[string]any{"ok": true, "acked": acked, "skipped": skipped}), nil, nil
	}
}
