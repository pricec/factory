package factoryapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pricec/factory/internal/claudecli"
	"github.com/pricec/factory/internal/llmctx"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

// Planner brain session constants (spec §9.4 harness pattern, §9.5).
const (
	// plannerBrainTimeout bounds one planner session (either runner):
	// 30m, headroom for long multi-tool planner turns (2026-11-15
	// operator decision; was 300s/5m).
	plannerBrainTimeout = 1800 * time.Second
	// plannerConfigName is the generated opencode config written into
	// WorkDir (spec §8.4). It must be the V2 project-config name
	// `opencode.json`: V2 discovers config from the working directory,
	// and the V1-era OPENCODE_CONFIG env var is ignored by V2 (upstream
	// regression, umbrella issue #36990) - pointing the child at a temp
	// file silently lost MCP injection, permissions, and the model pin.
	// Written only by opencode-runner sessions (§9.4 runner routing).
	plannerConfigName = "opencode.json"
	// plannerClaudeServer is the MCP server name in the generated Claude
	// Code config (spec §9.4): Claude Code prefixes its tools with it, so
	// the surface arrives as mcp__factory__<tool>. The config file itself
	// (claude-mcp.json) is written by internal/claudecli.
	plannerClaudeServer = "factory"
	// envPlannerToken carries the planner bearer into the child so the
	// generated config can reference it without the secret landing in
	// the file at rest: {env:FACTORY_PLANNER_TOKEN} on the opencode
	// runner, ${FACTORY_PLANNER_TOKEN} on the claude runner.
	envPlannerToken = "FACTORY_PLANNER_TOKEN"
)

// PlannerBrain runs one-shot factory-planner LLM sessions (spec §9.5):
// a CLI child configured with the factory's own MCP surface
// (/v1/mcp/factory, planner bearer), woken with a prompt built from the
// operator inbox history (spec §6.6: per-payload 2KB caps). The runner
// routes on the session model (spec §9.4, the §8.4 worker twin): a
// claude-* model runs the Claude Code CLI, everything else opencode.
// The agent drives the factory with the MCP tools (create_job,
// list_jobs, read_result, message_foreman, list_foremen) and replies to
// the operator with notify_operator. It implements routing.Brain
// semantics for planner:main.
type PlannerBrain struct {
	// Model is the child CLI model string; "" -> the CLI's default.
	Model string
	// ModelFn, when set, resolves the model per session - the settings
	// panel's live override path (spec §6.5, 2026-11-15 amendment):
	// ModelFn wins over the static Model field so panel changes apply to
	// the next session without a restart. nil -> the static Model field.
	// It is read once per session (session/forTask) so the runner route
	// and the model the child runs can never disagree mid-session.
	ModelFn func() string
	// ClaudeBin is the Claude Code CLI binary; "" means "claude" on
	// PATH. Used only by sessions routed to the claude runner (§9.4).
	ClaudeBin string
	// MCPURL is the factory planner MCP endpoint (/v1/mcp/factory).
	MCPURL string
	// Bearer is the factory planner token.
	Bearer string
	// WorkDir is where the generated config temp file lives.
	WorkDir string
	// Exec runs one brain session for a prompt and returns its stdout.
	// nil means the real child CLI (routed on the session model, §9.4).
	// Tests replace it with a recorder; no test ever invokes a real CLI.
	Exec func(ctx context.Context, prompt string) (string, error)
	// OnUsage, when set, receives the usage parsed from each claude run.
	OnUsage func(ctx context.Context, taskID string, u claudecli.Usage)
	// taskID is set by forTask for per-task sessions.
	taskID string
}

// Handle wakes the planner for envs without a task binding: prompt =
// wake reason + history + the triggering envelopes, all payload-capped
// via llmctx (spec §6.6). The planner loop uses HandleTask (spec §9.5.2);
// Handle remains the unthreaded entry point. Errors are returned for the
// caller to surface to the operator - the brain never panics.
func (b *PlannerBrain) Handle(ctx context.Context, reason string, history, envs []wire.MessageEnvelope) error {
	prompt := llmctx.BuildWakePrompt(reason, append(history, envs...))
	stdout, err := b.session().exec()(ctx, prompt)
	if err != nil {
		return fmt.Errorf("planner session for %d envelope(s): %w", len(envs), err)
	}
	log.Printf("factoryapp: planner handled %d envelope(s); stdout: %s", len(envs), snippet(stdout))
	return nil
}

// currentModel resolves the session model: ModelFn when wired (the
// settings panel path), else the static Model field.
func (b *PlannerBrain) currentModel() string {
	if b.ModelFn != nil {
		return b.ModelFn()
	}
	return b.Model
}

