package factoryapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/occonfig"
	"github.com/pricec/factory/internal/wire"
)

// TestPlannerModelDefault: the planner brain defaults to the pinned
// claude default (spec §6.5, 2026-11-16 planner-claude amendment) when
// FACTORY_PLANNER_MODEL is unset - and that default routes to the
// claude runner, so an accidental re-pin to a non-claude string cannot
// silently put the planner back on opencode.
func TestPlannerModelDefault(t *testing.T) {
	t.Setenv("FACTORY_PLANNER_TOKEN", "test-planner-secret-0123456789")
	a, err := New(Config{DataDir: t.TempDir(), Addr: ":8090", AdvertiseURL: "http://factory.test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	if a.planner.Model != models.PlannerDefault {
		t.Errorf("planner model = %q, want the pinned default %q", a.planner.Model, models.PlannerDefault)
	}
	if !isClaudeModel(models.PlannerDefault) {
		t.Errorf("pinned planner default %q does not route to the claude runner (planner-claude amendment)", models.PlannerDefault)
	}
	// The MCP endpoint must be loopback: the MCP SDK's streamable-HTTP
	// server rejects non-loopback Host headers, and the planner child
	// always runs on this host (spec §9.5).
	if a.planner.MCPURL != "http://127.0.0.1:8090/v1/mcp/factory" {
		t.Errorf("planner MCP URL = %q, want the loopback endpoint", a.planner.MCPURL)
	}
}

// TestPlannerOpencodeConfig pins the generated opencode-runner config:
// it must land at the V2 project-config path (WorkDir/opencode.json -
// the V1-era OPENCODE_CONFIG env var is ignored by V2, which silently
// dropped the whole config), carry the allow-all permissions (a
// headless run has no client to answer an ask; an unanswered ask
// aborts the session), and pin the model. Written only when the
// session model routes to the opencode runner (§9.4).
func TestPlannerOpencodeConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := &PlannerBrain{
		Model:   "openrouter/z-ai/glm-5.3",
		MCPURL:  "http://factory.test/v1/mcp/factory",
		Bearer:  "planner-secret",
		WorkDir: dir,

		OpenCODEVersion: occonfig.V2,
	}
	path, err := b.writeConfig()
	if err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	if want := filepath.Join(dir, "opencode.json"); path != want {
		t.Errorf("config path = %q, want the V2 project-config path %q", path, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("config perms: %v (%o), want 0600", err, info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal(%s): %v", data, err)
	}
	if got["model"] != "openrouter/z-ai/glm-5.3" {
		t.Errorf("model = %v, want %q", got["model"], "openrouter/z-ai/glm-5.3")
	}
	wantPerms := []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"}}
	if !reflect.DeepEqual(got["permissions"], wantPerms) {
		t.Errorf("permissions = %v, want %v", got["permissions"], wantPerms)
	}
	if got["mcp"] == nil {
		t.Errorf("generated config lost the mcp block:\n%s", data)
	}
	if contains := string(data); containsSecret(contains, "planner-secret") {
		t.Errorf("generated config leaks the bearer at rest:\n%s", data)
	}
}

// TestPlannerClaudeConfig pins the generated claude-runner config (spec
// §9.4): it lands at WorkDir/claude-mcp.json (0600, rewritten every
// run), mounts the factory MCP server as a streamable-HTTP entry named
// "factory" (tools arrive as mcp__factory__*), references the bearer
// via ${FACTORY_PLANNER_TOKEN} so the secret never lands at rest, and
// carries NO model field (the model rides --model) and NO permissions
// block (the headless policy rides --permission-mode).
func TestPlannerClaudeConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := &PlannerBrain{
		Model:   "claude-opus-5-5",
		MCPURL:  "http://factory.test/v1/mcp/factory",
		Bearer:  "planner-secret",
		WorkDir: dir,
	}
	path, err := b.writeClaudeConfig()
	if err != nil {
		t.Fatalf("writeClaudeConfig: %v", err)
	}
	if want := filepath.Join(dir, "claude-mcp.json"); path != want {
		t.Errorf("config path = %q, want %q", path, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("config perms: %v (%o), want 0600", err, info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got plannerClaudeConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal(%s): %v", data, err)
	}
	srv, ok := got.MCPServers["factory"]
	if !ok {
		t.Fatalf("generated config lost the factory MCP server:\n%s", data)
	}
	if srv.Type != "http" {
		t.Errorf("factory server type = %q, want the streamable-HTTP transport \"http\"", srv.Type)
	}
	if srv.URL != b.MCPURL {
		t.Errorf("factory server url = %q, want %q", srv.URL, b.MCPURL)
	}
	if want := "Bearer ${FACTORY_PLANNER_TOKEN}"; srv.Headers["Authorization"] != want {
		t.Errorf("Authorization header = %q, want %q", srv.Headers["Authorization"], want)
	}
	if len(got.MCPServers) != 1 {
		t.Errorf("mcpServers = %v, want exactly the factory server", got.MCPServers)
	}
	if contains := string(data); containsSecret(contains, "planner-secret") {
		t.Errorf("generated config leaks the bearer at rest:\n%s", data)
	}
}

// TestPlannerRunnerRouting pins the §9.4 runner routing (the §8.4
// worker twin): bare claude ids/aliases, optionally behind the
// anthropic/ provider prefix, run via the Claude Code CLI; everything
// else - including OpenRouter-hosted claude models (a different
// credential path) - stays on opencode.
func TestPlannerRunnerRouting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model  string
		claude bool
	}{
		{"claude", true},
		{"claude-opus-5-5", true},
		{"claude-sonnet-4-5", true},
		{"anthropic/claude-opus-5-5", true},
		{"", false},
		{"openrouter/z-ai/glm-5.3", false},
		{"openrouter/z-ai/glm-5.3-flash#max", false},
		{"ollama/qwen3:8b", false},
		{"openrouter/anthropic/claude-haiku-4.5", false},
		{"glm-5.3", false},
		{"claudecode", false},
	}
	for _, tc := range tests {
		if got := isClaudeModel(tc.model); got != tc.claude {
			t.Errorf("isClaudeModel(%q) = %v, want %v", tc.model, got, tc.claude)
		}
	}
}

// TestPlannerClaudeModelArg pins the --model mapping: the anthropic/
// provider prefix is stripped (the CLI takes bare ids and aliases),
// everything else passes through verbatim - openrouter/... strings
// never reach the claude runner (routing above), so they need no
// handling here.
func TestPlannerClaudeModelArg(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model string
		want  string
	}{
		{"claude-opus-5-5", "claude-opus-5-5"},
		{"anthropic/claude-opus-5-5", "claude-opus-5-5"},
		{"anthropic/claude", "claude"},
		{"opus", "opus"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := claudeModelArg(tc.model); got != tc.want {
			t.Errorf("claudeModelArg(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

// TestPlannerClaudeArgs pins the claude-runner argument vector: the
// pinned model rides --model (omitted when unset), the generated
// config rides --mcp-config together with the isolation, permission,
// and persistence flags, and the prompt is the FINAL argument -
// --mcp-config is variadic and would otherwise swallow it.
func TestPlannerClaudeArgs(t *testing.T) {
	t.Parallel()
	prompt := "handle the operator inbox"
	tests := []struct {
		name   string
		model  string
		config string
	}{
		{"pinned model", "claude-opus-5-5", "/srv/factory/planner/claude-mcp.json"},
		{"anthropic prefix stripped", "anthropic/claude-opus-5-5", "/tmp/c.json"},
		{"unset model", "", "/tmp/c.json"},
	}
	for _, tc := range tests {
		got := claudeArgs(tc.model, tc.config, prompt)
		want := []string{"-p", "--output-format", "stream-json", "--verbose"}
		if tc.model != "" {
			want = append(want, "--model", claudeModelArg(tc.model))
		}
		want = append(want,
			"--mcp-config", tc.config,
			"--strict-mcp-config",
			"--permission-mode", "bypassPermissions",
			"--no-session-persistence",
			prompt,
		)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: claudeArgs = %q, want %q", tc.name, got, want)
		}
		if got[len(got)-1] != prompt {
			t.Errorf("%s: prompt is not the final argument: %q", tc.name, got)
		}
	}
}

// fakePlannerClaude writes a fake `claude` shell script named "claude"
// into a fresh temp dir and returns its path (the ClaudeBin seam). The
// child runs with cwd WorkDir, so its recordings (args.txt, pwd.txt,
// token.txt) land there. No test ever invokes a real CLI.
func fakePlannerClaude(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > args.txt\n" +
		"pwd > pwd.txt\n" +
		"printf '%s' \"$FACTORY_PLANNER_TOKEN\" > token.txt\n" +
		"printf '%s' '" + body + "'\n"
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

// TestPlannerClaudeRun runs the claude runner end to end against a fake
// Claude Code CLI: the generated config is written before the child
// starts, the child runs in WorkDir with FACTORY_PLANNER_TOKEN in its
// environment, the argument vector is exactly claudeArgs, and the
// child's stdout is the session's return value.
func TestPlannerClaudeRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := &PlannerBrain{
		Model:     "claude-opus-5-5",
		ClaudeBin: fakePlannerClaude(t, "planner says hi"),
		MCPURL:    "http://127.0.0.1:8090/v1/mcp/factory",
		Bearer:    "planner-secret",
		WorkDir:   dir,
	}
	stdout, err := b.runClaude(context.Background(), "handle the operator inbox")
	if err != nil {
		t.Fatalf("runClaude: %v", err)
	}
	if stdout != "planner says hi" {
		t.Errorf("stdout = %q, want the fake claude's output", stdout)
	}

	// The generated config exists next to the child (spec §9.4).
	if _, err := os.Stat(filepath.Join(dir, "claude-mcp.json")); err != nil {
		t.Errorf("generated claude config missing: %v", err)
	}

	// Argument vector: exactly claudeArgs with the WorkDir config path.
	args, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatalf("fake claude never recorded its argv: %v", err)
	}
	wantArgs := claudeArgs("claude-opus-5-5", filepath.Join(dir, "claude-mcp.json"), "handle the operator inbox")
	if got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("fake claude argv = %q, want %q", got, wantArgs)
	}

	// The child ran in WorkDir with the bearer in its environment.
	pwd, err := os.ReadFile(filepath.Join(dir, "pwd.txt"))
	if err != nil || strings.TrimSpace(string(pwd)) != dir {
		t.Errorf("fake claude cwd = %q (err %v), want the WorkDir %q", pwd, err, dir)
	}
	tok, err := os.ReadFile(filepath.Join(dir, "token.txt"))
	if err != nil || string(tok) != "planner-secret" {
		t.Errorf("fake claude FACTORY_PLANNER_TOKEN = %q (err %v), want the bearer", tok, err)
	}
}

// TestPlannerClaudeRunFailures pins the claude-runner error semantics
// (spec §8.4/§8.5): a non-zero exit with stderr surfaces stderr; a
// non-zero exit with EMPTY stderr surfaces stdout - Claude Code reports
// in-run failures (missing auth among them) as the result on stdout
// (observed live on 2.1.325).
func TestPlannerClaudeRunFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		script  string
		wantErr string
	}{
		{
			name:    "stderr surfaces stderr",
			script:  "printf 'boom' >&2; exit 3",
			wantErr: "stderr: boom",
		},
		{
			name:    "empty stderr falls back to stdout",
			script:  "printf 'Not logged in'; exit 1",
			wantErr: "stdout: Not logged in",
		},
	}
	for _, tc := range tests {
		dir := t.TempDir()
		bin := filepath.Join(dir, "claude")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
			t.Fatalf("%s: write fake claude: %v", tc.name, err)
		}
		b := &PlannerBrain{
			Model:     "claude-opus-5-5",
			ClaudeBin: bin,
			MCPURL:    "http://127.0.0.1:8090/v1/mcp/factory",
			Bearer:    "planner-secret",
			WorkDir:   dir,
		}
		_, err := b.runClaude(context.Background(), "prompt")
		if err == nil {
			t.Errorf("%s: runClaude = nil error, want the exit failure surfaced", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "claude run:") {
			t.Errorf("%s: error %q does not name the claude runner", tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error %q does not surface %q", tc.name, err, tc.wantErr)
		}
	}
}

// TestPlannerHandleRoutesClaudeRunner pins the §9.4 routing where it
// matters: a claude-* session model sends Handle's default Exec to the
// claude runner (a non-claude model keeps the opencode path, covered by
// TestPlannerModelDefault's routing guard and the §9.4 spec text).
func TestPlannerHandleRoutesClaudeRunner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := &PlannerBrain{
		Model:     "claude-opus-5-5",
		ClaudeBin: fakePlannerClaude(t, "done"),
		MCPURL:    "http://127.0.0.1:8090/v1/mcp/factory",
		Bearer:    "planner-secret",
		WorkDir:   dir,
	}
	env := wire.MessageEnvelope{
		Type:    "chat.message",
		From:    "human:operator",
		Payload: []byte(`{"text":"plan me a job"}`),
	}
	if err := b.Handle(context.Background(), "planner: new chat from the operator needs a reply", nil, []wire.MessageEnvelope{env}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// The fake claude ran (its argv recording exists) and the generated
	// config landed next to it: the claude runner served the session.
	if _, err := os.Stat(filepath.Join(dir, "args.txt")); err != nil {
		t.Errorf("claude runner never ran: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude-mcp.json")); err != nil {
		t.Errorf("generated claude config missing: %v", err)
	}
	// Each runner generates only its own file: the opencode config was
	// not written.
	if _, err := os.Stat(filepath.Join(dir, "opencode.json")); err == nil {
		t.Error("claude-runner session wrote the opencode config")
	}
}

func containsSecret(data, secret string) bool {
	return len(secret) >= 6 && strings.Contains(data, secret)
}

// TestPlannerBrainTimeoutBound pins the planner brain session bound at
// 30m (2026-11-15 operator decision; previously 5m). Guards against an
// accidental revert to the old 300s constant while keeping the bound
// finite.
func TestPlannerBrainTimeoutBound(t *testing.T) {
	t.Parallel()
	if plannerBrainTimeout != 30*time.Minute {
		t.Errorf("plannerBrainTimeout = %v, want 30m (1800s)", plannerBrainTimeout)
	}
}

// TestPlannerModelFnPerSession: the settings panel's live override
// (spec §6.5, 2026-11-15 amendment) - ModelFn wins over the static
// Model, is resolved once per session (session/forTask), and the
// resolved value drives the runner route (§9.4): a panel switch from a
// claude-* id to an opencode id re-routes the next session.
func TestPlannerModelFnPerSession(t *testing.T) {
	current := "claude-opus-5-5"
	b := &PlannerBrain{
		Model:   "openrouter/z-ai/glm-5.3",
		ModelFn: func() string { return current },
		MCPURL:  "http://127.0.0.1:1/v1/mcp/factory",
		WorkDir: t.TempDir(),
	}
	s := b.session()
	if s.Model != "claude-opus-5-5" || s.ModelFn != nil {
		t.Fatalf("session = model %q fn-nil=%v, want the ModelFn value frozen", s.Model, s.ModelFn == nil)
	}
	if !isClaudeModel(s.Model) {
		t.Errorf("claude panel model %q does not route to the claude runner", s.Model)
	}
	// A later panel change does not leak into an already-resolved session.
	current = "ollama/qwen3:8b"
	if s.Model != "claude-opus-5-5" {
		t.Errorf("resolved session model changed mid-session to %q", s.Model)
	}
	// ...but the next session (task-bound included) picks it up.
	if got := b.forTask("task_1").Model; got != "ollama/qwen3:8b" {
		t.Errorf("forTask model = %q, want the new panel value", got)
	}
	if isClaudeModel(b.session().Model) {
		t.Errorf("opencode panel model routes to the claude runner")
	}
	// No ModelFn: the static Model applies.
	b.ModelFn = nil
	if got := b.session().Model; got != "openrouter/z-ai/glm-5.3" {
		t.Errorf("static session model = %q, want the Model field", got)
	}
}

// TestPlannerOpencodeConfigV1 pins the V1 shape on a V1 host.
func TestPlannerOpencodeConfigV1(t *testing.T) {
	t.Parallel()
	b := &PlannerBrain{
		Model: "openrouter/z-ai/glm-5.3", MCPURL: "http://factory.test/v1/mcp/factory",
		Bearer: "planner-secret", WorkDir: t.TempDir(), OpenCODEVersion: occonfig.V1,
	}
	path, err := b.writeConfig()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["permissions"]; ok {
		t.Errorf("V1 config carries V2 permissions:\n%s", data)
	}
	if perm, _ := got["permission"].(map[string]any); perm["edit"] != "allow" || perm["bash"] != "allow" {
		t.Errorf("permission = %v, want allow-all rules", got["permission"])
	}
	if _, ok := got["mcp"].(map[string]any)["factory"]; !ok {
		t.Errorf("V1 mcp must nest the server directly:\n%s", data)
	}
}
