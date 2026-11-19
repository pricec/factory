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
	// Repo is the target repo ("pricec/factory", or a bare name like
	// "video-chat"); empty = pricec/factory. The merge targets that repo's
	// default branch (master for factory, main for video-chat).
	Repo string `json:"repo,omitempty" jsonschema:"target repo, e.g. pricec/factory (default) or video-chat; merges into its default branch"`
	// After: branches that must land first (spec §6.11 dependency gate).
	After []string `json:"after,omitempty"`
}

// requestMergeDescription tells the planner that merge-and-deploy asks are
// merge requests, never prose jobs (the 2026-11-17 merge queue amendment),
// and that routine queuing is no longer its job: code-change jobs file
// their own requests (job-owned merges, spec §6.11).
const requestMergeDescription = "Queue a merge of branch at commit head (sha) into the repo's default branch (repo optional, default pricec/factory = master; video-chat = main); note = PR title; after = branches that must land first. " +
	"Jobs queue their OWN merges (the factory tracks them): use this only for a branch no job queued. " +
	"One merge at a time via the fixed procedure (PR, green CI, merge, deploy). " +
	"Every \"merge (and deploy) X\" request MUST be request_merge, never a create_job prose job."

func addRequestMergeTool(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{Name: "request_merge", Description: requestMergeDescription}, requestMergeTool(cfg))
}

func requestMergeTool(cfg Config) mcp.ToolHandlerFor[requestMergeArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in requestMergeArgs) (*mcp.CallToolResult, any, error) {
		mr, err := cfg.Store.CreateMergeRequest(ctx, jobs.NewMergeRequest{
			Repo: in.Repo, Branch: in.Branch, Head: in.Head, Note: in.Note, After: in.After, RequestedBy: "planner",
		})
		if err != nil {
			return nil, nil, fmt.Errorf("request_merge: %w", err)
		}
		if cfg.MergeQueued != nil {
			cfg.MergeQueued()
		}
		out := map[string]any{"id": mr.ID, "state": mr.State}
		if len(mr.WaitingOn) > 0 {
			out["waiting_on"] = mr.WaitingOn
		}
		return structuredResult(out), nil, nil
	}
}
