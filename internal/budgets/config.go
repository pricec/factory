package budgets

import (
	"os"
	"strconv"
)

// Roles in the model tiering map (spec §6.5).
const (
	RolePlanner      = "planner"
	RoleForemanBrain = "foreman_brain"
	RoleWorker       = "worker"
)

// Env override names (spec §6.5: env-overridable; FACTORY_SPEND_OVERRIDE=1
// bypasses the breaker). Config-file wiring lands with the cmd/
// integration; swapping a role's model is an env change only for now.
const (
	envPlannerModel      = "FACTORY_PLANNER_MODEL"
	envForemanBrainModel = "FACTORY_FOREMAN_BRAIN_MODEL"
	envWorkerModel       = "FACTORY_WORKER_MODEL"
	envDailySpendCap     = "FACTORY_DAILY_SPEND_CAP"
	envSpendOverride     = "FACTORY_SPEND_OVERRIDE"
)

// Config is the #13 model-tiering + spend-cap configuration. Empty model
// strings mean the opencode default (nothing injected).
type Config struct {
	PlannerModel      string
	ForemanBrainModel string
	WorkerModel       string
	DailySpendCapUSD  float64
	// SpendOverride disables the daily spend breaker (spec §6.5).
	SpendOverride bool
}

// DefaultConfig returns the pinned defaults: opencode-default models and
// a $20/day spend cap (spec §6.5, §11).
func DefaultConfig() Config {
	return Config{DailySpendCapUSD: 20}
}

// LoadConfig returns DefaultConfig with env overrides applied: the three
// model strings, FACTORY_DAILY_SPEND_CAP (unparsable values keep the
// default), and FACTORY_SPEND_OVERRIDE="1" disabling the breaker.
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
	if v := os.Getenv(envDailySpendCap); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.DailySpendCapUSD = f
		}
	}
	cfg.SpendOverride = os.Getenv(envSpendOverride) == "1"
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
