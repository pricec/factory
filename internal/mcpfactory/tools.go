package mcpfactory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/tasks"
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
		Task     string   `json:"task"`
		Requires []string `json:"requires,omitempty"`
		// Secrets are credentials-store NAMES (spec §13.2), never values.
		Secrets []string `json:"secrets,omitempty"`
		// Touches is the declared file footprint (spec §6.10 conflict
		// coordinator): repo-relative path globs.
		Touches []string `json:"touches,omitempty"`
		// Foreman is the explicit routing pin (spec §6.7): bare or
		// foreman:-prefixed id of the foreman the job runs on; empty
		// means the Matcher's capability pick.
		Foreman      string `json:"foreman,omitempty"`
		ResultSchema any    `json:"result_schema,omitempty"`
		Tier         string `json:"tier,omitempty"`
		Model        string `json:"model,omitempty"`
		TimeoutS     *int   `json:"timeout_s,omitempty"`
		Priority     *int   `json:"priority,omitempty"`
		ScheduleCron string `json:"schedule_cron,omitempty"`
		TaskID       string `json:"task_id,omitempty"`
	}
	readResultArgs struct {
		JobID string `json:"job_id"`
	}
	cancelJobArgs struct {
		JobID     string `json:"job_id"`
		Reason    string `json:"reason,omitempty"`
		Supersede bool   `json:"supersede,omitempty"`
	}
	messageForemanArgs struct {
		ForemanID string `json:"foreman_id"`
		Type      string `json:"type"`
		Payload   any    `json:"payload"`
		JobID     string `json:"job_id,omitempty"`
	}
	notifyOperatorArgs struct {
		Text   string `json:"text"`
		TaskID string `json:"task_id,omitempty"`
		// Spoken is an optional 1-3 sentence plain-text rendering of the
		// reply for text-to-speech (spec §6.15); trimmed and capped here.
		Spoken string `json:"spoken,omitempty"`
	}
)

// MaxSpokenChars caps a brain.reply `spoken` field (runes).
const MaxSpokenChars = 300

