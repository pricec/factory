package foremanapp

import (
	"testing"

	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/wire"
)

func TestClaudeOnlyModelConfig(t *testing.T) {
	t.Parallel()
	glm := modelValues{brain: models.ForemanBrainDefault, flash: models.WorkerFlash, standard: models.WorkerStandard, hardest: models.WorkerHardest}
	m := newModelConfig(glm, true)
	for tier, want := range map[string]string{
		wire.TierFlash: models.ClaudeWorkerFlash, "": models.ClaudeWorkerFlash,
		wire.TierDefault: models.ClaudeWorkerStandard, wire.TierHardest: models.ClaudeWorkerHardest,
	} {
		if got := m.WorkerForTier(tier); got != want {
			t.Errorf("tier %q = %q, want %q", tier, got, want)
		}
	}
	if m.Brain() != models.ClaudeForemanBrain {
		t.Errorf("brain = %q", m.Brain())
	}
	// A panel-set GLM model must not push the foreman back to opencode; a
	// panel-set claude model is honoured.
	m.apply([]settingEntry{
		{Role: "worker_flash", Model: "openrouter/z-ai/glm-5.3-flash", Source: "panel"},
		{Role: "worker_hardest", Model: "claude-opus-4-6", Source: "panel"},
	})
	if got := m.WorkerForTier(wire.TierFlash); got != models.ClaudeWorkerFlash {
		t.Errorf("panel GLM flash = %q", got)
	}
	if got := m.WorkerForTier(wire.TierHardest); got != "claude-opus-4-6" {
		t.Errorf("panel claude hardest = %q", got)
	}
	// Explicit job model overrides: non-claude is coerced, claude kept.
	if got := m.ModelForTier("openrouter/x", wire.TierDefault); got != models.ClaudeWorkerStandard {
		t.Errorf("override = %q", got)
	}
	if got := m.ModelForTier("claude-haiku-5-5", wire.TierDefault); got != "claude-haiku-5-5" {
		t.Errorf("claude override = %q", got)
	}
}

func TestDefaultModelConfigUnchanged(t *testing.T) {
	t.Parallel()
	m := newModelConfig(modelValues{flash: "openrouter/x"}, false)
	if m.WorkerForTier(wire.TierFlash) != "openrouter/x" || m.ModelForTier("openrouter/y", "") != "openrouter/y" {
		t.Fatal("non-pinned foreman must not coerce models")
	}
}

func TestClaudeHarnessTools(t *testing.T) {
	t.Parallel()
	if got := claudeHarnessTools("docker,git,opencode", true); got != "claude,docker,git" {
		t.Errorf("got %q", got)
	}
	if got := claudeHarnessTools("git,opencode", false); got != "git" {
		t.Errorf("got %q", got)
	}
}
