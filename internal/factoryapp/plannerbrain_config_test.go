package factoryapp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/models"
)

// TestPlannerModelDefault: the planner brain defaults to the pinned GLM
// tiering (spec §6.5) when FACTORY_PLANNER_MODEL is unset.
func TestPlannerModelDefault(t *testing.T) {
	t.Setenv("FACTORY_PLANNER_TOKEN", "test-planner-secret-0123456789")
	a, err := New(Config{DataDir: t.TempDir(), Addr: ":8090", AdvertiseURL: "http://factory.test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	if a.planner.Model != models.PlannerDefault {
		t.Errorf("planner model = %q, want the pinned default %q", a.planner.Model, models.PlannerDefault)
	}
	// The MCP endpoint must be loopback: the MCP SDK's streamable-HTTP
	// server rejects non-loopback Host headers, and the planner child
	// always runs on this host (spec §9.5).
	if a.planner.MCPURL != "http://127.0.0.1:8090/v1/mcp/factory" {
		t.Errorf("planner MCP URL = %q, want the loopback endpoint", a.planner.MCPURL)
	}
}

// TestPlannerConfig pins the generated planner-child config: it must land
// at the V2 project-config path (WorkDir/opencode.json - the V1-era
// OPENCODE_CONFIG env var is ignored by V2, which silently dropped the
// whole config), carry the allow-all permissions (a headless run has no
// client to answer an ask; an unanswered ask aborts the session), and pin
// the model.
func TestPlannerConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := &PlannerBrain{
		Model:   models.PlannerDefault,
		MCPURL:  "http://factory.test/v1/mcp/factory",
		Bearer:  "planner-secret",
		WorkDir: dir,
	}
	path, err := b.writeConfig()
	if err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	if want := filepath.Join(dir, "opencode.json"); path != want {
		t.Errorf("config path = %q, want the V2 project-config path %q", path, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("config perms: %v (%o), want 0600", err, info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal(%s): %v", data, err)
	}
	if got["model"] != models.PlannerDefault {
		t.Errorf("model = %v, want %q", got["model"], models.PlannerDefault)
	}
	wantPerms := []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"}}
	if !reflect.DeepEqual(got["permissions"], wantPerms) {
		t.Errorf("permissions = %v, want %v", got["permissions"], wantPerms)
	}
	if got["mcp"] == nil {
		t.Errorf("generated config lost the mcp block:\n%s", data)
	}
	if contains := string(data); containsSecret(contains, "planner-secret") {
		t.Errorf("generated config leaks the bearer at rest:\n%s", data)
	}
}

func containsSecret(data, secret string) bool {
	return len(secret) >= 6 && strings.Contains(data, secret)
}

// TestPlannerBrainTimeoutBound pins the planner brain session bound at
// 30m (2026-11-15 operator decision; previously 5m). Guards against an
// accidental revert to the old 300s constant while keeping the bound
// finite.
func TestPlannerBrainTimeoutBound(t *testing.T) {
	t.Parallel()
	if plannerBrainTimeout != 30*time.Minute {
		t.Errorf("plannerBrainTimeout = %v, want 30m (1800s)", plannerBrainTimeout)
	}
}
