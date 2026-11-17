package mcpfactory_test

import (
	"context"
	"strings"
	"testing"
)

// TestCreateJobFactoryRepoNeedsPin: an unpinned job naming the factory repo
// or its worktrees is rejected; pinning foreman:server passes; pinning
// another foreman passes with a warning; unrelated tasks are untouched.
func TestCreateJobFactoryRepoNeedsPin(t *testing.T) {
	tests := []struct {
		name     string
		task     string
		foreman  string
		wantErr  bool
		wantWarn bool
	}{
		{"slug unpinned", "Fix a bug in GitHub pricec/factory", "", true, false},
		{"worktree unpinned", "edit /home/price/work/factory-auditor/internal/x.go", "", true, false},
		{"operator checkout unpinned", "look at /home/price/temp/factory", "", true, false},
		{"case-insensitive", "see PRICEC/Factory", "", true, false},
		{"pinned server", "fix pricec/factory", "foreman:server", false, false},
		{"pinned server bare id", "fix pricec/factory", "server", false, false},
		{"pinned elsewhere warns", "fix pricec/factory", "alpha", false, true},
		{"unrelated unpinned", "build the widget", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			if err := e.store.UpsertForeman(ctx, "server", []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"task": tc.task}
			if tc.foreman != "" {
				args["foreman"] = tc.foreman
			}
			out := e.callTool("create_job", args)
			if tc.wantErr {
				if s := toolError(t, "create_job", out); !strings.Contains(s, "factory_repo_needs_pin") || !strings.Contains(s, "foreman:server") {
					t.Errorf("error %q does not tell the caller to pin foreman:server", s)
				}
				return
			}
			sc := structured(t, resultOf(t, out))
			_, warned := sc["warnings"]
			if tc.wantWarn && !warned {
				t.Errorf("want a warning, got %v", sc)
			}
			if !tc.wantWarn && tc.foreman == "foreman:server" && warned {
				t.Errorf("unexpected warnings %v", sc["warnings"])
			}
		})
	}
}
