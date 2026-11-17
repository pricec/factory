// Package budgets implements the #13 enforcement proxies (spec §6.5): v1 is
// honest about limits - true token spend is not enforceable mid-run, so jobs
// are bounded by prompt bytes pre-dispatch and post-hoc usage accounting.
// There is no daily dollar breaker; the usage throttle gate is the only
// spending limit.
package budgets

import "github.com/pricec/factory/internal/wire"

// boilerplateBytes is the fixed allowance for the §8.3 prompt boilerplate
// ("Machine input:" framing + the report instruction) on top of task and
// payload JSON.
const boilerplateBytes = 128

// PromptFitsBudget is the prompt-bytes proxy (spec §6.5 proxy 1): the
// foreman rejects a job.request whose assembled prompt (§8.3: task +
// payload JSON + boilerplate) would exceed maxPromptBytes. Exactly at the
// cap fits.
func PromptFitsBudget(task string, payload []byte, maxPromptBytes int) bool {
	return len(task)+len(payload)+boilerplateBytes <= maxPromptBytes
}

// ShouldFlagOverBudget is the post-hoc check (spec §6.5 proxy 4): usage
// recorded from report_result flags the job when its cost exceeds the
// per-job cap. Exactly at the cap is within budget.
func ShouldFlagOverBudget(u wire.Usage, maxCostUSD float64) bool {
	return u.CostUSD > maxCostUSD
}
