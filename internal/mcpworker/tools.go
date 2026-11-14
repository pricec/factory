package mcpworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/wire"
)

// reportResultArgs is report_result's argument set (spec §9.2). Unmarshaled
// by hand (In = any) to keep the advertised input schema inside the
// description byte budget (spec §9.1): validation runs via wire.Validate.
type reportResultArgs struct {
	Status  string         `json:"status"`
	Summary string         `json:"summary"`
	Result  any            `json:"result,omitempty"`
	Error   *wire.JobError `json:"error,omitempty"`
	Usage   *wire.Usage    `json:"usage,omitempty"`
}

// askParentArgs is ask_parent's argument set (spec §9.2).
type askParentArgs struct {
	Question    string `json:"question"`
	WaitSeconds *int   `json:"wait_seconds,omitempty"`
}

// decodeArgs round-trips the tool's untyped JSON arguments into dst.
func decodeArgs(req *mcp.CallToolRequest, dst any) error {
	if req.Params.Arguments == nil {
		return nil
	}
	b, err := json.Marshal(req.Params.Arguments)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func reportResultTool(cfg Config) mcp.ToolHandlerFor[any, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		cred, ok := credFromContext(ctx)
		if !ok {
			return nil, nil, errors.New("no credential in request context")
		}
		var in reportResultArgs
		if err := decodeArgs(req, &in); err != nil {
			return nil, nil, fmt.Errorf("arguments: %w", err)
		}

		var result json.RawMessage
		if in.Result != nil {
			b, err := json.Marshal(in.Result)
			if err != nil {
				return nil, nil, fmt.Errorf("result: %w", err)
			}
			result = b
		}
		jr := wire.JobResult{
			JobID:   cred.jobID,
			Status:  in.Status,
			Summary: in.Summary,
			Result:  result,
			Error:   in.Error,
			Usage:   in.Usage,
		}
		if err := jr.Validate(nil); err != nil {
			return nil, nil, err
		}

		payload, err := json.Marshal(jr)
		if err != nil {
			return nil, nil, err
		}
		env := wire.MessageEnvelope{
			Type:    "job.result",
			JobID:   cred.jobID,
			From:    "worker:" + cred.jobID,
			Payload: payload,
		}
		if _, err := cfg.Inbox.Post(ctx, cfg.ForemanInboxID, env); err != nil {
			var dup *inbox.DuplicateError
			if errors.As(err, &dup) {
				// Idempotent success (spec §4.2): already reported. Still
				// ensure the flag file exists (the first report may have
				// come from the harness fallback, which never wrote it).
				if ferr := writeResultFlag(cfg, cred.jobID); ferr != nil {
					return nil, nil, ferr
				}
				return structuredResult(map[string]any{"ok": true, "duplicate": true}), nil, nil
			}
			return nil, nil, err
		}
		if err := writeResultFlag(cfg, cred.jobID); err != nil {
			return nil, nil, err
		}
		return structuredResult(map[string]any{"ok": true}), nil, nil
	}
}

func askParentTool(cfg Config) mcp.ToolHandlerFor[askParentArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in askParentArgs) (*mcp.CallToolResult, any, error) {
		cred, ok := credFromContext(ctx)
		if !ok {
			return nil, nil, errors.New("no credential in request context")
		}

		wait := DefaultWaitSeconds
		if in.WaitSeconds != nil {
			wait = *in.WaitSeconds
		}
		if wait < 0 {
			wait = 0
		}
		if wait > MaxWaitSeconds {
			wait = MaxWaitSeconds
		}

		payload, err := json.Marshal(map[string]string{"question": in.Question})
		if err != nil {
			return nil, nil, err
		}
		env := wire.MessageEnvelope{
			Type:    "job.question",
			JobID:   cred.jobID,
			From:    "worker:" + cred.jobID,
			Payload: payload,
		}
		if _, err := cfg.Inbox.Post(ctx, cfg.ForemanInboxID, env); err != nil {
			return nil, nil, err
		}

		// Long-poll the per-job inbox for job.answer (spec §3.2, §9.2).
		// Answers arrive from a different component; the inbox Get wait IS
		// the wait mechanism. Get may return empty before the deadline (its
		// wait is capped at MaxWaitSeconds, and spurious wakeups can return
		// early), so loop until the deadline.
		jobInbox := "job:" + cred.jobID
		deadline := cfg.now().Add(time.Duration(wait) * time.Second)
		for {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return structuredResult(map[string]any{"answer": DefaultAnswer}), nil, nil
			}
			envs, err := cfg.Inbox.Get(ctx, jobInbox, 50, remaining)
			if err != nil {
				return nil, nil, err // transport failure only
			}
			for _, env := range envs {
				if env.Type != "job.answer" {
					continue
				}
				var p struct {
					Answer string `json:"answer"`
				}
				if uerr := json.Unmarshal(env.Payload, &p); uerr != nil {
					continue
				}
				// Consume it so later questions never see a stale answer.
				if aerr := cfg.Inbox.Ack(ctx, jobInbox, []string{env.ID}); aerr != nil {
					return nil, nil, aerr
				}
				return structuredResult(map[string]any{"answer": p.Answer}), nil, nil
			}
		}
	}
}

// structuredResult packs v into the tool's structured content.
func structuredResult(v any) *mcp.CallToolResult {
	return &mcp.CallToolResult{StructuredContent: v}
}
