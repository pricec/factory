package mcpfactory

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/jobs"
)

// requestMergeArgs is the request_merge argument set (spec §9.4, §6.11).
type requestMergeArgs struct {
	Branch string `json:"branch"`
	Head   string `json:"head"`
	Note   string `json:"note,omitempty"`
}

// requestMergeDescription tells the planner that merge-and-deploy asks are
// merge requests, never prose jobs (the 2026-11-17 merge queue amendment).
const requestMergeDescription = "Queue a merge of branch at commit head (sha) into master; note = PR title. " +
	"The factory runs one merge at a time via the fixed procedure (PR, green CI, merge, deploy). " +
	"Every \"merge (and deploy) X\" request MUST be request_merge, never a create_job prose job."

func addRequestMergeTool(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{Name: "request_merge", Description: requestMergeDescription}, requestMergeTool(cfg))
}

func requestMergeTool(cfg Config) mcp.ToolHandlerFor[requestMergeArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in requestMergeArgs) (*mcp.CallToolResult, any, error) {
		mr, err := cfg.Store.CreateMergeRequest(ctx, jobs.NewMergeRequest{
			Branch: in.Branch, Head: in.Head, Note: in.Note, RequestedBy: "planner",
		})
		if err != nil {
			return nil, nil, fmt.Errorf("request_merge: %w", err)
		}
		if cfg.MergeQueued != nil {
			cfg.MergeQueued()
		}
		return structuredResult(map[string]any{"id": mr.ID, "state": mr.State}), nil, nil
	}
}