// session returns a copy of b with the model resolved once for one
// session (spec §6.5): every later read of Model in the runner sees the
// same value.
func (b *PlannerBrain) session() *PlannerBrain {
	c := *b
	c.Model = b.currentModel()
	c.ModelFn = nil
	return &c
}

// exec returns the session runner: Exec when set (tests), else the child
// CLI routed on the session model (spec §9.4).
func (b *PlannerBrain) exec() func(ctx context.Context, prompt string) (string, error) {
	switch {
	case b.Exec != nil:
		return b.Exec
	case isClaudeModel(b.Model):
		return b.runClaude
	default:
		return b.runOpencode
	}
}

// PlannerWake is one per-task planner wake (spec §9.5.2).
type PlannerWake struct {
	// Reason names the trigger (operator chat vs foreman report).
	Reason string
	// Task is the woken task (header: id, title, priority, status, jobs).
	Task tasks.Task
	// Criteria is the operator's priority_criteria text ("" = unset).
	Criteria string
	// Thread is the task's own thread, oldest first (last 40 messages,
	// internal rows included so the planner sees job news).
	Thread []wire.MessageEnvelope
	// Triggers are the envelopes that caused this wake.
	Triggers []wire.MessageEnvelope
}

// plannerTaskKey carries the woken task id on the session context.
type plannerTaskKey struct{}

// PlannerTaskFromContext returns the task a planner session was woken for
// ("" outside a task wake). Session runners (and the test Exec seam) see
// it on their context.
func PlannerTaskFromContext(ctx context.Context) string {
	id, _ := ctx.Value(plannerTaskKey{}).(string)
	return id
}

// HandleTask runs one planner session for a task wake (spec §9.5.2). The
// session is bound to the task: the child's MCP URL carries
// ?task_id=<id> (the "woken task" that notify_operator and create_job
// default to, mcpfactory.WokenTaskHeader) and it runs in its own per-task
// work dir (<WorkDir>/../planner-tasks/<id>, removed afterwards) so
// parallel sessions never share a generated config.
func (b *PlannerBrain) HandleTask(ctx context.Context, w PlannerWake) error {
	sb := b.forTask(w.Task.ID)
	if b.Exec == nil {
		defer func() {
			if err := os.RemoveAll(sb.WorkDir); err != nil {
				log.Printf("factoryapp: planner: clean %s: %v", sb.WorkDir, err)
			}
		}()
	}
	ctx = context.WithValue(ctx, plannerTaskKey{}, w.Task.ID)
	stdout, err := sb.exec()(ctx, buildTaskWakePrompt(w))
	if err != nil {
		return fmt.Errorf("planner session for task %s (%d envelope(s)): %w", w.Task.ID, len(w.Triggers), err)
	}
	log.Printf("factoryapp: planner handled task %s (%d envelope(s)); stdout: %s", w.Task.ID, len(w.Triggers), snippet(stdout))
	return nil
}

// forTask returns a copy of b bound to taskID: MCP URL with the task
// query, per-task work dir.
func (b *PlannerBrain) forTask(taskID string) *PlannerBrain {
	c := *b.session()
	c.taskID = taskID
	c.MCPURL = plannerTaskURL(b.MCPURL, taskID)
	c.WorkDir = filepath.Join(filepath.Dir(filepath.Clean(b.WorkDir)), "planner-tasks", taskID)
	return &c
}

// plannerTaskURL appends task_id=<id> to the MCP URL's query.
func plannerTaskURL(base, taskID string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("task_id", taskID)
	u.RawQuery = q.Encode()
	return u.String()
}

