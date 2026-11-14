package mcpforeman

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/wire"
)

// MaxWaitSeconds caps inbox_next's wait_seconds (spec §4.1 long-poll
// window).
const MaxWaitSeconds = 60

// inboxNextArgs is inbox_next's argument set (spec §9.3): wait_seconds
// is 0-60, default 0 (spec §11).
type inboxNextArgs struct {
	WaitSeconds *int `json:"wait_seconds,omitempty"`
}

// inboxAckArgs is inbox_ack's argument set (spec §9.3).
type inboxAckArgs struct {
	IDs []string `json:"ids"`
}

// postToFactoryArgs is post_to_factory's argument set (spec §9.3).
// job_id is required for job-scoped types - the envelope carries it
// (spec §3.1).
type postToFactoryArgs struct {
	Type    string `json:"type"`
	Payload any    `json:"payload,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}

// spawnWorkerArgs is spawn_worker's argument set (spec §9.3).
// result_schema must stay within the wire subset (spec §5.3); budget
// fields left at zero fall back to the pinned defaults (spec §5.1);
// tier picks the worker model (spec §6.5) - empty means flash, the
// cost-biased default.
type spawnWorkerArgs struct {
	Task         string `json:"task"`
	ResultSchema any    `json:"result_schema,omitempty"`
	Tier         string `json:"tier,omitempty"`
	TimeoutS     *int   `json:"timeout_s,omitempty"`
	Budget       any    `json:"budget,omitempty"`
}

// answerWorkerArgs is answer_worker's argument set (spec §9.3).
type answerWorkerArgs struct {
	JobID  string `json:"job_id"`
	Answer string `json:"answer"`
}

// inboxNextTool returns the oldest unacked envelope from the foreman's
// own inbox, long-polling up to wait_seconds; an empty inbox yields
// {message: null} (spec §9.3).
func inboxNextTool(cfg Config) mcp.ToolHandlerFor[inboxNextArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in inboxNextArgs) (*mcp.CallToolResult, any, error) {
		wait := 0
		if in.WaitSeconds != nil {
			wait = *in.WaitSeconds
		}
		if wait < 0 {
			wait = 0
		}
		if wait > MaxWaitSeconds {
			wait = MaxWaitSeconds
		}
		envs, err := cfg.Inbox.Get(ctx, cfg.ForemanInboxID, 1, time.Duration(wait)*time.Second)
		if err != nil {
			return nil, nil, err
		}
		if len(envs) == 0 {
			return structuredResult(map[string]any{"message": nil}), nil, nil
		}
		return structuredResult(map[string]any{"message": envs[0]}), nil, nil
	}
}

// inboxAckTool acks messages in the foreman's own inbox by id (spec
// §9.3).
func inboxAckTool(cfg Config) mcp.ToolHandlerFor[inboxAckArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in inboxAckArgs) (*mcp.CallToolResult, any, error) {
		if err := cfg.Inbox.Ack(ctx, cfg.ForemanInboxID, in.IDs); err != nil {
			return nil, nil, err
		}
		return structuredResult(map[string]any{"ok": true}), nil, nil
	}
}

// postToFactoryTool posts an envelope to the parent inbox (spec §9.3):
// from is the foreman's own inbox id and the server assigns the message
// id; job-scoped types must carry a job_id (spec §3.1).
func postToFactoryTool(cfg Config) mcp.ToolHandlerFor[postToFactoryArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in postToFactoryArgs) (*mcp.CallToolResult, any, error) {
		env := wire.MessageEnvelope{
			Type:  in.Type,
			JobID: in.JobID,
			From:  cfg.ForemanInboxID,
		}
		if in.Payload != nil {
			b, err := json.Marshal(in.Payload)
			if err != nil {
				return nil, nil, fmt.Errorf("payload: %w", err)
			}
			env.Payload = b
		}
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

// spawnWorkerTool creates a local JobSpec and enqueues it to the harness
// via the Spawn seam, returning the job id immediately (spec §9.3).
// created_by is the foreman identity (the pinned NewJobSpec second arg);
// Normalize refills budget/timeout fields left at zero before Validate
// runs (spec §5.1).
func spawnWorkerTool(cfg Config) mcp.ToolHandlerFor[spawnWorkerArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in spawnWorkerArgs) (*mcp.CallToolResult, any, error) {
		spec := wire.NewJobSpec(in.Task, cfg.ForemanInboxID)
		if in.ResultSchema != nil {
			b, err := json.Marshal(in.ResultSchema)
			if err != nil {
				return nil, nil, fmt.Errorf("result_schema: %w", err)
			}
			spec.ResultSchema = b
		}
		spec.Tier = in.Tier
		if in.TimeoutS != nil {
			spec.TimeoutS = *in.TimeoutS
		}
		if in.Budget != nil {
			b, err := json.Marshal(in.Budget)
			if err != nil {
				return nil, nil, fmt.Errorf("budget: %w", err)
			}
			var budget wire.Budget
			if err := json.Unmarshal(b, &budget); err != nil {
				return nil, nil, fmt.Errorf("budget: %w", err)
			}
			spec.Budget = budget
		}
		spec.Normalize()
		if err := spec.Validate(); err != nil {
			return nil, nil, err
		}
		if err := cfg.Spawn(ctx, spec); err != nil {
			return nil, nil, err
		}
		return structuredResult(map[string]any{"job_id": spec.ID}), nil, nil
	}
}

// answerWorkerTool posts a job.answer envelope to the per-job inbox
// job:{job_id}, which the worker long-polls via ask_parent (spec §3.2,
// §9.2, §9.3).
func answerWorkerTool(cfg Config) mcp.ToolHandlerFor[answerWorkerArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in answerWorkerArgs) (*mcp.CallToolResult, any, error) {
		payload, err := json.Marshal(map[string]string{"answer": in.Answer})
		if err != nil {
			return nil, nil, err
		}
		env := wire.MessageEnvelope{
			Type:    "job.answer",
			JobID:   in.JobID,
			From:    cfg.ForemanInboxID,
			Payload: payload,
		}
		if err := env.Validate(); err != nil {
			return nil, nil, err
		}
		if _, err := cfg.Inbox.Post(ctx, "job:"+in.JobID, env); err != nil {
			return nil, nil, err
		}
		return structuredResult(map[string]any{"ok": true}), nil, nil
	}
}

// structuredResult packs v into the tool's structured content.
// Package-local twin of mcpworker's (unexported there).
func structuredResult(v any) *mcp.CallToolResult {
	return &mcp.CallToolResult{StructuredContent: v}
}
