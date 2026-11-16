package foremanapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/claudecli"
	"github.com/pricec/factory/internal/wire"
)

// fakeBrainClaude writes a fake `claude` shell script into a fresh temp dir
// and returns its path (the ClaudeBin seam). The child runs with cwd
// WorkDir, so its recordings (args.txt, pwd.txt, token.txt) land there. No
// test ever invokes a real CLI.
func fakeBrainClaude(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	// NUL-delimited: the wake prompt is multi-line.
	script := "#!/bin/sh\n" +
		"printf '%s\\0' \"$@\" > args.txt\n" +
		"pwd > pwd.txt\n" +
		"printf '%s' \"$FOREMAN_BRAIN_TOKEN\" > token.txt\n" +
		"printf '%s' '" + body + "'\n"
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

// TestBrainClaudeConfig pins the generated Claude Code MCP config for the
// brain: the foreman server (tools surface as mcp__foreman__<tool>), the
// streamable-HTTP transport, the bearer via ${FOREMAN_BRAIN_TOKEN} (never
// at rest), and no opencode-only blocks.
func TestBrainClaudeConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := &BrainRunner{
		Model:   "claude-opus-5-5",
		MCPURL:  "http://127.0.0.1:8090/v1/mcp/foreman",
		Bearer:  "brain-secret",
		WorkDir: dir,
	}
	path, err := b.writeClaudeConfig()
	if err != nil {
		t.Fatalf("writeClaudeConfig: %v", err)
	}
	if want := filepath.Join(dir, "claude-mcp.json"); path != want {
		t.Errorf("config path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got claudecli.Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal(%s): %v", data, err)
	}
	srv, ok := got.MCPServers["foreman"]
	if !ok || len(got.MCPServers) != 1 {
		t.Fatalf("mcpServers = %v, want exactly the foreman server", got.MCPServers)
	}
	if srv.Type != "http" || srv.URL != b.MCPURL {
		t.Errorf("server = %+v, want http at %q", srv, b.MCPURL)
	}
	if want := "Bearer ${FOREMAN_BRAIN_TOKEN}"; srv.Headers["Authorization"] != want {
		t.Errorf("Authorization = %q, want %q", srv.Headers["Authorization"], want)
	}
	if strings.Contains(string(data), "brain-secret") {
		t.Errorf("generated config leaks the bearer at rest:\n%s", data)
	}
}

// TestBrainClaudeRunRouting drives Handle against a claude-* session model
// with a fake CLI: the claude runner serves the session (argv recorded,
// claude-mcp.json generated), the child runs in WorkDir with the bearer in
// its env, and the opencode config is never written (each runner generates
// only its own file).
func TestBrainClaudeRunRouting(t *testing.T) {
	dir := t.TempDir()
	b := &BrainRunner{
		Model:     "claude-opus-5-5",
		ClaudeBin: fakeBrainClaude(t, "handled"),
		MCPURL:    "http://127.0.0.1:8090/v1/mcp/foreman",
		Bearer:    "brain-secret",
		WorkDir:   dir,
	}
	env := wire.MessageEnvelope{Type: "job.question", ID: "m1", From: "job:j1", Payload: []byte(`{"text":"which way?"}`)}
	if err := b.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	args, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatalf("claude runner never ran: %v", err)
	}
	cfg := filepath.Join(dir, "claude-mcp.json")
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("generated claude config missing: %v", err)
	}
	// The argv is exactly the shared claudecli shape, prompt last.
	got := strings.Split(strings.TrimSuffix(string(args), "\x00"), "\x00")
	prompt := got[len(got)-1]
	if want := claudecli.Args("claude-opus-5-5", cfg, prompt); !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if !strings.Contains(prompt, "which way?") {
		t.Errorf("prompt does not carry the triggering envelope: %q", prompt)
	}
	if pwd, err := os.ReadFile(filepath.Join(dir, "pwd.txt")); err != nil || strings.TrimSpace(string(pwd)) != dir {
		t.Errorf("child cwd = %q (err %v), want %q", pwd, err, dir)
	}
	if tok, err := os.ReadFile(filepath.Join(dir, "token.txt")); err != nil || string(tok) != "brain-secret" {
		t.Errorf("FOREMAN_BRAIN_TOKEN in child = %q (err %v), want the bearer", tok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "opencode.json")); err == nil {
		t.Error("claude-runner session wrote the opencode config")
	}
}

// TestBrainClaudeMissingCLIFailsLoudly: a claude-* brain with no Claude
// Code CLI on the host returns an error to the router (logged, never a
// silent drop).
func TestBrainClaudeMissingCLIFailsLoudly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := &BrainRunner{
		Model:     "claude-opus-5-5",
		ClaudeBin: filepath.Join(dir, "no-such-claude"),
		MCPURL:    "http://127.0.0.1:8090/v1/mcp/foreman",
		Bearer:    "brain-secret",
		WorkDir:   dir,
	}
	env := wire.MessageEnvelope{Type: "brain.wake", ID: "m2", From: "foreman:f1", Payload: []byte(`{}`)}
	err := b.Handle(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "claude CLI not found") {
		t.Fatalf("Handle with a missing claude CLI = %v, want a loud \"claude CLI not found\" error", err)
	}
}

// TestBrainClaudeAuthFailureFailsLoudly: an unauthenticated claude run
// (non-zero exit, the auth error on stdout with empty stderr - observed
// live) surfaces the auth text in the session error.
func TestBrainClaudeAuthFailureFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'Not logged in · Please run /login'; exit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &BrainRunner{
		Model:     "claude-opus-5-5",
		ClaudeBin: bin,
		MCPURL:    "http://127.0.0.1:8090/v1/mcp/foreman",
		Bearer:    "brain-secret",
		WorkDir:   dir,
	}
	env := wire.MessageEnvelope{Type: "brain.wake", ID: "m3", From: "foreman:f1", Payload: []byte(`{}`)}
	err := b.Handle(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("Handle with unauthenticated claude = %v, want the auth error surfaced", err)
	}
}
