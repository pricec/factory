package mcpfactory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pricec/factory/internal/llmctx"
	"github.com/pricec/factory/internal/tasks"
)

// WokenTaskHeader carries the planner session's woken task (spec §9.5.2).
// The factory binds each planner session to its task by appending
// ?task_id=<id> to the session's MCP URL; plannerAuth copies that query
// parameter into this header, which the MCP SDK hands every tool call
// (RequestExtra.Header). A client may also send the header directly.
const WokenTaskHeader = "X-Factory-Task-Id"

// Task tool constants (spec §9.5.2).
const (
	defaultListTasksLimit = 50
	maxListTasksLimit     = 200
	// getTaskThreadCap: get_task returns the task's last 40 messages, the
	// same window a wake replays.
	getTaskThreadCap = 40
	// get_task payload caps (get_message returns any message in full).
	threadInternalCap = 4 << 10
	threadConvCap     = 16 << 10
	// titleMax bounds a fallback task title (first 60 characters).
	titleMax = 60
)

type (
	listTasksArgs struct {
		Status string `json:"status,omitempty"`
		Q      string `json:"q,omitempty"`
		Limit  *int   `json:"limit,omitempty"`
	}
	getTaskArgs struct {
		TaskID string `json:"task_id"`
	}
	updateTaskArgs struct {
		TaskID   string  `json:"task_id"`
		Title    *string `json:"title,omitempty"`
		Priority *int    `json:"priority,omitempty"`
		Status   string  `json:"status,omitempty"`
		Reason   string  `json:"reason,omitempty"`
		// NeedsOperator sets (true) or clears (false) the task's
		// needs_operator flag; omitted leaves it alone.
		NeedsOperator *bool `json:"needs_operator,omitempty"`
	}
	setPriorityCriteriaArgs struct {
		Text string `json:"text"`
	}
)

// addTaskTools registers the planner's task tools (spec §9.5.2). Skipped
// without a task store.
func addTaskTools(srv *mcp.Server, cfg Config) {
	if cfg.Tasks == nil {
		return
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_tasks",
		Description: "List tasks, ranked: {tasks}. status open|closed|all (default open); q matches title/id/date.",
	}, listTasksTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_task",
		Description: "One task plus its thread (last 40 messages; long payloads cut).",
	}, getTaskTool(cfg))
	addMessageTool(srv, cfg)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "update_task",
		Description: "Retitle, reprioritise (0 = most urgent), set status open|closed (+reason), or set needs_operator " +
			"(true while blocked on an operator decision/approval; operator reply/close clears it).",
	}, updateTaskTool(cfg))
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_priority_criteria",
		Description: "Store the operator's task-ranking criteria (shown on every wake).",
	}, setPriorityCriteriaTool(cfg))
}

// taskChanged notifies Config.TaskChanged, when set.
func (cfg Config) taskChanged(ctx context.Context, taskID string) {
	if cfg.TaskChanged != nil && taskID != "" {
		cfg.TaskChanged(ctx, taskID)
	}
}

// wokenTask returns the session's woken task id ("" when none).
func wokenTask(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return strings.TrimSpace(req.Extra.Header.Get(WokenTaskHeader))
}

// resolveTask picks explicit, else the woken task, and checks it exists.
// "" (no error) when neither is set.
func resolveTask(ctx context.Context, cfg Config, req *mcp.CallToolRequest, explicit string) (string, error) {
	id := strings.TrimSpace(explicit)
	if id == "" {
		id = wokenTask(req)
	}
	if id == "" {
		return "", nil
	}
	if _, err := cfg.Tasks.GetTask(ctx, id); err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return "", fmt.Errorf("unknown task %q (see list_tasks)", id)
		}
		return "", err
	}
	return id, nil
}

// jobTask resolves create_job's task: task_id, else the woken task; ""
// without a task store or when neither is set.
func jobTask(ctx context.Context, cfg Config, req *mcp.CallToolRequest, explicit string) (string, error) {
	if cfg.Tasks == nil {
		return "", nil
	}
	return resolveTask(ctx, cfg, req, explicit)
}

// titleFrom derives a fallback task title: whitespace collapsed, the
// first titleMax runes.
func titleFrom(text string) string {
	t := strings.Join(strings.FieldsFunc(text, unicode.IsSpace), " ")
	if r := []rune(t); len(r) > titleMax {
		t = strings.TrimSpace(string(r[:titleMax]))
	}
	if t == "" {
		t = "(untitled)"
	}
	return t
}

