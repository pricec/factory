package claudecli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExecExtraEnvRedacted (spec §13.3): ExtraEnv reaches the child and
// its values never surface in an error or the result text.
func TestExecExtraEnvRedacted(t *testing.T) {
	const tok = "sk-ant-oat01-EXTRAENVabcdefghijklmnopqrstuvwxyz"
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"token is $CLAUDE_CODE_OAUTH_TOKEN\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	text, _, err := Exec(context.Background(), Session{
		Bin: bin, Dir: dir, TokenEnv: "X_TOKEN", Token: "t", Prompt: "p",
		Timeout: 10 * time.Second, Label: "test", ExtraEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok},
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "token is [REDACTED]") {
		t.Fatalf("child did not see the env or output not redacted: %v", err)
	}
	if strings.Contains(err.Error(), tok) || strings.Contains(text, tok) {
		t.Fatalf("token leaked: %v / %q", err, text)
	}
}
