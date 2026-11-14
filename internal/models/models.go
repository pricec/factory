// Package models pins the factory's model tiering (spec §6.5): the
// factory standardizes on GLM and optimizes for cost - the overhead roles
// (factory planner, foreman brain) run on GLM 5.3 Flash, workers default
// to GLM 5.2, and jobs marked `tier: "hardest"` run on the strongest GLM
// 5.3. Every string was verified reachable against the running opencode
// install; each role stays env-overridable (spec §6.5: swapping a role's
// model is a config change only).
package models

// Resolve returns override when set, else fallback: the env-override
// helper every role wiring uses (spec §6.5).
func Resolve(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}

// PlannerDefault is the factory planner's model: conversational
// tool-driving over the planner MCP surface; GLM 5.3 Flash is the cheap
// tier for it (openrouter lists ~$0.04/$0.08 per M tokens in/out).
const PlannerDefault = "openrouter/z-ai/glm-5.3-flash"

// ForemanBrainDefault is the foreman brain's model: routing decisions
// over the foreman MCP surface; same cost tier as the planner.
const ForemanBrainDefault = "openrouter/z-ai/glm-5.3-flash"

// WorkerDefault is the worker's default model: GLM 5.2 (openrouter,
// ~$0.007/$6.8 per M tokens in/out).
const WorkerDefault = "openrouter/z-ai/glm-5.2"

// WorkerHardest is the worker's `tier: "hardest"` model: full-strength
// GLM 5.3 (ollama-cloud, ~$1.4/$4.4 per M tokens in/out) for the hardest
// tasks only.
const WorkerHardest = "ollama-cloud/glm-5.3"
