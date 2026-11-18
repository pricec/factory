package mcpfactory

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pricec/factory/internal/tasks"
)

type getMessageArgs struct {
	MessageID string `json:"message_id"`
}

// addMessageTool registers get_message: the recovery path for a payload
// truncated in a wake prompt or get_task (the marker names the id).
func addMessageTool(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_message",
		Description: "Full text of a message by id (see '…[truncated' markers).",
	}, getMessageTool(cfg))
}

func getMessageTool(cfg Config) mcp.ToolHandlerFor[getMessageArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in getMessageArgs) (*mcp.CallToolResult, any, error) {
		id := strings.TrimSpace(in.MessageID)
		if id == "" {
			return nil, nil, fmt.Errorf("message_id: required")
		}
		env, ok, err := cfg.Inbox.GetMessage(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("unknown message %q", id)
		}
		// Deliberately uncapped: the caller asked for this message.
		payload := compactJSON(env.Payload)
		return structuredResult(map[string]any{
			"id":       env.ID,
			"type":     env.Type,
			"from":     env.From,
			"job_id":   env.JobID,
			"ts":       env.TS,
			"internal": tasks.IsInternal(env.Type),
			"bytes":    len(payload),
			"payload":  string(payload),
		}), nil, nil
	}
}
