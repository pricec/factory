package mcpfactory

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/wire"
)

type setJobPriorityArgs struct {
	JobID    string `json:"job_id"`
	Priority *int   `json:"priority"`
	Reason   string `json:"reason"`
}

// addSetJobPriorityTool registers set_job_priority: an in-place job-level
// priority override, so a throttle-held job can be bumped (0 = P0, never
// held) without superseding it and re-filing under another task.
func addSetJobPriorityTool(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "set_job_priority",
		Description: "Change a non-terminal job's priority in place (0 = P0, 1-9; overrides its task's priority; keeps job/task linkage). " +
			"A held job is re-evaluated at once: priority 0 dispatches immediately. reason is required and recorded on the task thread.",
	}, setJobPriorityTool(cfg))
}

func setJobPriorityTool(cfg Config) mcp.ToolHandlerFor[setJobPriorityArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in setJobPriorityArgs) (*mcp.CallToolResult, any, error) {
		if in.JobID == "" {
			return nil, nil, fmt.Errorf("job_id: required")
		}
		if in.Priority == nil || *in.Priority < 0 || *in.Priority > wire.MaxPriority {
			return nil, nil, fmt.Errorf("priority: required, 0-%d (0 = P0)", wire.MaxPriority)
		}
		reason := strings.TrimSpace(in.Reason)
		if reason == "" {
			return nil, nil, fmt.Errorf("reason: required")
		}
		old, ok, err := cfg.Store.SetPriority(ctx, in.JobID, *in.Priority)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("job %q is unknown or already terminal (see list_jobs)", in.JobID)
		}
		if cfg.JobPriorityChanged != nil {
			cfg.JobPriorityChanged(ctx, in.JobID, old, *in.Priority, reason)
		}
		job, _, err := cfg.Store.Get(ctx, in.JobID)
		if err != nil {
			return nil, nil, err
		}
		return structuredResult(map[string]any{
			"ok": true, "job_id": job.ID, "state": job.State,
			"priority": *in.Priority, "held_reason": job.HeldReason,
		}), nil, nil
	}
}
