package mcpworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/wire"
)

// reportResultArgs is report_result's argument set (spec §9.2). Its JSON
// schema is advertised via reportResultSchema, which pins the object form
// of error as the contract. The tool is registered untyped, so the SDK does
// not enforce that schema on calls: Error is decoded as any and normalized
// by normalizeError, which accepts the lenient forms agents actually send.
// wire.JobResult.Validate still enforces the pinned rules afterwards.
type reportResultArgs struct {
	Status  string      `json:"status"`
	Summary string      `json:"summary"`
	Result  any         `json:"result,omitempty" jsonschema:"required if the job has a result_schema"`
	Error   any         `json:"error,omitempty"`
	Usage   *wire.Usage `json:"usage,omitempty"`
}

// reportResultDescription is kept short: it counts toward the §9.1 worker
// byte budget (1024 bytes across both tools). The input schema carries the
// field shapes.
const reportResultDescription = "Report the final result. failed needs error {kind,message}; kind: validation|timeout|crash|llm|other."

// reportResultSchema derives report_result's advertised input schema from
// reportResultArgs and pins the contract: error is the object form
// {kind, message} with kind one of the five pinned kinds (spec §9.2).
// Unknown top-level arguments are tolerated, as before, so the schema does
// not declare additionalProperties: false.
func reportResultSchema() *jsonschema.Schema {
	s, err := jsonschema.For[reportResultArgs](nil)
	if err != nil {
		panic(fmt.Errorf("report_result input schema: %w", err))
	}
	s.AdditionalProperties = nil
	s.Properties["status"] = &jsonschema.Schema{
		Type: "string",
		Enum: []any{wire.StatusDone, wire.StatusFailed},
	}
	s.Properties["error"] = &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"kind": {Type: "string", Enum: []any{
				string(wire.KindValidation), string(wire.KindTimeout), string(wire.KindCrash),
				string(wire.KindLLM), string(wire.KindOther),
			}},
			"message": {Type: "string"},
		},
		Required: []string{"kind", "message"},
	}
	s.Properties["usage"] = &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"input_tokens":  {Type: "integer"},
			"output_tokens": {Type: "integer"},
			"cost_usd":      {Type: "number"},
		},
	}
	return s
}

// errorKindAliases maps the kinds agents commonly send onto the pinned
// kinds. Keys are lowercase; lookups are case-insensitive (spec §9.2).
var errorKindAliases = map[string]wire.ErrorKind{
	"timeout":    wire.KindTimeout,
	"time":       wire.KindTimeout,
	"validation": wire.KindValidation,
	"invalid":    wire.KindValidation,
	"crash":      wire.KindCrash,
	"llm":        wire.KindLLM,
}

// normalizeError maps the raw error argument onto the pinned object form
// (spec §9.2 normalization). nil means no error was given. Rules:
//   - a JSON string becomes {kind: other, message: <string>};
//   - an object's kind is matched case-insensitively against the pinned
//     kinds and the aliases above; any other non-empty kind becomes other,
//     with the original kind preserved at the front of the message;
//   - any other JSON type is rejected.
func normalizeError(raw any) (*wire.JobError, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case string:
		return &wire.JobError{Kind: wire.KindOther, Message: v}, nil
	case map[string]any:
		orig := strings.TrimSpace(jsonText(v["kind"]))
		msg := jsonText(v["message"])
		if orig == "" {
			return &wire.JobError{Kind: wire.KindOther, Message: msg}, nil
		}
		lower := strings.ToLower(orig)
		if k := wire.ErrorKind(lower); k.Valid() {
			return &wire.JobError{Kind: k, Message: msg}, nil
		}
		if k, ok := errorKindAliases[lower]; ok {
			return &wire.JobError{Kind: k, Message: msg}, nil
		}
		if msg == "" {
			msg = fmt.Sprintf("original kind %q", orig)
		} else {
			msg = fmt.Sprintf("original kind %q: %s", orig, msg)
		}
		return &wire.JobError{Kind: wire.KindOther, Message: msg}, nil
	default:
		return nil, fmt.Errorf("must be an object {kind, message} or a string, got %T", raw)
	}
}

// jsonText renders a decoded JSON scalar as text: strings as-is, nil as
// empty, anything else as its JSON encoding.
func jsonText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// failResult reports err as a tool error (an MCP result with isError set),
// the same shape the SDK gives a typed handler's returned error.
func failResult(err error) (*mcp.CallToolResult, error) {
	var res mcp.CallToolResult
	res.SetError(err)
	return &res, nil
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

func reportResultTool(cfg Config) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cred, ok := credFromContext(ctx)
		if !ok {
			return failResult(errors.New("no credential in request context"))
		}
		var in reportResultArgs
		if err := decodeArgs(req, &in); err != nil {
			return failResult(fmt.Errorf("arguments: %w", err))
		}

		var result json.RawMessage
		if in.Result != nil {
			b, err := json.Marshal(in.Result)
			if err != nil {
				return failResult(fmt.Errorf("result: %w", err))
			}
			result = b
		}
		jobErr, err := normalizeError(in.Error)
		if err != nil {
			return failResult(fmt.Errorf("error: %w", err))
		}
		if in.Status == wire.StatusFailed && jobErr == nil {
			jobErr = &wire.JobError{Kind: wire.KindOther, Message: "status=failed with no error field"}
		}
		jr := wire.JobResult{
			JobID:   cred.jobID,
			Status:  in.Status,
			Summary: in.Summary,
			Result:  result,
			Error:   jobErr,
			Usage:   in.Usage,
		}
		if err := jr.Validate(nil); err != nil {
			return failResult(err)
		}

		payload, err := json.Marshal(jr)
		if err != nil {
			return failResult(err)
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
					return failResult(ferr)
				}
				return structuredResult(map[string]any{"ok": true, "duplicate": true}), nil
			}
			return failResult(err)
		}
		if err := writeResultFlag(cfg, cred.jobID); err != nil {
			return failResult(err)
		}
		return structuredResult(map[string]any{"ok": true}), nil
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
