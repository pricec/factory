package workerharness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestGeneratedConfigGolden pins the generated opencode config against the
// verified V2 shape (opencode docs, MCP servers: servers nest under
// mcp.servers; the worker token is referenced via {env:WORKER_TOKEN} so it
// never lands in the file at rest).
func TestGeneratedConfigGolden(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		model string
		want  map[string]any
	}{
		{
			"with model",
			"anthropic/claude-sonnet-4-5",
			map[string]any{
				"model":       "anthropic/claude-sonnet-4-5",
				"permissions": []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"}},
				"mcp": map[string]any{
					"servers": map[string]any{
						"inbox": map[string]any{
							"type":    "remote",
							"url":     "http://foreman:8080/v1/mcp/worker",
							"headers": map[string]any{"Authorization": "Bearer {env:WORKER_TOKEN}"},
							"oauth":   false,
						},
					},
				},
			},
		},
		{
			"without model",
			"",
			map[string]any{
				"permissions": []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"}},
				"mcp": map[string]any{
					"servers": map[string]any{
						"inbox": map[string]any{
							"type":    "remote",
							"url":     "http://foreman:8080/v1/mcp/worker",
							"headers": map[string]any{"Authorization": "Bearer {env:WORKER_TOKEN}"},
							"oauth":   false,
						},
					},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{
				MCPURL: "http://foreman:8080/v1/mcp/worker",
				Token:  "tok123",
				Model:  tc.model,
			}
			data, err := json.Marshal(newOpenCODEConfig(cfg))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal(%s): %v", data, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("generated config =\n%s\nwant structure\n%s", data, mustJSON(t, tc.want))
			}
			if containsSecret(t, data, cfg.Token) {
				t.Errorf("generated config leaks the worker token at rest:\n%s", data)
			}
		})
	}
}

func TestWriteConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{MCPURL: "http://foreman:8080/v1/mcp/worker", Token: "secret-token"}
	path := filepath.Join(t.TempDir(), configFileName)
	if err := writeConfig(path, newOpenCODEConfig(cfg)); err != nil {
		t.Fatalf("writeConfig(): %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %o, want 0600", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if containsSecret(t, data, cfg.Token) {
		t.Errorf("config file contains the raw token: %s", data)
	}
	var got openCODEConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal(%s): %v", data, err)
	}
	srv := got.MCP.Servers["inbox"]
	if srv == nil {
		t.Fatal("generated config has no inbox MCP server")
	}
	if srv.Type != "remote" || srv.URL != cfg.MCPURL {
		t.Errorf("inbox server = %+v, want remote %s", srv, cfg.MCPURL)
	}
	if auth := srv.Headers["Authorization"]; auth != "Bearer {env:WORKER_TOKEN}" {
		t.Errorf("Authorization header = %q, want %q", auth, "Bearer {env:WORKER_TOKEN}")
	}
	if srv.OAuth {
		t.Error("OAuth auto-detection must be disabled")
	}
	if got.Model != "" {
		t.Errorf("model = %q, want empty when unset", got.Model)
	}
	if len(got.Permissions) != 1 || got.Permissions[0] != (permissionRule{Action: "*", Resource: "*", Effect: "allow"}) {
		t.Errorf("permissions = %+v, want one allow-all rule (headless runs die on asks)", got.Permissions)
	}
}

// containsSecret reports whether data contains the raw token. It is
// skipped when the token is too short to be a meaningful secret.
func containsSecret(t *testing.T, data []byte, token string) bool {
	t.Helper()
	if len(token) < 6 {
		return false
	}
	return bytes.Contains(data, []byte(token))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