// buildTaskWakePrompt renders a per-task wake prompt (spec §9.5.2): the
// reason, the task header, the operator's priority criteria, then the
// task thread oldest first with internal rows and the new triggers
// marked. Payloads are llmctx-capped (2KB each, spec §6.6).
func buildTaskWakePrompt(w PlannerWake) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are the factory planner. %s\n", w.Reason)

	t := w.Task
	sb.WriteString("\n=== task ===\n")
	fmt.Fprintf(&sb, "id: %s\ntitle: %s\nstatus: %s\npriority: %d (0 = most urgent)\n", t.ID, t.Title, t.Status, t.Priority)
	jobs := "none"
	if len(t.JobIDs) > 0 {
		jobs = strings.Join(t.JobIDs, ", ")
	}
	fmt.Fprintf(&sb, "job_ids: %s\n", jobs)

	sb.WriteString("\n=== operator priority criteria ===\n")
	if strings.TrimSpace(w.Criteria) == "" {
		sb.WriteString("(none set; record them with set_priority_criteria when the operator states some)\n")
	} else {
		sb.WriteString(string(llmctx.Cap([]byte(w.Criteria))) + "\n")
	}

	isTrigger := map[string]bool{}
	for _, e := range w.Triggers {
		isTrigger[e.ID] = true
	}
	fmt.Fprintf(&sb, "\n=== task thread: last %d message(s), oldest first; [internal] rows are never shown to the operator, [new] rows woke you ===\n", len(w.Thread))
	if len(w.Thread) == 0 {
		sb.WriteString("(empty)\n")
	}
	inThread := map[string]bool{}
	for i, env := range w.Thread {
		inThread[env.ID] = true
		fmt.Fprintf(&sb, "\n--- message %d%s ---\n%s", i+1, promptTags(env, isTrigger[env.ID]), llmctx.EnvelopeSnippet(env))
	}
	var missing []wire.MessageEnvelope
	for _, env := range w.Triggers {
		if !inThread[env.ID] {
			missing = append(missing, env)
		}
	}
	if len(missing) > 0 {
		sb.WriteString("\n=== new messages not yet in the thread view ===\n")
		for i, env := range missing {
			fmt.Fprintf(&sb, "\n--- new message %d%s ---\n%s", i+1, promptTags(env, true), llmctx.EnvelopeSnippet(env))
		}
	}
	fmt.Fprintf(&sb, taskWakeFooter, t.ID)
	return sb.String()
}

// promptTags marks a thread row in the wake prompt.
func promptTags(env wire.MessageEnvelope, trigger bool) string {
	var tags string
	if tasks.IsInternal(env.Type) {
		tags += " [internal]"
	}
	if trigger {
		tags += " [new]"
	}
	return tags
}

const taskWakeFooter = `
This session is bound to task %s: notify_operator and create_job default to it. Reply to the operator ONLY with notify_operator. ` +
	`Retitle or reprioritise with update_task; close it with update_task(status=closed) once its jobs are terminal and the operator has been told. ` +
	`When you are waiting on an operator decision, approval or answer, set update_task(needs_operator=true) (it puts the task in their "Needs you" list); ` +
	`set it false when that is no longer true. It clears itself when the operator replies or closes the task, so set it again if you are still waiting. ` +
	`Handle the [new] messages with your tools, then finish; do not repeat work.`

// runOpencode is the opencode-runner Exec (the non-claude route, spec
// §9.4): it regenerates the opencode project
// config in WorkDir (mcp.servers.factory -> MCPURL, bearer via
// {env:FACTORY_PLANNER_TOKEN}, oauth off, pinned model, allow-all
// permissions), then runs `opencode run <prompt>` non-interactively in
// its own process group with a 30m timeout. The child discovers the
// config from its working directory (V2 project config; OPENCODE_CONFIG
// is ignored by V2). Mirrors foremanapp's BrainRunner (#9.3 pattern).
func (b *PlannerBrain) runOpencode(ctx context.Context, prompt string) (string, error) {
	if _, err := b.writeConfig(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, plannerBrainTimeout)
	defer cancel()
	// CommandContext (not Command): cmd.Cancel below requires it.
	// --standalone: a private server inside this child process. Without
	// it the shared background service loads the generated config and
	// substitutes the {env:FACTORY_PLANNER_TOKEN} header against its own
	// environment - which lacks the token - and the MCP connect dies
	// with 401 "missing bearer token", stripping the session of every
	// factory tool. The model rides -m, not the config's model field:
	// V2 drops `#variant` suffixes from the config field, and overrides
	// may carry one.
	args := []string{"run", "--standalone"}
	if b.Model != "" {
		args = append(args, "-m", b.Model)
	}
	cmd := exec.CommandContext(ctx, "opencode", append(args, prompt)...)
	cmd.Dir = b.WorkDir
	cmd.Env = withEnvValue(os.Environ(), envPlannerToken, b.Bearer)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return stdout.String(), fmt.Errorf("planner session exceeded %s", plannerBrainTimeout)
		}
		return stdout.String(), fmt.Errorf("opencode run: %w; stderr: %s", err, snippet(stderr.String()))
	}
	return stdout.String(), nil
}

// isClaudeModel reports whether the planner session model routes to the
// Claude Code CLI (spec §9.4, the §8.4 worker routing twin): bare
// `claude-*` ids (or the bare `claude` alias), optionally behind the
// `anthropic/` provider prefix. OpenRouter-hosted claude models
// (`openrouter/anthropic/claude-...`) stay on opencode - a different
// credential path (the OpenRouter key), not the Anthropic CLI's own
// auth.
func isClaudeModel(model string) bool { return claudecli.IsModel(model) }

// claudeModelArg maps the session model onto the `claude --model` flag
// value (claudecli.ModelArg: the `anthropic/` prefix is stripped).
func claudeModelArg(model string) string { return claudecli.ModelArg(model) }

