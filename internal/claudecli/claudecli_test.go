package claudecli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestIsModel pins the claude-runner routing (spec §9.3/§9.4, the §8.4
// worker twin): bare claude-* ids and the bare claude alias, optionally
// behind the anthropic/ prefix, route to the Claude Code CLI. Everything
// else - including OpenRouter-hosted claude models, a different credential
// path - stays on opencode.
func TestIsModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model string
		want  bool
	}{
		{"claude-opus-5-5", true},
		{"claude-sonnet-5-5", true},
		{"anthropic/claude-opus-5-5", true},
		{"claude", true},
		{"anthropic/claude", true},
		{"openrouter/anthropic/claude-opus-5-5", false},
		{"openrouter/z-ai/glm-5.3", false},
		{"opus", false},
		{"claudex", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := IsModel(tc.model); got != tc.want {
			t.Errorf("IsModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// TestModelArg: the anthropic/ provider prefix is stripped for --model; the
// rest passes through verbatim (exact ids and aliases).
func TestModelArg(t *testing.T) {
	t.Parallel()
	tests := []struct{ model, want string }{
		{"claude-opus-5-5", "claude-opus-5-5"},
		{"anthropic/claude-opus-5-5", "claude-opus-5-5"},
		{"anthropic/claude", "claude"},
		{"opus", "opus"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := ModelArg(tc.model); got != tc.want {
			t.Errorf("ModelArg(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

// TestArgs pins the claude-runner argument vector: -p, the pinned model on
// --model (omitted when unset), the config via --mcp-config with the
// isolation, permission, and persistence flags, and the prompt as the FINAL
// argument (--mcp-config is variadic and would otherwise swallow it).
func TestArgs(t *testing.T) {
	t.Parallel()
	const prompt = "handle the inbox"
	tests := []struct {
		name, model, config string
	}{
		{"pinned model", "claude-opus-5-5", "/srv/factory/brain/claude-mcp.json"},
		{"anthropic prefix stripped", "anthropic/claude-opus-5-5", "/tmp/c.json"},
		{"unset model", "", "/tmp/c.json"},
	}
	for _, tc := range tests {
		got := Args(tc.model, tc.config, prompt)
		want := []string{"-p"}
		if tc.model != "" {
			want = append(want, "--model", ModelArg(tc.model))
		}
		want = append(want,
			"--mcp-config", tc.config,
			"--strict-mcp-config",
			"--permission-mode", "bypassPermissions",
			"--no-session-persistence",
			prompt,
		)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Args = %q, want %q", tc.name, got, want)
		}
		if got[len(got)-1] != prompt {
			t.Errorf("%s: prompt is not the final argument: %q", tc.name, got)
		}
	}
}

// TestWriteConfig pins the generated Claude Code MCP config: the file lands
// at <dir>/claude-mcp.json 0600, carries exactly one streamable-HTTP server
// under the given name, references the bearer via ${VAR} expansion, and
// never leaks the secret at rest. No model or permissions block rides it.
func TestWriteConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path, err := WriteConfig(dir, "foreman", "http://127.0.0.1:8090/v1/mcp/foreman", "FOREMAN_BRAIN_TOKEN")
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
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
	var got Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal(%s): %v", data, err)
	}
	if len(got.MCPServers) != 1 {
		t.Fatalf("mcpServers = %v, want exactly the foreman server", got.MCPServers)
	}
	srv, ok := got.MCPServers["foreman"]
	if !ok {
		t.Fatalf("generated config lost the foreman MCP server:\n%s", data)
	}
	if srv.Type != "http" {
		t.Errorf("server type = %q, want \"http\"", srv.Type)
	}
	if srv.URL != "http://127.0.0.1:8090/v1/mcp/foreman" {
		t.Errorf("server url = %q", srv.URL)
	}
	if want := "Bearer ${FOREMAN_BRAIN_TOKEN}"; srv.Headers["Authorization"] != want {
		t.Errorf("Authorization = %q, want %q", srv.Headers["Authorization"], want)
	}
	for _, forbidden := range []string{"model", "permissions"} {
		if strings.Contains(string(data), `"`+forbidden+`"`) {
			t.Errorf("generated config carries a %q block (model rides --model, policy rides --permission-mode):\n%s", forbidden, data)
		}
	}
}

// fakeClaude writes a fake `claude` shell script into a fresh temp dir and
// returns its path. The child runs with cwd Dir, so its recordings
// (args.txt, pwd.txt, token.txt) land there. No test invokes a real CLI.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > args.txt\n" +
		"pwd > pwd.txt\n" +
		"printf '%s' \"$TEST_CLAUDE_TOKEN\" > token.txt\n" +
		"printf '%s' '" + body + "'\n"
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

// TestExec runs a session against a fake CLI: the argv is exactly Args, the
// child runs in Dir with the bearer in its env, and stdout is returned.
func TestExec(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ConfigName)
	stdout, err := Exec(context.Background(), Session{
		Bin:        fakeClaude(t, "said hi"),
		Model:      "claude-opus-5-5",
		ConfigPath: cfg,
		Dir:        dir,
		TokenEnv:   "TEST_CLAUDE_TOKEN",
		Token:      "bearer-secret",
		Prompt:     "wake up",
		Timeout:    time.Minute,
		Label:      "test session",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if stdout != "said hi" {
		t.Errorf("stdout = %q, want the fake CLI output", stdout)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatalf("fake claude never recorded argv: %v", err)
	}
	want := Args("claude-opus-5-5", cfg, "wake up")
	if got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"); !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
	pwd, err := os.ReadFile(filepath.Join(dir, "pwd.txt"))
	if err != nil || strings.TrimSpace(string(pwd)) != dir {
		t.Errorf("cwd = %q (err %v), want %q", pwd, err, dir)
	}
	tok, err := os.ReadFile(filepath.Join(dir, "token.txt"))
	if err != nil || string(tok) != "bearer-secret" {
		t.Errorf("token env = %q (err %v), want the bearer", tok, err)
	}
}

// TestExecFailures pins the loud-failure semantics: a non-zero exit with
// stderr surfaces stderr; with EMPTY stderr it surfaces stdout, where Claude
// Code reports in-run failures such as missing auth (observed live).
func TestExecFailures(t *testing.T) {
	tests := []struct {
		name, script, wantErr string
	}{
		{"stderr surfaces stderr", "printf 'boom' >&2; exit 3", "stderr: boom"},
		{"empty stderr falls back to stdout", "printf 'Not logged in'; exit 1", "stdout: Not logged in"},
	}
	for _, tc := range tests {
		dir := t.TempDir()
		bin := filepath.Join(dir, "claude")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
			t.Fatalf("%s: write: %v", tc.name, err)
		}
		_, err := Exec(context.Background(), Session{
			Bin: bin, Model: "claude-opus-5-5", ConfigPath: "c.json", Dir: dir,
			TokenEnv: "TEST_CLAUDE_TOKEN", Token: "x", Prompt: "p",
			Timeout: time.Minute, Label: "test session",
		})
		if err == nil {
			t.Errorf("%s: Exec = nil error, want the exit failure surfaced", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "claude run:") || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error %q does not surface %q", tc.name, err, tc.wantErr)
		}
	}
}

// TestExecMissingCLI: an absent Claude Code binary fails loudly by name
// instead of silently producing an empty session.
func TestExecMissingCLI(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Exec(context.Background(), Session{
		Bin: filepath.Join(dir, "no-such-claude"), Model: "claude-opus-5-5",
		ConfigPath: "c.json", Dir: dir, TokenEnv: "TEST_CLAUDE_TOKEN", Token: "x",
		Prompt: "p", Timeout: time.Minute, Label: "test session",
	})
	if err == nil || !strings.Contains(err.Error(), "claude CLI not found") {
		t.Fatalf("Exec with a missing binary = %v, want a loud \"claude CLI not found\" error", err)
	}
}

// TestExecTimeout: the session bound kills the child and names the session
// in the error.
func TestExecTimeout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Exec(context.Background(), Session{
		Bin: bin, Model: "claude-opus-5-5", ConfigPath: "c.json", Dir: dir,
		TokenEnv: "TEST_CLAUDE_TOKEN", Token: "x", Prompt: "p",
		Timeout: 200 * time.Millisecond, Label: "test session",
	})
	if err == nil || !strings.Contains(err.Error(), "test session exceeded") {
		t.Fatalf("Exec past its bound = %v, want a \"test session exceeded\" error", err)
	}
}

// TestExecParentCancel: a caller's deadline (the router's job.question SLA)
// is reported as the caller's cancellation, not as the session bound.
func TestExecParentCancel(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Exec(ctx, Session{
		Bin: bin, Model: "claude-opus-5-5", ConfigPath: "c.json", Dir: dir,
		TokenEnv: "TEST_CLAUDE_TOKEN", Token: "x", Prompt: "p",
		Timeout: time.Minute, Label: "test session",
	})
	if err == nil || strings.Contains(err.Error(), "test session exceeded") || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("Exec under a caller deadline = %v, want a \"cancelled\" error (not the session bound)", err)
	}
}

// TestSnippet bounds error text at 200 runes.
func TestSnippet(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 300)
	if got := []rune(snippet(long)); len(got) != 200 {
		t.Errorf("snippet len = %d runes, want 200", len(got))
	}
	if snippet("short") != "short" {
		t.Errorf("snippet mangled a short string")
	}
}
