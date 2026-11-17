package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeLoginWarning(t *testing.T) {
	loggedIn := t.TempDir()
	if err := os.MkdirAll(filepath.Join(loggedIn, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loggedIn, ".claude", ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		models []string
		env    map[string]string
		warn   bool
	}{
		{"no claude models", []string{"", "glm-5"}, map[string]string{"HOME": t.TempDir()}, false},
		{"claude, no creds", []string{"claude-opus-5-5"}, map[string]string{"HOME": t.TempDir()}, true},
		{"claude, creds file", []string{"claude-opus-5-5"}, map[string]string{"HOME": loggedIn}, false},
		{"claude, api key", []string{"claude-opus-5-5"}, map[string]string{"HOME": t.TempDir(), "ANTHROPIC_API_KEY": "x"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeLoginWarning(tc.models, func(k string) string { return tc.env[k] })
			if (got != "") != tc.warn {
				t.Fatalf("warning = %q, want warn=%v", got, tc.warn)
			}
		})
	}
}
