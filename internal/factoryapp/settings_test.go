package factoryapp_test

import (
	"net/http"
	"testing"

	"github.com/pricec/factory/internal/models"
)

// settingRow is one role row of the GET/POST /v1/settings response.
type settingRow struct {
	Role         string `json:"role"`
	Model        string `json:"model"`
	DefaultModel string `json:"default_model"`
	Source       string `json:"source"`
}

// settingsResp is the record-wrapped settings response body.
type settingsResp struct {
	Roles []settingRow `json:"roles"`
}

// byRole indexes a settings response by role name.
func byRole(s settingsResp) map[string]settingRow {
	out := map[string]settingRow{}
	for _, r := range s.Roles {
		out[r.Role] = r
	}
	return out
}

// TestSettingsEndpoint: GET /v1/settings shows every managed role with
// its pinned default; POST stores, clears, and leaves absent keys
// untouched; unknown roles and bad model strings are 400s (spec §6.9,
// 2026-11-15 amendment).
func TestSettingsEndpoint(t *testing.T) {
	e := newEnv(t)

	// Initial state: every role at its pinned default, in pinned order.
	var got settingsResp
	if status := e.do("GET", "/v1/settings", "", nil, &got); status != http.StatusOK {
		t.Fatalf("GET /v1/settings status = %d, want 200", status)
	}
	if len(got.Roles) != len(models.Roles) {
		t.Fatalf("roles = %d, want %d", len(got.Roles), len(models.Roles))
	}
	for i, row := range got.Roles {
		want := models.Roles[i]
		if row.Role != want.Name {
			t.Errorf("row %d role = %q, want %q", i, row.Role, want.Name)
		}
		if row.Source != "default" || row.Model != want.Default || row.DefaultModel != want.Default {
			t.Errorf("row %d = %+v, want pinned default %q", i, row, want.Default)
		}
	}

	// Save two roles: the response carries the updated state.
	if status := e.do("POST", "/v1/settings", "", map[string]string{
		"planner":      "openrouter/z-ai/glm-5.3-flash#max",
		"worker_flash": "ollama/qwen3:8b",
	}, &got); status != http.StatusOK {
		t.Fatalf("POST /v1/settings status = %d, want 200", status)
	}
	rows := byRole(got)
	if r := rows["planner"]; r.Model != "openrouter/z-ai/glm-5.3-flash#max" || r.Source != "panel" {
		t.Errorf("planner row = %+v, want the stored value with source panel", r)
	}
	if r := rows["worker_flash"]; r.Model != "ollama/qwen3:8b" || r.Source != "panel" {
		t.Errorf("worker_flash row = %+v, want the stored value with source panel", r)
	}
	if r := rows["worker_default"]; r.Source != "default" {
		t.Errorf("untouched worker_default row = %+v, want source default", r)
	}

	// Persistence: GET shows the same state.
	if status := e.do("GET", "/v1/settings", "", nil, &got); status != http.StatusOK {
		t.Fatalf("GET after save status = %d", status)
	}
	if r := byRole(got)["worker_flash"]; r.Model != "ollama/qwen3:8b" {
		t.Errorf("worker_flash after reload = %+v, want the stored value", r)
	}

	// Clear one role ("" value): back to default; the other stays.
	if status := e.do("POST", "/v1/settings", "", map[string]string{"worker_flash": ""}, &got); status != http.StatusOK {
		t.Fatalf("POST clear status = %d, want 200", status)
	}
	rows = byRole(got)
	if r := rows["worker_flash"]; r.Source != "default" || r.Model != models.WorkerFlash {
		t.Errorf("cleared worker_flash = %+v, want the pinned default", r)
	}
	if r := rows["planner"]; r.Source != "panel" {
		t.Errorf("planner after clear = %+v, want untouched", r)
	}

	// Validation: unknown role and bad model are 400s (loud, not silent).
	for _, tt := range []struct {
		name string
		in   any
	}{
		{"not json", "{"},
		{"unknown role", map[string]string{"gpu": "x"}},
		{"bad model", map[string]string{"planner": "has space"}},
	} {
		var errResp map[string]any
		if status := e.do("POST", "/v1/settings", "", tt.in, &errResp); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tt.name, status)
		}
	}
}

// TestPlannerModelPrecedence: the factory planner's model resolves
// stored panel setting > FACTORY_PLANNER_MODEL env > pinned default
// (spec §6.5, 2026-11-15 amendment), re-resolved per session so panel
// changes apply without a restart.
func TestPlannerModelPrecedence(t *testing.T) {
	t.Setenv("FACTORY_PLANNER_MODEL", "env/planner-model")
	e := newEnv(t)

	if got := e.a.PlannerModel(); got != "env/planner-model" {
		t.Fatalf("planner model with env only = %q, want the env value", got)
	}

	// A panel value wins over the env var...
	if status := e.do("POST", "/v1/settings", "", map[string]string{"planner": "ollama/gemma3:4b"}, nil); status != http.StatusOK {
		t.Fatalf("POST planner status = %d, want 200", status)
	}
	if got := e.a.PlannerModel(); got != "ollama/gemma3:4b" {
		t.Fatalf("planner model with panel value = %q, want the panel value", got)
	}

	// ...and clearing it falls back to the env var, not the default.
	if status := e.do("POST", "/v1/settings", "", map[string]string{"planner": ""}, nil); status != http.StatusOK {
		t.Fatalf("POST clear status = %d, want 200", status)
	}
	if got := e.a.PlannerModel(); got != "env/planner-model" {
		t.Fatalf("planner model after clear = %q, want the env value back", got)
	}
}
