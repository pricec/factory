package mcpworker

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MergeRequestArgs is request_merge's argument set (spec §9.2, §6.11):
// the worker twin of the planner's request_merge, plus the dependency
// gate.
type MergeRequestArgs struct {
	Branch string   `json:"branch"`
	Head   string   `json:"head" jsonschema:"pushed commit sha"`
	Note   string   `json:"note,omitempty" jsonschema:"PR title"`
	After  []string `json:"after,omitempty" jsonschema:"branches that must merge first"`
	Repo   string   `json:"repo,omitempty"`
}

// MergeRequestResult is what the factory answered: the queued request.
type MergeRequestResult struct {
	ID        string   `json:"id"`
	State     string   `json:"state"`
	WaitingOn []string `json:"waiting_on,omitempty"`
}

// requestMergeDescription is the whole "own your merge" instruction a
// worker sees on the tool itself; it counts toward the §9.1 worker budget.
const requestMergeDescription = "Queue YOUR pushed, CI-green branch for merge+deploy. Call it yourself when done, " +
	"then report done with the id; the factory tracks the outcome and fixes failures."

// errNoMergeQueue is the tool error when the foreman wired no relay.
var errNoMergeQueue = errors.New("request_merge: this foreman has no merge-queue relay; report the branch and head in your result instead")

// requestMergeTool files a merge request for the calling job's branch
// (spec §9.2): the foreman relays it to the factory's POST
// /v1/merge-requests with the caller's job id, so the request is
// job-owned (§6.11). Validation and duplicate errors come back as tool
// errors with the factory's message.
func requestMergeTool(cfg Config) mcp.ToolHandlerFor[MergeRequestArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in MergeRequestArgs) (*mcp.CallToolResult, any, error) {
		cred, ok := credFromContext(ctx)
		if !ok {
			return nil, nil, errors.New("request_merge: no job credential")
		}
		if cfg.RequestMerge == nil {
			return nil, nil, errNoMergeQueue
		}
		if in.Branch == "" || in.Head == "" {
			return nil, nil, errors.New("request_merge: branch and head are required")
		}
		res, err := cfg.RequestMerge(ctx, cred.jobID, in)
		if err != nil {
			return nil, nil, fmt.Errorf("request_merge: %w", err)
		}
		return structuredResult(res), nil, nil
	}
}
