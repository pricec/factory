package foremanapp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/occonfig"
)

// TestBrainConfig pins the generated brain-child config: it must land at
// the V2 project-config path (WorkDir/opencode.json - the V1-era
// OPENCODE_CONFIG env var is ignored by V2), carry the allow-all
// permissions (a headless run has no client to answer an ask), and pin
// the model.
func TestBrainConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := &BrainRunner{
		Model:   models.ForemanBrainDefault,
		MCPURL:  "http://foreman.test/v1/mcp/foreman",
		Bearer:  "brain-secret",
		WorkDir: dir,

		OpenCODEVersion: occonfig.V2,
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
	if got["model"] != models.ForemanBrainDefault {
		t.Errorf("model = %v, want %q", got["model"], models.ForemanBrainDefault)
	}
	wantPerms := []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"}}
	if !reflect.DeepEqual(got["permissions"], wantPerms) {
		t.Errorf("permissions = %v, want %v", got["permissions"], wantPerms)
	}
	if got["mcp"] == nil {
		t.Errorf("generated config lost the mcp block:\n%s", data)
	}
	if strings.Contains(string(data), "brain-secret") {
		t.Errorf("generated config leaks the bearer at rest:\n%s", data)
	}
}

// TestBrainTimeoutBound pins the brain session bound at 30m (2026-11-15
// operator decision; previously 5m). Guards against an accidental revert
// to the old 300s constant while keeping the bound finite.
func TestBrainTimeoutBound(t *testing.T) {
	t.Parallel()
	if brainTimeout != 30*time.Minute {
		t.Errorf("brainTimeout = %v, want 30m (1800s)", brainTimeout)
	}
}

// TestBrainConfigV1 pins the V1 shape the brain child gets on a V1 host:
// "permission" rules, no V2 "permissions" array, server directly under mcp.
func TestBrainConfigV1(t *testing.T) {
	t.Parallel()
	b := &BrainRunner{
		Model: models.ForemanBrainDefault, MCPURL: "http://foreman.test/v1/mcp/foreman",
		Bearer: "brain-secret", WorkDir: t.TempDir(), OpenCODEVersion: occonfig.V1,
	}
	path, err := b.writeConfig()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["permissions"]; ok {
		t.Errorf("V1 config carries V2 permissions:\n%s", data)
	}
	if perm, _ := got["permission"].(map[string]any); perm["edit"] != "allow" || perm["bash"] != "allow" {
		t.Errorf("permission = %v, want allow-all rules", got["permission"])
	}
	if _, ok := got["mcp"].(map[string]any)["foreman"]; !ok {
		t.Errorf("V1 mcp must nest the server directly:\n%s", data)
	}
}
