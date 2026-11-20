package foremanapp

import (
	"context"
	"strings"
	"testing"
)

func TestBrainPreflight(t *testing.T) {
	ok := &BrainRunner{Bearer: "secret-token-value", MCPURL: "http://127.0.0.1:1/v1/mcp/foreman"}
	if err := ok.Preflight(); err != nil {
		t.Fatalf("complete runner: %v", err)
	}
	for name, b := range map[string]*BrainRunner{
		"no token": {MCPURL: "http://x"},
		"no url":   {Bearer: "secret-token-value"},
		"neither":  {},
	} {
		err := b.Preflight()
		if err == nil {
			t.Fatalf("%s: want error", name)
		}
		if strings.Contains(err.Error(), "secret-token-value") {
			t.Fatalf("%s: error leaks token: %v", name, err)
		}
	}
}

// A real (non-Exec) claude session with no bearer must fail before the CLI runs.
func TestBrainClaudeMissingTokenFailsLoudly(t *testing.T) {
	b := &BrainRunner{
		Model: "claude-opus-5-5", MCPURL: "http://x", WorkDir: t.TempDir(),
		ClaudeBin: fakeBrainClaude(t, "ran"),
	}
	if _, err := b.runClaude(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("want preflight error, got %v", err)
	}
}
