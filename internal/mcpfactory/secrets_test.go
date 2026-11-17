package mcpfactory_test

import (
	"context"
	"strings"
	"testing"
)

const mcpSecretValue = "MCP-SECRET-VALUE-4be1"

// TestCreateJobSecrets: names are validated at create against the store
// (unknown names fail loudly, listing them, and create nothing); known
// names land in the spec as names only.
func TestCreateJobSecrets(t *testing.T) {
	e := newEnv(t)
	if _, err := e.secrets.Put("github-push", mcpSecretValue, "GITHUB_TOKEN"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		secrets []any
		errHas  []string
	}{
		{"known", []any{"github-push"}, nil},
		{"unknown listed", []any{"github-push", "ghost-b", "ghost-a"}, []string{"unknown_secrets", "ghost-a", "ghost-b"}},
		{"bad shape", []any{"Bad_Name"}, []string{"secrets"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := e.callTool("create_job", map[string]any{"task": "needs creds", "secrets": tc.secrets})
			if tc.errHas != nil {
				msg := toolError(t, "create_job", out)
				for _, w := range tc.errHas {
					if !strings.Contains(msg, w) {
						t.Errorf("error %q missing %q", msg, w)
					}
				}
				if strings.Contains(msg, mcpSecretValue) {
					t.Errorf("error leaked a value: %q", msg)
				}
				return
			}
			sc := structured(t, resultOf(t, out))
			job, ok, err := e.store.Get(context.Background(), sc["job_id"].(string))
			if err != nil || !ok {
				t.Fatalf("get job: ok=%v err=%v", ok, err)
			}
			if len(job.Spec.Secrets) != 1 || job.Spec.Secrets[0] != "github-push" {
				t.Errorf("spec.secrets = %v, want [github-push]", job.Spec.Secrets)
			}
		})
	}
	jobs, err := e.store.List(context.Background(), "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Errorf("%d jobs exist, want 1 (rejected creates make none)", len(jobs))
	}
}
