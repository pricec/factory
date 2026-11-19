package models_test

import (
	"testing"

	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/wire"
)

// TestFableAllowlist: Fable is a valid explicit job model (spec model
// validation) and recognised by IsFable, bare or provider-prefixed.
func TestFableAllowlist(t *testing.T) {
	t.Parallel()
	if models.Fable != "claude-fable-5-2" {
		t.Fatalf("Fable = %q", models.Fable)
	}
	for _, m := range []string{models.Fable, "anthropic/" + models.Fable} {
		if !models.IsFable(m) {
			t.Errorf("IsFable(%q) = false", m)
		}
		spec := wire.NewJobSpec("x", "test")
		spec.Model = m
		if err := spec.Validate(); err != nil {
			t.Errorf("create_job model %q rejected: %v", m, err)
		}
	}
	for _, m := range []string{"", "claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-20", "openrouter/z-ai/glm-5.3"} {
		if models.IsFable(m) {
			t.Errorf("IsFable(%q) = true", m)
		}
	}
}

// TestTiersNeverResolveToFable: no tier, role default or Claude-harness
// default is Fable; it is reachable only via an explicit model.
func TestTiersNeverResolveToFable(t *testing.T) {
	t.Parallel()
	all := map[string]string{
		"PlannerDefault": models.PlannerDefault, "ForemanBrainDefault": models.ForemanBrainDefault,
		"WorkerFlash": models.WorkerFlash, "WorkerStandard": models.WorkerStandard, "WorkerHardest": models.WorkerHardest,
		"ClaudeWorkerFlash": models.ClaudeWorkerFlash, "ClaudeWorkerStandard": models.ClaudeWorkerStandard,
		"ClaudeWorkerHardest": models.ClaudeWorkerHardest, "ClaudeForemanBrain": models.ClaudeForemanBrain,
	}
	for name, m := range all {
		if models.IsFable(m) {
			t.Errorf("%s resolves to Fable", name)
		}
	}
}
