package workerharness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

// TestRunSecretEnv: SecretEnv reaches the runner child's environment, and
// the harness neither persists the value (job.json, generated config) nor
// lets it into the posted result when the agent echoes it (redaction).
func TestRunSecretEnv(t *testing.T) {
	t.Parallel()
	const val = "HARNESS-SECRET-3d9a51"

	dir := t.TempDir()
	spec, specPath := writeSpec(t, dir, func(s *wire.JobSpec) { s.Secrets = []string{"github-push"} })
	binDir := fakeOpencode(t, `env > env.txt
printf 'leaked %s and again %s' "$GITHUB_TOKEN" "$GITHUB_TOKEN"
printf 'stderr %s' "$GITHUB_TOKEN" >&2`)
	rec := &recordingPost{}
	cfg := testConfig(t, specPath, binDir, rec.post)
	cfg.SecretEnv = map[string]string{"GITHUB_TOKEN": val}

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	jobDir := filepath.Join(cfg.WorkDir, jobsDirName, spec.ID)
	envDump, err := os.ReadFile(filepath.Join(jobDir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envDump), "GITHUB_TOKEN="+val) {
		t.Errorf("child env lacks GITHUB_TOKEN; secrets must reach the runner")
	}
	// Persisted artifacts the harness wrote: no value anywhere (env.txt is
	// the fake child's own dump, excluded).
	for _, p := range []string{specPath, filepath.Join(jobDir, configFileName)} {
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), val) {
			t.Errorf("%s contains the secret value", p)
		}
	}
	if rec.calls() != 1 {
		t.Fatalf("posts = %d, want 1", rec.calls())
	}
	b, _ := json.Marshal(rec.last())
	if strings.Contains(string(b), val) {
		t.Errorf("posted result leaked the value: %s", b)
	}
	if !strings.Contains(string(b), redactedMarker) {
		t.Errorf("posted result lacks the redaction marker: %s", b)
	}
}

// TestRedactSecrets: table for the best-effort scrubber.
func TestRedactSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
		env            map[string]string
	}{
		{"no secrets", "abc", "abc", nil},
		{"one", "tok=abc123;", "tok=[REDACTED];", map[string]string{"A": "abc123"}},
		{"repeated", "x x", "[REDACTED] [REDACTED]", map[string]string{"A": "x"}},
		{"longest first", "abcdef", "[REDACTED]", map[string]string{"A": "abc", "B": "abcdef"}},
		{"empty value ignored", "abc", "abc", map[string]string{"A": ""}},
	}
	for _, tc := range tests {
		if got := redactSecrets(tc.env, tc.in); got != tc.want {
			t.Errorf("%s: redactSecrets(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