func listTasksTool(cfg Config) mcp.ToolHandlerFor[listTasksArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in listTasksArgs) (*mcp.CallToolResult, any, error) {
		status := in.Status
		switch status {
		case "":
			status = tasks.StatusOpen
		case "all":
			status = ""
		case tasks.StatusOpen, tasks.StatusClosed:
		default:
			return nil, nil, fmt.Errorf("status: want open, closed or all, got %q", in.Status)
		}
		limit := defaultListTasksLimit
		if in.Limit != nil {
			limit = min(max(*in.Limit, 1), maxListTasksLimit)
		}
		list, err := cfg.Tasks.ListTasks(ctx, tasks.TaskFilter{Status: status, Query: in.Q, Limit: limit})
		if err != nil {
			return nil, nil, err
		}
		if list == nil {
			list = []tasks.Task{}
		}
		return structuredResult(map[string]any{"tasks": list}), nil, nil
	}
}

func getTaskTool(cfg Config) mcp.ToolHandlerFor[getTaskArgs, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in getTaskArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.TaskID) == "" {
			return nil, nil, fmt.Errorf("task_id: required")
		}
		t, err := cfg.Tasks.GetTask(ctx, in.TaskID)
		if errors.Is(err, tasks.ErrNotFound) {
			return nil, nil, fmt.Errorf("unknown task %q (see list_tasks)", in.TaskID)
		}
		if err != nil {
			return nil, nil, err
		}
		thread, err := cfg.Inbox.ThreadMessages(ctx, t.ID, getTaskThreadCap)
		if err != nil {
			return nil, nil, err
		}
		// Payloads enter LLM context: internal rows are capped at
		// threadInternalCap, conversational rows at threadConvCap, each
		// marker naming the message id (recover with get_message), and
		// rendered as a string so a cut never yields invalid JSON.
		msgs := make([]map[string]any, 0, len(thread))
		for _, env := range thread {
			limit := threadInternalCap
			if env.Type == "chat.message" || env.Type == "brain.reply" {
				limit = threadConvCap
			}
			msgs = append(msgs, map[string]any{
				"id":       env.ID,
				"type":     env.Type,
				"from":     env.From,
				"job_id":   env.JobID,
				"ts":       env.TS,
				"internal": tasks.IsInternal(env.Type),
				"payload":  string(llmctx.CapLimit(compactJSON(env.Payload), limit, env.ID)),
			})
		}
		return structuredResult(map[string]any{"task": t, "messages": msgs}), nil, nil
	}
}

// compactJSON strips insignificant whitespace from valid JSON.
func compactJSON(b []byte) []byte {
	var out strings.Builder
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return b
	}
	return []byte(strings.TrimSuffix(out.String(), "\n"))
}

func updateTaskTool(cfg Config) mcp.ToolHandlerFor[updateTaskArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in updateTaskArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.TaskID) == "" {
			return nil, nil, fmt.Errorf("task_id: required")
		}
		var u tasks.TaskUpdate
		if in.Title != nil {
			title := strings.TrimSpace(*in.Title)
			if title == "" {
				return nil, nil, fmt.Errorf("title: must not be empty")
			}
			u.Title = &title
		}
		if in.Priority != nil {
			if *in.Priority < 0 {
				return nil, nil, fmt.Errorf("priority: must be >= 0 (0 = most urgent)")
			}
			u.Priority = in.Priority
		}
		switch in.Status {
		case "":
		case tasks.StatusOpen, tasks.StatusClosed:
			status := in.Status
			u.Status = &status
		default:
			return nil, nil, fmt.Errorf("status: want open or closed, got %q", in.Status)
		}
		// reason rides a close (default "planner"); on a reopen it is
		// ignored (reopening clears close_reason).
		if in.Status == tasks.StatusClosed || (in.Status == "" && in.Reason != "") {
			reason := in.Reason
			if reason == "" {
				reason = "planner"
			}
			u.CloseReason = &reason
		}
		u.NeedsOperator = in.NeedsOperator
		t, err := cfg.Tasks.UpdateTask(ctx, in.TaskID, u)
		if errors.Is(err, tasks.ErrNotFound) {
			return nil, nil, fmt.Errorf("unknown task %q (see list_tasks)", in.TaskID)
		}
		if err != nil {
			return nil, nil, err
		}
		cfg.taskChanged(ctx, t.ID)
		return structuredResult(map[string]any{"task": t}), nil, nil
	}
}

func setPriorityCriteriaTool(cfg Config) mcp.ToolHandlerFor[setPriorityCriteriaArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in setPriorityCriteriaArgs) (*mcp.CallToolResult, any, error) {
		if err := cfg.Tasks.SetPriorityCriteria(ctx, in.Text); err != nil {
			return nil, nil, err
		}
		return structuredResult(map[string]any{"ok": true}), nil, nil
	}
}
