package mcpfactory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/wire"
)

// Pinned constants (spec §9.4).
const (
	// defaultListJobsLimit is list_jobs' default limit.
	defaultListJobsLimit = 50
	// maxListJobsLimit caps list_jobs' limit.
	maxListJobsLimit = 200
)

// Argument sets for each tool (spec §9.4). Tools register with typed In
// parameters, so the SDK both advertises each tool's input schema and
// rejects unknown or missing arguments at the MCP layer: callers (the LLM
// planner) can introspect argument names, and a mis-typed call fails
// loudly instead of silently dropping arguments (2026-11-14: a
// message_foreman call passing `text` instead of `payload` dead-ended
// two operator feature requests). Hand-written validation stays in the
// wire/ store layers.
type (
	listForemenArgs struct{}
	listJobsArgs    struct {
		State string `json:"state,omitempty"`
		Limit *int   `json:"limit,omitempty"`
	}
	createJobArgs struct {
		Task         string   `json:"task"`
		Requires     []string `json:"requires,omitempty"`
		ResultSchema any      `json:"result_schema,omitempty"`
		Tier         string   `json:"tier,omitempty"`
		TimeoutS     *int     `json:"timeout_s,omitempty"`
		Priority     *int     `json:"priority,omitempty"`
		ScheduleCron string   `json:"schedule_cron,omitempty"`
	}
	readResultArgs struct {
		JobID string `json:"job_id"`
	}
	messageForemanArgs struct {
		ForemanID string `json:"foreman_id"`
		Type      string `json:"type"`
		Payload   any    `json:"payload"`
		JobID     string `json:"job_id,omitempty"`
	}
	notifyOperatorArgs struct {
		Text string `json:"text"`
	}
)

// structuredResult packs v into the tool's structured content.
func structuredResult(v any) *mcp.CallToolResult {
	return &mcp.CallToolResult{StructuredContent: v}
}

func listForemenTool(cfg Config) mcp.ToolHandlerFor[listForemenArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ listForemenArgs) (*mcp.CallToolResult, any, error) {
		foremen, err := cfg.Store.Foremen(ctx)
		if err != nil {
			return nil, nil, err
		}
		out := make([]map[string]any, 0, len(foremen))
		for _, f := range foremen {
			caps := json.RawMessage(f.Capabilities)
			if len(caps) == 0 {
				caps = json.RawMessage("{}")
			}
			out = append(out, map[string]any{
				"id":           f.ID,
				"online":       f.Online,
				"capabilities": caps,
			})
		}
		// Record-wrapped ({"foremen": [...]}): MCP structured content is
		// an object - a bare array is rejected by client harnesses.
		return structuredResult(map[string]any{"foremen": out}), nil, nil
	}
}

func listJobsTool(cfg Config) mcp.ToolHandlerFor[listJobsArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in listJobsArgs) (*mcp.CallToolResult, any, error) {
		limit := defaultListJobsLimit
		if in.Limit != nil {
			limit = *in.Limit
			if limit < 1 {
				limit = 1
			}
			if limit > maxListJobsLimit {
				limit = maxListJobsLimit
			}
		}
		listed, err := cfg.Store.List(ctx, in.State, "", limit)
		if err != nil {
			return nil, nil, err
		}
		// Summaries only (spec §6.6, §9.4): NEVER the results array or the
		// spec payload - full payloads enter LLM context only via read_result.
		out := make([]map[string]any, 0, len(listed))
		for _, j := range listed {
			var usage any
			if len(j.Usage) > 0 {
				usage = j.Usage
			}
			out = append(out, map[string]any{
				"id":          j.ID,
				"state":       j.State,
				"summary":     lastSummary(j.Results),
				"usage":       usage,
				"over_budget": j.OverBudget,
			})
		}
		// Record-wrapped ({"jobs": [...]}): MCP structured content is an
		// object - a bare array is rejected by client harnesses.
		return structuredResult(map[string]any{"jobs": out}), nil, nil
	}
}

// lastSummary extracts the LAST result's summary field from the job's
// results JSON array; empty string when there are no results.
func lastSummary(results json.RawMessage) string {
	var entries []struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(results, &entries); err != nil {
		return ""
	}
	if len(entries) == 0 {
		return ""
	}
	return entries[len(entries)-1].Summary
}

