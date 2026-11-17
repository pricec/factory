package foremanapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// settingsRefreshDefault is the settings pull cadence (spec §6.5,
// 2026-11-15 amendment): foremen re-read the factory's role-model
// settings on startup and every 60s, mirroring the capabilities refresh
// (§6.7). A panel change reaches a foreman's worker/brain spawns within
// one interval; jobs dispatched in the window run with the previous
// defaults. Config.SettingsRefresh overrides it (test seam).
const settingsRefreshDefault = 60 * time.Second

// settingsFetchTimeout bounds one GET /v1/settings: a slow factory must
// never stall the loop goroutine.
const settingsFetchTimeout = 10 * time.Second

// modelValues is one snapshot of the foreman-resolved models for the
// roles the foreman runs (spec §6.5): the brain plus the three worker
// tiers.
type modelValues struct {
	brain    string
	flash    string
	standard string
	hardest  string
}

// modelConfig is the foreman's mutable model resolution (spec §6.5,
// 2026-11-15 amendment): seeds are the env-resolved boot values
// (FACTORY_FOREMAN_BRAIN_MODEL / FACTORY_WORKER_* > pinned defaults,
// resolved once in New); current is what spawns use, updated by the
// settings pull - a panel-set role (source "panel") wins, a cleared role
// reverts to its seed (env > default). Precedence per role: panel >
// env > pinned default.
type modelConfig struct {
	mu      sync.RWMutex
	seeds   modelValues
	current modelValues
}

func newModelConfig(seeds modelValues) *modelConfig {
	return &modelConfig{seeds: seeds, current: seeds}
}

// Brain returns the brain's current model.
func (m *modelConfig) Brain() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current.brain
}

// WorkerForTier maps a job tier to the worker's current model (spec
// §5.1, §6.5): flash - and no-tier jobs, the cost-biased default - run
// the flash model, `default` the standard one, `hardest` the strongest.
func (m *modelConfig) WorkerForTier(tier string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	switch tier {
	case wire.TierDefault:
		return m.current.standard
	case wire.TierHardest:
		return m.current.hardest
	default:
		return m.current.flash
	}
}

// apply folds one fetched settings listing into the current values: a
// panel-set role adopts the fetched model; anything else (cleared or
// never set) reverts to its env/default seed. Unknown roles are ignored
// (the planner role is the factory's business).
func (m *modelConfig) apply(rows []settingEntry) {
	next := m.seeds
	for _, r := range rows {
		if r.Source != "panel" {
			continue
		}
		switch r.Role {
		case "foreman_brain":
			next.brain = r.Model
		case "worker_flash":
			next.flash = r.Model
		case "worker_default":
			next.standard = r.Model
		case "worker_hardest":
			next.hardest = r.Model
		}
	}
	m.mu.Lock()
	m.current = next
	m.mu.Unlock()
}

// settingEntry is one role row of GET /v1/settings (the factoryapp shape).
type settingEntry struct {
	Role         string `json:"role"`
	Model        string `json:"model"`
	DefaultModel string `json:"default_model"`
	Source       string `json:"source"`
}

// fetchSettings pulls the factory's role-model settings (spec §6.9) and
// applies them. Errors leave the current values untouched - the loop
// logs and retries on the next tick, and a foreman that started before
// the factory keeps its env/default seeds until the first success.
func (a *App) fetchSettings(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.factoryURL+"/v1/settings", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /v1/settings: %s", resp.Status)
	}
	var out struct {
		Roles []settingEntry `json:"roles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	a.modelCfg.apply(out.Roles)
	return nil
}

// settingsLoop keeps the foreman's role models in sync with the
// factory's settings store (spec §6.5, 2026-11-15 amendment): one fetch
// at startup, then a ticker at cfg.SettingsRefresh (default 60s).
func (a *App) settingsLoop(ctx context.Context) {
	interval := a.cfg.SettingsRefresh
	if interval <= 0 {
		interval = settingsRefreshDefault
	}
	fetch := func() {
		fctx, cancel := context.WithTimeout(ctx, settingsFetchTimeout)
		defer cancel()
		if err := a.fetchSettings(fctx); err != nil {
			log.Printf("foremanapp: settings refresh: %v (keeping current models)", err)
		}
	}
	fetch()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetch()
		}
	}
}
