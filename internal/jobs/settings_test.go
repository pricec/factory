package jobs

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSettingsRoundTrip: the settings store is a role -> model map -
// absent reads fall through (ok=false), upserts replace, clears are
// idempotent, and AllSettings snapshots everything (spec §6.5,
// 2026-11-15 amendment).
func TestSettingsRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	// Absent: ok=false, never an error (the caller falls back).
	if m, ok, err := store.Setting(ctx, "worker_flash"); err != nil || ok || m != "" {
		t.Fatalf("absent setting = (%q, %v, %v), want (\"\", false, nil)", m, ok, err)
	}

	// Upsert twice: the second write replaces the first.
	for _, m := range []string{"ollama/qwen3:8b", "ollama/gemma3:4b"} {
		if err := store.SetSetting(ctx, "worker_flash", m); err != nil {
			t.Fatalf("SetSetting(%q): %v", m, err)
		}
	}
	if m, ok, err := store.Setting(ctx, "worker_flash"); err != nil || !ok || m != "ollama/gemma3:4b" {
		t.Fatalf("setting after upsert = (%q, %v, %v), want (ollama/gemma3:4b, true, nil)", m, ok, err)
	}

	// A second role round-trips alongside the first.
	if err := store.SetSetting(ctx, "planner", "openrouter/z-ai/glm-5.3"); err != nil {
		t.Fatalf("SetSetting(planner): %v", err)
	}
	all, err := store.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	want := map[string]string{
		"worker_flash": "ollama/gemma3:4b",
		"planner":      "openrouter/z-ai/glm-5.3",
	}
	if len(all) != len(want) || all["worker_flash"] != want["worker_flash"] || all["planner"] != want["planner"] {
		t.Fatalf("AllSettings = %v, want %v", all, want)
	}

	// Clear: gone, and clearing an absent role is a no-op.
	if err := store.ClearSetting(ctx, "worker_flash"); err != nil {
		t.Fatalf("ClearSetting: %v", err)
	}
	if err := store.ClearSetting(ctx, "worker_flash"); err != nil {
		t.Fatalf("ClearSetting(absent): %v", err)
	}
	if _, ok, err := store.Setting(ctx, "worker_flash"); err != nil || ok {
		t.Fatalf("cleared setting = ok %v err %v, want gone", ok, err)
	}
}
