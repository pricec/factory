package budgets

import (
	"slices"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.PlannerModel != "" || cfg.ForemanBrainModel != "" || cfg.WorkerModel != "" {
		t.Errorf("default models must be empty (opencode defaults): %+v", cfg)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("FACTORY_PLANNER_MODEL", "model-p")
	t.Setenv("FACTORY_FOREMAN_BRAIN_MODEL", "model-fb")
	t.Setenv("FACTORY_WORKER_MODEL", "model-w")
	t.Setenv("FACTORY_DAILY_SPEND_CAP", "50.5")
	t.Setenv("FACTORY_SPEND_OVERRIDE", "1")

	cfg := LoadConfig()
	if cfg.PlannerModel != "model-p" || cfg.ForemanBrainModel != "model-fb" || cfg.WorkerModel != "model-w" {
		t.Errorf("model overrides not applied: %+v", cfg)
	}
	// Deprecated spend env vars are tolerated (ignored) at startup.
	WarnDeprecatedEnv()
}

func TestLoadConfigEmptyEnvKeepsDefaults(t *testing.T) {
	t.Setenv("FACTORY_PLANNER_MODEL", "")
	t.Setenv("FACTORY_FOREMAN_BRAIN_MODEL", "")
	t.Setenv("FACTORY_WORKER_MODEL", "")
	t.Setenv("FACTORY_DAILY_SPEND_CAP", "")
	t.Setenv("FACTORY_SPEND_OVERRIDE", "")

	if cfg := LoadConfig(); cfg != DefaultConfig() {
		t.Errorf("empty env changed config: %+v", cfg)
	}
}

func TestLoadConfigModelSwap(t *testing.T) {
	if args := DefaultConfig().ModelArgs(RoleWorker); len(args) != 0 {
		t.Errorf("default worker model must inject nothing, got %v", args)
	}
	t.Setenv("FACTORY_WORKER_MODEL", "glm-4.7")
	args := LoadConfig().ModelArgs(RoleWorker)
	if !slices.Equal(args, []string{"-m", "glm-4.7"}) {
		t.Errorf("model swap: args = %v, want [-m glm-4.7]", args)
	}
	// Other roles keep their defaults.
	if args := LoadConfig().ModelArgs(RolePlanner); len(args) != 0 {
		t.Errorf("planner default must inject nothing, got %v", args)
	}
}

func TestPromptFitsBudget(t *testing.T) {
	cases := []struct {
		name          string
		taskLen       int
		payloadLen    int
		maxPromptByte int
		want          bool
	}{
		{"exactly at cap fits", 100, 100, 328, true},
		{"over by one rejects", 100, 100, 327, false},
		{"boilerplate-only prompt fits at 128", 0, 0, 128, true},
		{"well under cap fits", 10, 50, 1000, true},
		{"zero cap rejects any prompt", 1, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := strings.Repeat("x", c.taskLen)
			payload := []byte(strings.Repeat("y", c.payloadLen))
			got := PromptFitsBudget(task, payload, c.maxPromptByte)
			if got != c.want {
				t.Errorf("PromptFitsBudget(task %dB, payload %dB, cap %d) = %v, want %v",
					c.taskLen, c.payloadLen, c.maxPromptByte, got, c.want)
			}
		})
	}
}

func TestShouldFlagOverBudget(t *testing.T) {
	cases := []struct {
		name  string
		usage wire.Usage
		max   float64
		want  bool
	}{
		{"below max", wire.Usage{CostUSD: 5}, 5.01, false},
		{"exactly at max", wire.Usage{CostUSD: 5}, 5, false},
		{"above max flags", wire.Usage{CostUSD: 5}, 4.99, true},
		{"zero cost at zero max", wire.Usage{}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldFlagOverBudget(c.usage, c.max); got != c.want {
				t.Errorf("ShouldFlagOverBudget(%+v, %v) = %v, want %v", c.usage, c.max, got, c.want)
			}
		})
	}
}
