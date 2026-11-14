package foremanapp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pricec/factory/internal/foremanapp"
	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/wire"
)

// TestModelTieringDefaults: unset models fall back to the pinned GLM
// tiering (spec §6.5), and BuildHarnessConfig maps the job's tier to the
// right model (spec §5.1, §6.5): flash - and no-tier jobs, the
// cost-biased default - run GLM 5.3 Flash, `tier: "default"` runs GLM
// 5.2, `tier: "hardest"` runs GLM 5.3.
func TestModelTieringDefaults(t *testing.T) {
	// Pre-seed paired.json so New never contacts the factory (loadOrPair
	// loads the stored record instead of burning a token).
	dataDir := t.TempDir()
	paired := `{"bearer":"test-bearer","inbox_id":"foreman:alpha","parent_url":"http://factory.invalid"}`
	if err := os.WriteFile(filepath.Join(dataDir, "paired.json"), []byte(paired), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := foremanapp.New(foremanapp.Config{
		FactoryURL: "http://factory.invalid",
		ID:         "alpha",
		DataDir:    dataDir,
		Addr:       "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("foremanapp.New: %v", err)
	}
	defer a.Close()

	cases := []struct {
		name      string
		tier      string
		wantModel string
	}{
		{"unset tier runs the cheap default (cost bias)", "", models.WorkerFlash},
		{"explicit flash tier", wire.TierFlash, models.WorkerFlash},
		{"default tier runs the middle model", wire.TierDefault, models.WorkerStandard},
		{"hardest tier runs the strongest model", wire.TierHardest, models.WorkerHardest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := wire.NewJobSpec("do the thing", "test")
			spec.Tier = tc.tier
			cfg := a.BuildHarnessConfig(spec, "bearer")
			if cfg.Model != tc.wantModel {
				t.Errorf("tier %q: harness model = %q, want %q", tc.tier, cfg.Model, tc.wantModel)
			}
		})
	}
}
