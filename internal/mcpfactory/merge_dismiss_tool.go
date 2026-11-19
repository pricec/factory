package mcpfactory

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/jobs"
)

// DismissMergeFunc closes a needs_attention merge request (Config.DismissMerge).
type DismissMergeFunc func(ctx context.Context, id, actor, reason string, force bool) (jobs.MergeRequest, error)

type dismissMergeArgs struct {
	ID     string `json:"id" jsonschema:"merge request id (mr_...)"`
	Reason string `json:"reason" jsonschema:"why it is stale (required)"`
	Force  bool   `json:"force,omitempty" jsonschema:"dismiss even though the branch has unmerged commits"`
}

const dismissMergeDescription = "Close a needs_attention merge request that is stale (work merged, superseded or empty); reason required, audited, its alerts are acked. " +
	"Refused while the branch has commits master lacks unless force=true."

func addDismissMergeTool(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{Name: "dismiss_merge", Description: dismissMergeDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in dismissMergeArgs) (*mcp.CallToolResult, any, error) {
			if cfg.DismissMerge == nil {
				return nil, nil, fmt.Errorf("dismiss_merge: not configured")
			}
			mr, err := cfg.DismissMerge(ctx, in.ID, jobs.MergeActorPlanner, in.Reason, in.Force)
			if err != nil {
				return nil, nil, fmt.Errorf("dismiss_merge: %w", err)
			}
			return structuredResult(map[string]any{"id": mr.ID, "state": mr.State, "result": mr.Result}), nil, nil
		})
}
