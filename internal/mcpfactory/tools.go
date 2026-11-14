package mcpfactory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// Pinned constants (spec §9.4).
const (
	// defaultListJobsLimit is list_jobs' default limit.
	defaultListJobsLimit = 50
	// maxListJobsLimit caps list_jobs' limit.
	maxListJobsLimit = 200
)

// Argument sets for each tool (spec §9.4). Unmarshaled by hand (In = any)
// to keep the advertised input schema inside the description byte budget
// (spec §9.1); validation runs via the wire/ store layers.
type (
	listJobsArgs struct {
		State string `json:"state,omitempty"`
		Limit *int   `json:"limit,omitempty"`
	}
	createJobArgs struct {
		Task         string          `json:"task"`
		Requires     []string        `json:"requires,omitempty"`
		ResultSchema json.RawMessage `json:"result_schema,omitempty"`
		TimeoutS     *int            `json:"timeout_s,omitempty"`
		Priority     *int            `json:"priority,omitempty"`
		ScheduleCron string          `json:"schedule_cron,omitempty"`
	}
	readResultArgs struct {
		JobID string `json:"job_id"`
	}
	messageForemanArgs struct {
		ForemanID string          `json:"foreman_id"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload,omitempty"`
		JobID     string          `json:"job_id,omitempty"`
	}
)

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

// structuredResult packs v into the tool's structured content.
func structuredResult(v any) *mcp.CallToolResult {
	return &mcp.CallToolResult{StructuredContent: v}
}

func listForemenTool(cfg Config) mcp.ToolHandlerFor[any, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
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
		return structuredResult(out), nil, nil
	}
}

func listJobsTool(cfg Config) mcp.ToolHandlerFor[any, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		var in listJobsArgs
		if err := decodeArgs(req, &in); err != nil {
			return nil, nil, fmt.Errorf("arguments: %w", err)
		}
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
		return structuredResult(out), nil, nil
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

func createJobTool(cfg Config) mcp.ToolHandlerFor[any, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		var in createJobArgs
		if err := decodeArgs(req, &in); err != nil {
			return nil, nil, fmt.Errorf("arguments: %w", err)
		}

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
		spec.ResultSchema = in.ResultSchema
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

func readResultTool(cfg Config) mcp.ToolHandlerFor[any, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		var in readResultArgs
		if err := decodeArgs(req, &in); err != nil {
			return nil, nil, fmt.Errorf("arguments: %w", err)
		}
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

func messageForemanTool(cfg Config) mcp.ToolHandlerFor[any, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		var in messageForemanArgs
		if err := decodeArgs(req, &in); err != nil {
			return nil, nil, fmt.Errorf("arguments: %w", err)
		}
		if in.ForemanID == "" {
			return nil, nil, fmt.Errorf("foreman_id: required")
		}
		env := wire.MessageEnvelope{
			Type:    in.Type,
			JobID:   in.JobID,
			From:    auth.FactoryPlannerIdentity,
			Payload: in.Payload,
		}
		if err := env.Validate(); err != nil {
			return nil, nil, err
		}
		posted, err := cfg.Inbox.Post(ctx, "foreman:"+in.ForemanID, env)
		if err != nil {
			return nil, nil, err
		}
		return structuredResult(map[string]any{"ok": true, "id": posted.ID}), nil, nil
	}
}