func createJobTool(cfg Config) mcp.ToolHandlerFor[createJobArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in createJobArgs) (*mcp.CallToolResult, any, error) {
		// Breaker first (spec §6.5): refuse with spend_cap_exceeded when the
		// daily spend breaker is open.
		if err := cfg.Store.RefuseIfSpendCap(ctx, cfg.Budget.DailySpendCapUSD, cfg.Budget.SpendOverride); err != nil {
			var cap *jobs.CapError
			if errors.As(err, &cap) {
				return nil, nil, fmt.Errorf("spend_cap_exceeded: %s", cap.Error())
			}
			return nil, nil, err
		}

		spec := wire.NewJobSpec(in.Task, "factory")
		spec.Requires = in.Requires
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
		if in.Priority != nil {
			spec.Priority = *in.Priority
		}
		if err := spec.Validate(); err != nil {
			return nil, nil, err
		}

		// Ad hoc dispatch via the Matcher (spec §6.7): no matching foreman
		// leaves the job pending.
		state, foreman := "pending", ""
		if cfg.Matcher != nil {
			f, ok, err := cfg.Matcher.Match(ctx, spec.Requires)
			if err != nil {
				return nil, nil, err
			}
			if ok {
				foreman = f.ID
				state = "dispatched"
			}
		}
		if err := cfg.Store.Create(ctx, spec); err != nil {
			return nil, nil, err
		}
		if state == "dispatched" {
			if err := cfg.Store.Dispatch(ctx, spec.ID, foreman); err != nil {
				return nil, nil, err
			}
		}

		// With schedule_cron ALSO create a schedule with the same template
		// (spec §9.4, §6.8), in addition to the one-off job above.
		if in.ScheduleCron != "" {
			template := spec
			template.ID = ""
			template.CreatedAt = ""
			if _, err := cfg.Store.CreateSchedule(ctx, in.ScheduleCron, template); err != nil {
				return nil, nil, fmt.Errorf("create schedule: %w", err)
			}
		}

		var foremanOut any
		if foreman != "" {
			foremanOut = foreman
		}
		return structuredResult(map[string]any{
			"job_id":  spec.ID,
			"state":   state,
			"foreman": foremanOut,
		}), nil, nil
	}
}

func readResultTool(cfg Config) mcp.ToolHandlerFor[readResultArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in readResultArgs) (*mcp.CallToolResult, any, error) {
		job, ok, err := cfg.Store.Get(ctx, in.JobID)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("unknown job %q", in.JobID)
		}
		// The ONLY path by which full payloads enter LLM context (spec
		// §9.4, §6.6): full results array plus spec.
		return structuredResult(map[string]any{
			"job_id":  job.ID,
			"state":   job.State,
			"results": job.Results,
			"spec":    job.Spec,
		}), nil, nil
	}
}

func messageForemanTool(cfg Config) mcp.ToolHandlerFor[messageForemanArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in messageForemanArgs) (*mcp.CallToolResult, any, error) {
		if in.ForemanID == "" {
			return nil, nil, fmt.Errorf("foreman_id: required")
		}
		// Never open a dead inbox: the foreman reference may arrive bare
		// ("server") or in the full form list_foremen reports
		// ("foreman:server") - both address the same inbox (spec §7.3) -
		// and an id matching no registered foreman is an error, not a
		// post to an inbox nothing reads (2026-11-14: double-prefixed ids
		// dead-ended two operator feature requests).
		rows, err := cfg.Store.Foremen(ctx)
		if err != nil {
			return nil, nil, err
		}
		known := false
		for _, f := range rows {
			if registry.SameForeman(f.ID, in.ForemanID) {
				known = true
				break
			}
		}
		if !known {
			return nil, nil, fmt.Errorf("unknown foreman %q: not registered (see list_foremen)", in.ForemanID)
		}
		// A missing body is a mis-typed call (e.g. `text` instead of
		// `payload`), not an empty message: reject it.
		if in.Payload == nil {
			return nil, nil, fmt.Errorf("payload: required (send {} for an empty body)")
		}
		payloadJSON, err := json.Marshal(in.Payload)
		if err != nil {
			return nil, nil, fmt.Errorf("payload: %w", err)
		}
		env := wire.MessageEnvelope{
			Type:    in.Type,
			JobID:   in.JobID,
			From:    auth.FactoryPlannerIdentity,
			Payload: payloadJSON,
		}
		if err := env.Validate(); err != nil {
			return nil, nil, err
		}
		posted, err := cfg.Inbox.Post(ctx, registry.InboxID(in.ForemanID), env)
		if err != nil {
			return nil, nil, err
		}
		return structuredResult(map[string]any{"ok": true, "id": posted.ID}), nil, nil
	}
}

// notifyOperatorTool serves the notify_operator tool (spec §9.4): posts the
// planner's reply into the operator inbox (planner:main, spec §9.5) as a
// brain.reply envelope. This is the ONLY way the LLM planner talks back -
// the conversational UI renders planner:main, so every reply lands in the
// operator's chat log (and streams to browsers via the SSE hub).
func notifyOperatorTool(cfg Config) mcp.ToolHandlerFor[notifyOperatorArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in notifyOperatorArgs) (*mcp.CallToolResult, any, error) {
		if in.Text == "" {
			return nil, nil, fmt.Errorf("text: required")
		}
		env := wire.MessageEnvelope{
			Type:    "brain.reply",
			From:    auth.FactoryBrainIdentity,
			Payload: json.RawMessage(`{"text":` + mustQuote(in.Text) + `}`),
		}
		posted, err := cfg.Inbox.Post(ctx, auth.PlannerMainInbox, env)
		if err != nil {
			return nil, nil, err
		}
		return structuredResult(map[string]any{"ok": true, "id": posted.ID}), nil, nil
	}
}

// mustQuote marshals s to a JSON string (always valid).
func mustQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
