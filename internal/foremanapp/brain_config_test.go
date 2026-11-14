package foremanapp

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestBrainConfigPermissions pins the generated brain-child config's
// permission policy: allow-all, because a headless `opencode run` has no
// client to answer an "ask" and an unanswered ask aborts the whole session.
func TestBrainConfigPermissions(t *testing.T) {
	t.Parallel()
	b := &BrainRunner{
		Model:   "google/gemini-4-hydrogen",
		MCPURL:  "http://foreman.test/v1/mcp/foreman",
		Bearer:  "brain-secret",
		WorkDir: t.TempDir(),
	}
	path, err := b.writeConfig()
	if err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal(%s): %v", data, err)
	}
	want := []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"}}
	if !reflect.DeepEqual(got["permissions"], want) {
		t.Errorf("permissions = %v, want %v", got["permissions"], want)
	}
	if got["mcp"] == nil {
		t.Errorf("generated config lost the mcp block:\n%s", data)
	}
}
