package factoryapp

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestPlannerConfigPermissions pins the generated planner-child config's
// permission policy: allow-all, because a headless `opencode run` has no
// client to answer an "ask" and an unanswered ask aborts the whole session
// (the live failure this pins: read/grep outside the workdir declined,
// exit status 1, operator left with an error note).
func TestPlannerConfigPermissions(t *testing.T) {
	t.Parallel()
	b := &PlannerBrain{
		Model:   "google/gemini-4-hydrogen",
		MCPURL:  "http://factory.test/v1/mcp/factory",
		Bearer:  "planner-secret",
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
