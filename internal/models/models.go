// Package models pins the factory's model tiering (spec §6.5): the
// factory standardizes on GLM via OpenRouter (single provider, no
// per-provider credential sprawl) and optimizes for cost - the overhead
// roles (factory planner, foreman brain) run GLM 5.2, and worker jobs
// pick among three tiers with an explicit bias towards the cheapest one
// that can do the job. Every string was verified reachable against the
// running opencode install; each role stays env-overridable (spec §6.5:
// swapping a role's model is a config change only).
package models

// Resolve returns override when set, else fallback: the env-override
// helper every role wiring uses (spec §6.5).
func Resolve(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}

// PlannerDefault is the factory planner's model (GLM 5.2: the brains run
// at least 5.2; openrouter ~$0.007/$6.8 per M tokens in/out). Brain
// sessions are input-heavy and output-light, so 5.2's rock-bottom input
// and cache-read prices make it the cheapest brain model despite the
// expensive output - the output volume never comes into play.
const PlannerDefault = "openrouter/z-ai/glm-5.2"

// ForemanBrainDefault is the foreman brain's model (same tier as the
// planner).
const ForemanBrainDefault = "openrouter/z-ai/glm-5.2"

// WorkerFlash is the worker's cheap tier (spec §5.1 `tier: "flash"`, also
// the cost-biased default for jobs that pick no tier): GLM 5.3 Flash
// (openrouter, ~$0.04/$0.08 per M tokens).
const WorkerFlash = "openrouter/z-ai/glm-5.3-flash"

// WorkerStandard is the worker's middle tier (spec §5.1
// `tier: "default"`): GLM 5.3 Flash at max reasoning effort
// (openrouter, ~$0.04/$0.08 per M tokens - unchanged from plain flash;
// the only extra spend is the reasoning tokens actually consumed). The
// #max variant MUST ride the -m flag: opencode V2 drops #variant
// suffixes from the config's model field. GLM 5.3 FlashX priced itself
// out of the slot earlier (~$0.37/M input, 4.4x full GLM 5.3's) - the
// factory's jobs run ~99:1 input:output, where FlashX costs more than
// the stronger model.
const WorkerStandard = "openrouter/z-ai/glm-5.3-flash#max"

// WorkerHardest is the worker's `tier: "hardest"` model: full-strength
// GLM 5.3 (openrouter, ~$0.084/$3.6 per M tokens) for the hardest tasks
// only.
const WorkerHardest = "openrouter/z-ai/glm-5.3"
