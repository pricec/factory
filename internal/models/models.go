// Package models pins the factory's model tiering (spec §6.5): the
// factory optimizes for cost - worker jobs pick among three GLM tiers
// (OpenRouter, single provider, no per-provider credential sprawl) with
// an explicit bias towards the cheapest one that can do the job, while
// the coordinator runs full-strength models (2026-11-16 planner-claude
// amendment: the factory planner runs Claude Opus 5.5 via the Claude
// Code CLI; the foreman brain stays on full GLM 5.3 per #32 until its
// own claude routing). Every opencode string was verified reachable
// against the running opencode install and the claude string against
// the installed Claude Code CLI (2.1.325); each role stays
// env-overridable (spec §6.5: swapping a role's model is a config
// change only).
package models

// Resolve returns override when set, else fallback: the env-override
// helper every role wiring uses (spec §6.5).
func Resolve(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}

// PlannerDefault is the factory planner's model (2026-11-16
// planner-claude amendment): Claude Opus 5.5 via the Claude Code CLI -
// Claude Code's default model for long-running agentic work, $4/$20
// per M tokens in/out (planner sessions are operator-driven and
// output-light, so the output price barely comes into play). The exact
// snapshot id, not the floating `opus` alias: pinned defaults are exact
// strings; the alias drifts across CLI upgrades (overrides via
// FACTORY_PLANNER_MODEL may use aliases). Was openrouter/z-ai/glm-5.3
// (#32) before the claude move; the foreman brain keeps the GLM pin
// until its own routing.
const PlannerDefault = "claude-opus-5-5"

// ForemanBrainDefault is the foreman brain's model (same coordinator
// tier as the planner, #32).
const ForemanBrainDefault = "openrouter/z-ai/glm-5.3"

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