// claudeArgs builds the claude-runner argument vector (claudecli.Args,
// spec §9.4): -p, the pinned model on --model, the generated config via
// --mcp-config with the isolation, permission, and persistence flags,
// and the prompt as the FINAL argument.
func claudeArgs(model, configPath, prompt string) []string {
	return claudecli.Args(model, configPath, prompt)
}

// runClaude is the claude-runner Exec (spec §9.4, 2026-11-16
// planner-claude amendment): it regenerates the Claude Code MCP config
// in WorkDir (mcpServers.factory -> MCPURL, bearer via
// ${FACTORY_PLANNER_TOKEN}) and runs the Claude Code CLI through the
// shared claudecli runner under the 30m planner bound. The bearer rides
// the child env; it never lands in the file at rest.
func (b *PlannerBrain) runClaude(ctx context.Context, prompt string) (string, error) {
	configPath, err := b.writeClaudeConfig()
	if err != nil {
		return "", err
	}
	text, usage, err := claudecli.Exec(ctx, claudecli.Session{
		Bin:        b.ClaudeBin,
		Model:      b.Model,
		ConfigPath: configPath,
		Dir:        b.WorkDir,
		TokenEnv:   envPlannerToken,
		Token:      b.Bearer,
		Prompt:     prompt,
		Timeout:    plannerBrainTimeout,
		Label:      "planner session",
	})
	if b.OnUsage != nil && !usage.IsZero() {
		b.OnUsage(ctx, b.taskID, usage)
	}
	return text, err
}

// writeConfig regenerates the opencode project config at
// WorkDir/opencode.json, 0600 (the §8.4 pattern; foremanapp.BrainRunner
// twin). The file persists across runs - each run rewrites it, so the
// child always starts from the freshly generated config. Written only
// by opencode-runner sessions (§9.4 runner routing).
func (b *PlannerBrain) writeConfig() (string, error) {
	if err := os.MkdirAll(b.WorkDir, 0o755); err != nil {
		return "", fmt.Errorf("planner work dir: %w", err)
	}
	path := filepath.Join(b.WorkDir, plannerConfigName)
	cfg := plannerOpenCODEConfig{
		Model: b.Model,
		MCP: plannerMCPBlock{Servers: map[string]*plannerRemoteMCPServer{
			"factory": {
				Type:    "remote",
				URL:     b.MCPURL,
				Headers: map[string]string{"Authorization": "Bearer {env:" + envPlannerToken + "}"},
				OAuth:   false,
			},
		}},
		Permissions: plannerPermissions,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	return path, os.WriteFile(path, data, 0o600)
}

// writeClaudeConfig regenerates the Claude Code MCP config at
// WorkDir/claude-mcp.json, 0600 (spec §9.4; the workerharness
// claude-mcp.json twin). The file persists across runs - each run
// rewrites it. It carries only the MCP injection: the model rides
// --model and the headless permission policy rides --permission-mode
// (the analog of the opencode runner's generated allow-all block).
func (b *PlannerBrain) writeClaudeConfig() (string, error) {
	return claudecli.WriteConfig(b.WorkDir, plannerClaudeServer, b.MCPURL, envPlannerToken)
}

// plannerOpenCODEConfig and friends mirror the opencode V2 config shape
// (docs/mcp-servers) used by workerharness.occonfig / foremanapp.brain.
type plannerRemoteMCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	OAuth   bool              `json:"oauth"`
}

type plannerMCPBlock struct {
	Servers map[string]*plannerRemoteMCPServer `json:"servers"`
}

// plannerPermissionRule is one opencode V2 permission rule (docs/
// permissions); plannerPermissions pins allow-all for the headless child.
type plannerPermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// plannerPermissions is the planner child's permission policy: a headless
// `opencode run` has no client to answer an "ask", so any prompt
// auto-declines and aborts the whole session mid-flight (exit status 1,
// work already done discarded - observed live). The child already runs
// with the service user's authority; an explicit allow-all keeps it alive.
var plannerPermissions = []plannerPermissionRule{
	{Action: "*", Resource: "*", Effect: "allow"},
}

type plannerOpenCODEConfig struct {
	Model       string                  `json:"model,omitempty"`
	MCP         plannerMCPBlock         `json:"mcp"`
	Permissions []plannerPermissionRule `json:"permissions"`
}

// plannerClaudeConfig is the generated Claude Code MCP config (spec
// §9.4), shared with the foreman brain via internal/claudecli.
type plannerClaudeConfig = claudecli.Config

// withEnvValue returns base with key set to value, replacing any earlier
// assignment (foremanapp twin).
func withEnvValue(base []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return append(out, prefix+value)
}

// snippet returns the first 200 chars of s for logging.
func snippet(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}