// normSpoken trims s, collapses whitespace and truncates to MaxSpokenChars
// runes (on a word boundary where one exists, with an ellipsis).
func normSpoken(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= MaxSpokenChars {
		return s
	}
	cut := string(r[:MaxSpokenChars-1])
	if i := strings.LastIndex(cut, " "); i > MaxSpokenChars/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-") + "…"
}

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
		hosted := make([]registry.HostedForeman, 0, len(foremen))
		for _, f := range foremen {
			hosted = append(hosted, registry.HostedForeman{ID: f.ID, Host: f.Host})
			caps := json.RawMessage(f.Capabilities)
			if len(caps) == 0 {
				caps = json.RawMessage("{}")
			}
			entry := map[string]any{
				"id":           f.ID,
				"online":       f.Online,
				"capabilities": caps,
			}
			// host is absent for foremen that never reported one (older
			// builds); it is descriptive, never matched on.
			if f.Host != "" && f.Host != "null" {
				entry["host"] = json.RawMessage(f.Host)
			}
			out = append(out, entry)
		}
		// Record-wrapped ({"foremen": [...]}): MCP structured content is
		// an object - a bare array is rejected by client harnesses.
		// hosts groups foremen sharing a machine (same host_id).
		return structuredResult(map[string]any{"foremen": out, "hosts": registry.GroupHosts(hosted)}), nil, nil
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
			row := map[string]any{
				"id":          j.ID,
				"state":       j.State,
				"summary":     lastSummary(j.Results),
				"usage":       usage,
				"over_budget": j.OverBudget,
			}
			// S3 dispatch gate: a held job says why it waits.
			if j.HeldReason != "" {
				row["held_reason"] = j.HeldReason
			}
			out = append(out, row)
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
	return func(ctx context.Context, req *mcp.CallToolRequest, in createJobArgs) (*mcp.CallToolResult, any, error) {
		// Task filing (spec §9.5.2): task_id, else the woken task.
		taskID, err := jobTask(ctx, cfg, req, in.TaskID)
		if err != nil {
			return nil, nil, err
		}

		spec := wire.NewJobSpec(in.Task, "factory")
		spec.Requires = in.Requires
		spec.Secrets = in.Secrets
		spec.Touches = in.Touches
		if in.ResultSchema != nil {
			b, err := wire.MarshalPayload(in.ResultSchema)
			if err != nil {
				return nil, nil, fmt.Errorf("result_schema: %w", err)
			}
			spec.ResultSchema = b
		}
		spec.Tier = in.Tier
		spec.Model = in.Model
		if in.TimeoutS != nil {
			spec.TimeoutS = *in.TimeoutS
		}
		if in.Priority != nil {
			spec.Priority = *in.Priority
		}
		// The explicit routing pin (spec §6.7, 2026-11-16 foreman-pin
		// amendment): normalized to the full foreman:{id} form, it
		// replaces the Matcher's pick - the planner chose the foreman,
		// so no capability fallback applies.
		if in.Foreman != "" {
			spec.Foreman = registry.InboxID(in.Foreman)
		}
		if err := spec.Validate(); err != nil {
			return nil, nil, err
		}
		// Factory-repo work must be pinned to foreman:server (factory_repo.go).
		routeWarning, err := cfg.checkFactoryRouting(in.Task, spec.Foreman)
		if err != nil {
			return nil, nil, err
		}
		// Fail at create, never at dispatch (spec §13.2).
		if len(spec.Secrets) > 0 {
			if cfg.Secrets == nil {
				return nil, nil, fmt.Errorf("unknown_secrets: %v (no credentials store configured)", spec.Secrets)
			}
			if err := cfg.Secrets.CheckNames(spec.Secrets); err != nil {
				return nil, nil, err
			}
		}

		// Dispatch decision (spec §6.7): the pin wins over the Matcher.
		// A pinned job also skips creation-time requires validation -
		// requires stay informational on its spec, and their tag-name
		// rules would reject a job whose routing never consults them.
		state, foreman := "pending", ""
		var warnings []string
		if routeWarning != "" {
			warnings = append(warnings, routeWarning)
		}
		if spec.Foreman != "" {
			pin, found, err := jobs.FindForeman(ctx, cfg.Store, spec.Foreman)
			if err != nil {
				return nil, nil, err
			}
			if !found {
				return nil, nil, fmt.Errorf("unknown foreman %q: not registered (list_foremen lists registered foremen)", in.Foreman)
			}
			if pin.Online {
				foreman, state = pin.ID, "dispatched"
			} else {
				// Loud, non-fatal: the pin is honored, the job just
				// waits - the pending picker dispatches it when the
				// foreman returns (§6.7).
				warnings = append(warnings, "pinned foreman "+spec.Foreman+" is offline; the job pends until it returns")
			}
		} else {
			// Creation-time requires validation (spec §6.7, 2026-11-15
			// amendment): entries must be tag names - bare or name:value -
			// never capability values; an entry naming no registered tag key
			// can never match, and the job would park pending silently.
			w, err := cfg.Store.ValidateRequires(ctx, spec.Requires)
			if err != nil {
				return nil, nil, err
			}
			warnings = append(warnings, w...)
			// Ad hoc dispatch via the Matcher (spec §6.7): no matching foreman
			// leaves the job pending.
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
		}
		// Dispatch gate (S3 token throttle, spec §6.10): a held job is
		// created at once in state held - the id is returned now and the
		// releaser dispatches it when usage allows.
		hold, heldReason := cfg.Store.Admit(ctx, spec, taskID)
		if hold {
			state, foreman = jobs.StateHeld, ""
			if err := cfg.Store.CreateHeld(ctx, spec, heldReason); err != nil {
				return nil, nil, err
			}
		} else if err := cfg.Store.Create(ctx, spec); err != nil {
			return nil, nil, err
		}
		// Link before dispatch: the job's first report must already
		// resolve to its task (spec §9.5.2 relay stamping).
		if taskID != "" {
			if err := cfg.Tasks.LinkJob(ctx, taskID, spec.ID); err != nil {
				return nil, nil, fmt.Errorf("link job to task %s: %w", taskID, err)
			}
			cfg.taskChanged(ctx, taskID)
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
		out := map[string]any{
			"job_id":  spec.ID,
			"state":   state,
			"foreman": foremanOut,
		}
		if taskID != "" {
			out["task_id"] = taskID
		}
		if hold {
			out["held_reason"] = heldReason
		}
		// Loud, non-fatal warnings (spec §6.7): e.g. requires could not be
		// validated because no foremen are registered yet.
		if len(warnings) > 0 {
			out["warnings"] = warnings
		}
		return structuredResult(out), nil, nil
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

// cancelJobTool serves the cancel_job tool (spec §9.4, 2026-11-15
// amendment): terminal-cancels a non-terminal job through the jobs state
// machine (pending/dispatched/running/failed -> cancelled, or superseded
// with supersede=true) - the planner's alternative to direct jobs.db
// writes. The result surfaces the resulting state; terminal jobs and
// unknown ids are tool errors.
func cancelJobTool(cfg Config) mcp.ToolHandlerFor[cancelJobArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in cancelJobArgs) (*mcp.CallToolResult, any, error) {
		if in.JobID == "" {
			return nil, nil, fmt.Errorf("job_id: required")
		}
		to := jobs.StateCancelled
		if in.Supersede {
			to = jobs.StateSuperseded
		}
		undo := func() {}
		if cfg.MarkSelfCancel != nil {
			undo = cfg.MarkSelfCancel(in.JobID)
		}
		job, err := cfg.Store.Cancel(ctx, in.JobID, to, in.Reason)
		if err != nil {
			undo()
			if errors.Is(err, jobs.ErrNoSuchJob) {
				return nil, nil, fmt.Errorf("unknown job %q (see list_jobs)", in.JobID)
			}
			return nil, nil, err
		}
		return structuredResult(map[string]any{
			"ok":     true,
			"job_id": job.ID,
			"state":  job.State,
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
		payloadJSON, err := wire.MarshalPayload(in.Payload)
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

// notifyOperatorTool serves the notify_operator tool (spec §9.4, §9.5.2):
// posts the planner's reply into the operator inbox (planner:main) as a
// brain.reply envelope filed under a task: task_id, else the woken task,
// else (no task at all) a NEW task titled from the text, so the reply is
// always visible in the task UI. An unknown task_id is a tool error. This
// is the ONLY way the LLM planner talks back. Without a task store
// (Config.Tasks nil) it posts unthreaded, as before tasks.
func notifyOperatorTool(cfg Config) mcp.ToolHandlerFor[notifyOperatorArgs, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in notifyOperatorArgs) (*mcp.CallToolResult, any, error) {
		if in.Text == "" {
			return nil, nil, fmt.Errorf("text: required")
		}
		taskID := ""
		if cfg.Tasks != nil {
			var err error
			taskID, err = resolveTask(ctx, cfg, req, in.TaskID)
			if err != nil {
				return nil, nil, err
			}
			if taskID == "" {
				t, err := cfg.Tasks.CreateTask(ctx, titleFrom(in.Text), tasks.DefaultPriority)
				if err != nil {
					return nil, nil, err
				}
				taskID = t.ID
			}
		}
		env := wire.MessageEnvelope{
			Type:     "brain.reply",
			From:     auth.FactoryBrainIdentity,
			Payload:  replyPayload(in.Text, in.Spoken),
			ThreadID: taskID,
		}
		posted, err := cfg.Inbox.Post(ctx, auth.PlannerMainInbox, env)
		if err != nil {
			return nil, nil, err
		}
		out := map[string]any{"ok": true, "id": posted.ID}
		if taskID != "" {
			out["task_id"] = taskID
			cfg.taskChanged(ctx, taskID)
		}
		return structuredResult(out), nil, nil
	}
}

// replyPayload builds the brain.reply payload; `spoken` is omitted when
// empty so old consumers and envelopes are unaffected.
func replyPayload(text, spoken string) json.RawMessage {
	out := `{"text":` + mustQuote(text)
	if sp := normSpoken(spoken); sp != "" {
		out += `,"spoken":` + mustQuote(sp)
	}
	return json.RawMessage(out + `}`)
}

// mustQuote marshals s to a JSON string (always valid).
func mustQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
