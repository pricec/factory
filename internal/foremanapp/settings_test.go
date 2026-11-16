package foremanapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/wire"
)

// TestModelConfigApply: the foreman's model holder folds fetched
// settings in with the pinned precedence - panel value > env seed >
// pinned default (spec §6.5, 2026-11-15 amendment): a panel-set role
// adopts the fetched model, a cleared role (source "default") reverts
// to its seed, and unknown roles (the planner is the factory's
// business) are ignored.
func TestModelConfigApply(t *testing.T) {
	seeds := modelValues{
		brain:    "env/brain",
		flash:    models.WorkerFlash,
		standard: models.WorkerStandard,
		hardest:  models.WorkerHardest,
	}
	m := newModelConfig(seeds)

	// Tier mapping over the seeds (spec §5.1: no-tier runs flash).
	if got := m.WorkerForTier(""); got != seeds.flash {
		t.Errorf("no-tier = %q, want seed flash %q", got, seeds.flash)
	}
	if got := m.WorkerForTier(wire.TierDefault); got != seeds.standard {
		t.Errorf("default tier = %q, want seed standard %q", got, seeds.standard)
	}
	if got := m.WorkerForTier(wire.TierHardest); got != seeds.hardest {
		t.Errorf("hardest tier = %q, want seed hardest %q", got, seeds.hardest)
	}
	if got := m.Brain(); got != "env/brain" {
		t.Errorf("brain = %q, want env seed", got)
	}

	// Panel values override their roles; a "default"-sourced row clears
	// back to the seed; the planner row is not the foreman's business.
	m.apply([]settingEntry{
		{Role: "planner", Model: "factory-only/ignored", Source: "panel"},
		{Role: "worker_flash", Model: "ollama/qwen3:8b", Source: "panel"},
		{Role: "worker_hardest", Model: "openrouter/z-ai/glm-5.3", Source: "panel"},
		{Role: "foreman_brain", Model: "nonsense/cleared", Source: "default"},
	})
	if got := m.WorkerForTier(""); got != "ollama/qwen3:8b" {
		t.Errorf("flash after apply = %q, want the panel value", got)
	}
	if got := m.WorkerForTier(wire.TierHardest); got != "openrouter/z-ai/glm-5.3" {
		t.Errorf("hardest after apply = %q, want the panel value", got)
	}
	if got := m.WorkerForTier(wire.TierDefault); got != seeds.standard {
		t.Errorf("untouched standard = %q, want its seed %q", got, seeds.standard)
	}
	if got := m.Brain(); got != "env/brain" {
		t.Errorf("cleared brain = %q, want reverted to the env seed", got)
	}

	// A later fetch with no panel values reverts everything to seeds.
	m.apply(nil)
	if got := m.WorkerForTier(""); got != seeds.flash {
		t.Errorf("flash after revert = %q, want seed %q", got, seeds.flash)
	}
}

// settingsSrv serves canned GET /v1/settings responses (the factoryapp
// response shape).
func settingsSrv(t *testing.T, rows []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/settings" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"roles": rows})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchSettings: the pull applies the factory's listing to the
// holder - after one fetch, BuildHarnessConfig resolves worker tiers
// from the fetched values (the settings panel's live path), the brain's
// per-session model follows the holder, and a failing factory leaves
// the current values untouched. No test ever runs the loop goroutine
// against a real factory; fetchSettings is the loop's one step.
func TestFetchSettings(t *testing.T) {
	ctx := context.Background()

	// Boot a paired foreman pointed at a settings server (the
	// loadOrPair shortcut of tiering_test.go: a stored paired.json).
	boot := func(url string) *App {
		dataDir := t.TempDir()
		paired := `{"bearer":"test-bearer","inbox_id":"foreman:alpha","parent_url":"` + url + `"}`
		if err := os.WriteFile(filepath.Join(dataDir, "paired.json"), []byte(paired), 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := New(Config{FactoryURL: url, ID: "alpha", DataDir: dataDir, Addr: "127.0.0.1:0"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = a.Close() })
		return a
	}

	// Empty listing: everything stays at the pinned defaults; the brain
	// resolves per session via ModelFn while its static Model field
	// keeps the boot value (back-compat).
	a := boot("http://factory.invalid")
	a.factoryURL = settingsSrv(t, nil).URL
	if err := a.fetchSettings(ctx); err != nil {
		t.Fatalf("fetchSettings: %v", err)
	}
	if got := a.brain.currentModel(); got != models.ForemanBrainDefault {
		t.Errorf("brain currentModel = %q, want the pinned default %q", got, models.ForemanBrainDefault)
	}
	spec := wire.NewJobSpec("settings sync smoke", "test")
	if got := a.BuildHarnessConfig(spec, "bearer").Model; got != models.WorkerFlash {
		t.Errorf("harness model = %q, want the pinned default %q", got, models.WorkerFlash)
	}

	// Panel-set flash default: the harness resolution follows within one
	// fetch, and the explicit per-job model still wins (spec §5.1).
	const want = "ollama/gemma3:4b"
	a.factoryURL = settingsSrv(t, []map[string]any{
		{"role": "worker_flash", "model": want, "default_model": models.WorkerFlash, "source": "panel"},
	}).URL
	if err := a.fetchSettings(ctx); err != nil {
		t.Fatalf("fetchSettings(2): %v", err)
	}
	if got := a.BuildHarnessConfig(spec, "bearer").Model; got != want {
		t.Errorf("harness model after fetch = %q, want the panel value %q", got, want)
	}
	spec.Model = "openrouter/z-ai/glm-5.3"
	if got := a.BuildHarnessConfig(spec, "bearer").Model; got != spec.Model {
		t.Errorf("harness model with explicit job model = %q, want %q", got, spec.Model)
	}
	spec.Model = ""

	// A dead factory leaves the current values untouched.
	a.factoryURL = "http://127.0.0.1:1"
	if err := a.fetchSettings(ctx); err == nil {
		t.Fatal("fetchSettings against a dead factory: want error")
	}
	if got := a.BuildHarnessConfig(spec, "bearer").Model; got != want {
		t.Errorf("harness model after failed fetch = %q, want unchanged %q", got, want)
	}
}
