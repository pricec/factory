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
// byte budget (1536 bytes across the worker tools). The input schema carries the
// field shapes.
const reportResultDescription = "Report the final result. failed needs error {kind,message}; kind: validation|timeout|crash|llm|other|superseded (duplicate/superseded job; never retried)."

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
				string(wire.KindLLM), string(wire.KindOther), string(wire.KindSuperseded),
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
	"superseded": wire.KindSuperseded,
	"duplicate":  wire.KindSuperseded,
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

// spawnChildArgs is spawn_child's argument set (spec §9.2, 2026-11-15
// sub-spawning amendment). result_schema must stay within the wire subset
// (§5.3); tier/timeout/budget left unset inherit the parent job's, and the
// parent's timeout and budget are hard caps (§9.2).
type spawnChildArgs struct {
	Task         string `json:"task"`
	ResultSchema any    `json:"result_schema,omitempty"`
	Tier         string `json:"tier,omitempty"`
	TimeoutS     *int   `json:"timeout_s,omitempty"`
	Budget       any    `json:"budget,omitempty"`
}

// collectChildrenArgs is collect_children's argument set (spec §9.2).
type collectChildrenArgs struct {
	WaitSeconds *int `json:"wait_seconds,omitempty"`
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

// spawnChildTool spawns ONE child worker job for the calling worker (spec
// §9.2, 2026-11-15 sub-spawning amendment): the child JobSpec carries
// parent_job_id, inherits the parent's tier, and is hard-capped at the
// parent's timeout and budget - a child never outlives or out-spends its
// parent. It is enqueued through the same Spawn seam the foreman brain's
// spawn_worker uses (§9.3), so the child is an ordinary foreman-spawned
// job: registry-visible via the accepted/started events (§6.2), loud on
// harness failure (§8.5 analog). Depth is hard-capped at one level: a job
// that is itself a child (its persisted spec carries parent_job_id) is
// refused. The parent collects the child's terminal job.result from its
// own per-job inbox via collect_children - never through the foreman
// brain.
func spawnChildTool(cfg Config) mcp.ToolHandlerFor[spawnChildArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in spawnChildArgs) (*mcp.CallToolResult, any, error) {
		cred, ok := credFromContext(ctx)
		if !ok {
			return nil, nil, errors.New("no credential in request context")
		}
		parent, ok, err := cfg.ParentSpec(cred.jobID)
		if err != nil {
			return nil, nil, fmt.Errorf("parent spec: %w", err)
		}
		if !ok {
			return nil, nil, fmt.Errorf("parent spec %s: not found", cred.jobID)
		}
		if parent.ParentJobID != "" {
			return nil, nil, fmt.Errorf("spawn depth: job %s is itself a spawned child (max 1 level of children)", cred.jobID)
		}

		spec := wire.NewJobSpec(in.Task, "worker:"+cred.jobID)
		if in.ResultSchema != nil {
			b, err := json.Marshal(in.ResultSchema)
			if err != nil {
				return nil, nil, fmt.Errorf("result_schema: %w", err)
			}
			spec.ResultSchema = b
		}
		// Tier: the caller's pick wins; unset inherits the parent's.
		spec.Tier = in.Tier
		if spec.Tier == "" {
			spec.Tier = parent.Tier
		}
		// Timeout: the caller's pick, hard-capped at the parent's.
		spec.TimeoutS = parent.TimeoutS
		if in.TimeoutS != nil && *in.TimeoutS < spec.TimeoutS {
			spec.TimeoutS = *in.TimeoutS
		}
		// Budget: per field, the caller's value hard-capped at the
		// parent's; unset fields inherit the parent's.
		spec.Budget = parent.Budget
		if in.Budget != nil {
			b, err := json.Marshal(in.Budget)
			if err != nil {
				return nil, nil, fmt.Errorf("budget: %w", err)
			}
			var budget wire.Budget
			if err := json.Unmarshal(b, &budget); err != nil {
				return nil, nil, fmt.Errorf("budget: %w", err)
			}
			if budget.MaxPromptBytes > 0 {
				spec.Budget.MaxPromptBytes = min(budget.MaxPromptBytes, parent.Budget.MaxPromptBytes)
			}
			if budget.MaxOutputTokens > 0 {
				spec.Budget.MaxOutputTokens = min(budget.MaxOutputTokens, parent.Budget.MaxOutputTokens)
			}
			if budget.MaxCostUSD > 0 {
				spec.Budget.MaxCostUSD = min(budget.MaxCostUSD, parent.Budget.MaxCostUSD)
			}
		}
		spec.ParentJobID = cred.jobID
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

// collectChildrenNoMatchPace paces the collect_children long-poll loop
// when a Get batch yields no collectable result: uncollectable envelopes
// (job.answer, other jobs' stale copies) make Get return immediately, and
// without a pause the loop would spin until the deadline.
const collectChildrenNoMatchPace = 200 * time.Millisecond

// collectChildrenTool long-polls the caller's per-job inbox for terminal
// job.result copies of the children it spawned via spawn_child (spec §9.2,
// 2026-11-15 sub-spawning amendment): the foreman copies each child's
// terminal result into job:{parent_job_id} - the §8.5 synthesized
// spawn-failure results included - so the parent collects child results
// itself, never through a foreman-brain relay. Only the current attempt's
// children are returned (the foreman owns the spawn state); returned
// copies are acked. Returns as soon as at least one result is pending, or
// with {results: []} once wait_seconds elapses.
func collectChildrenTool(cfg Config) mcp.ToolHandlerFor[collectChildrenArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in collectChildrenArgs) (*mcp.CallToolResult, any, error) {
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
		children := map[string]bool{}
		for _, id := range cfg.ChildJobs(cred.jobID) {
			children[id] = true
		}

		jobInbox := "job:" + cred.jobID
		deadline := cfg.now().Add(time.Duration(wait) * time.Second)
		results := make([]wire.JobResult, 0, 4) // never null for the caller
		for {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return structuredResult(map[string]any{"results": results}), nil, nil
			}
			envs, err := cfg.Inbox.Get(ctx, jobInbox, 50, remaining)
			if err != nil {
				return nil, nil, err // transport failure only
			}
			matched := false
			for _, env := range envs {
				if env.Type != "job.result" {
					continue // job.answer stays for ask_parent
				}
				var jr wire.JobResult
				if uerr := json.Unmarshal(env.Payload, &jr); uerr != nil || jr.JobID != env.JobID {
					continue // not a child-result copy; leave it
				}
				if !children[env.JobID] {
					continue // not this attempt's child; leave it
				}
				// Consume the copy so a later collect never sees it again.
				if aerr := cfg.Inbox.Ack(ctx, jobInbox, []string{env.ID}); aerr != nil {
					return nil, nil, aerr
				}
				results = append(results, jr)
				matched = true
			}
			if matched {
				return structuredResult(map[string]any{"results": results}), nil, nil
			}
			// The batch held only uncollectable envelopes: Get would
			// return them again immediately. Pace before re-polling.
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(collectChildrenNoMatchPace):
			}
		}
	}
}
