package budgets

import (
	"log"
	"os"
)

// Roles in the model tiering map (spec §6.5).
const (
	RolePlanner      = "planner"
	RoleForemanBrain = "foreman_brain"
	RoleWorker       = "worker"
)

// Env override names (spec §6.5). Config-file wiring lands with the cmd/
// integration; swapping a role's model is an env change only for now.
const (
	envPlannerModel      = "FACTORY_PLANNER_MODEL"
	envForemanBrainModel = "FACTORY_FOREMAN_BRAIN_MODEL"
	envWorkerModel       = "FACTORY_WORKER_MODEL"
	envDailySpendCap     = "FACTORY_DAILY_SPEND_CAP" // deprecated, ignored
	envSpendOverride     = "FACTORY_SPEND_OVERRIDE"  // deprecated, ignored
)

// Config is the #13 model-tiering configuration. Empty model
// strings mean the opencode default (nothing injected).
type Config struct {
	PlannerModel      string
	ForemanBrainModel string
	WorkerModel       string
}

// DefaultConfig returns the pinned defaults: opencode-default models.
func DefaultConfig() Config {
	return Config{}
}

// LoadConfig returns DefaultConfig with env overrides applied: the three
// model strings. The dollar spend cap is gone (the usage gate is the only
// limit); FACTORY_DAILY_SPEND_CAP / FACTORY_SPEND_OVERRIDE are ignored.
func LoadConfig() Config {
	cfg := DefaultConfig()
	if v := os.Getenv(envPlannerModel); v != "" {
		cfg.PlannerModel = v
	}
	if v := os.Getenv(envForemanBrainModel); v != "" {
		cfg.ForemanBrainModel = v
	}
	if v := os.Getenv(envWorkerModel); v != "" {
		cfg.WorkerModel = v
	}
	return cfg
}

// Model returns the model string configured for role; "" (the opencode
// default) when the role has none or is unknown.
func (c Config) Model(role string) string {
	switch role {
	case RolePlanner:
		return c.PlannerModel
	case RoleForemanBrain:
		return c.ForemanBrainModel
	case RoleWorker:
		return c.WorkerModel
	}
	return ""
}

// ModelArgs returns the opencode invocation args injecting role's model
// (spec §8.4: model strings per role come from the tiering config), or nil
// when the role uses the opencode default - swapping a model changes only
// this string.
func (c Config) ModelArgs(role string) []string {
	m := c.Model(role)
	if m == "" {
		return nil
	}
	return []string{"-m", m}
}

// WarnDeprecatedEnv logs once at startup when the removed daily-spend-cap
// env vars are still set. They are ignored.
func WarnDeprecatedEnv() {
	for _, k := range []string{envDailySpendCap, envSpendOverride} {
		if os.Getenv(k) != "" {
			log.Printf("factory: %s is deprecated and ignored: the dollar spend cap was removed; the usage throttle gate is the only limit", k)
		}
	}
}
