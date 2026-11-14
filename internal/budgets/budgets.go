// Package budgets implements the #13 enforcement proxies and the daily
// spend circuit breaker (spec §6.5): v1 is honest about limits - true token
// spend is not enforceable mid-run, so jobs are bounded by prompt bytes
// pre-dispatch and post-hoc usage accounting, with a config-capped daily
// spend breaker guarding create_job.
package budgets

import (
	"fmt"

	"github.com/pricec/factory/internal/wire"
)

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

// SpendCapError is the breaker's refusal; Code is the pinned wire error
// code (spec §10).
type SpendCapError struct {
	Cap   float64
	Spend float64
}

func (e *SpendCapError) Error() string {
	return fmt.Sprintf("daily spend $%.2f exceeds cap $%.2f", e.Spend, e.Cap)
}

// Code is the pinned wire error code (spec §10).
func (e *SpendCapError) Code() string { return "spend_cap_exceeded" }

// Breaker is the daily spend circuit breaker (spec §6.5): above the config
// cap the factory refuses new create_job. It is deliberately stateless -
// the caller sums the day's spend (from the jobs store) on each call; a
// reset is a config bump or FACTORY_SPEND_OVERRIDE=1 (surfaced by
// LoadConfig). Immutable after construction, so it is safe for concurrent
// use.
type Breaker struct {
	capUSD   float64
	override bool
}

// NewBreaker builds a breaker over capUSD; override disables it.
func NewBreaker(capUSD float64, override bool) *Breaker {
	return &Breaker{capUSD: capUSD, override: override}
}

// Allow refuses spendToday above the cap unless overridden; exactly at the
// cap is allowed. The refusal is a *SpendCapError.
func (b *Breaker) Allow(spendToday float64) error {
	if b.override || spendToday <= b.capUSD {
		return nil
	}
	return &SpendCapError{Cap: b.capUSD, Spend: spendToday}
}
