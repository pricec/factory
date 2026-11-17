package wire

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pricec/factory/internal/secrets"
)

// Pinned JobSpec defaults (§5.1, §11).
const (
	// DefaultTimeoutS is the hard wall-clock CEILING (timeout_s) for a job
	// that sets none (4h): since the idle-timeout amendment timeout_s is a
	// backstop, not the working limit - idle_timeout_s is.
	DefaultTimeoutS = 14400
	// DefaultIdleTimeoutS is how long a worker may produce no output
	// (idle_timeout_s) before the harness kills it (15m).
	DefaultIdleTimeoutS = 900
	// DefaultLoopLimit is the loop guard default (identical call+result repeats).
	DefaultLoopLimit       = 8
	DefaultPriority        = 5
	MinPriority            = 1
	MaxPriority            = 9
	DefaultMaxPromptBytes  = 32768
	DefaultMaxOutputTokens = 8192
	// DefaultMaxCostUSD is the default live spend cap. NOT real money: the
	// factory runs on a Claude subscription usage plan, not API billing;
	// this is an API-equivalent cost ESTIMATE standing in for usage-window
	// consumption (see internal/claudecli price table).
	DefaultMaxCostUSD = 20.0
)

// Job tiers (§5.1, §6.5): the model tier a job runs at. Empty means
// TierFlash - the cost-biased default (spec §6.5: dispatchers bias
// towards saving money); `default` is the middle tier and `hardest` the
// strongest.
const (
	TierFlash   = "flash"
	TierDefault = "default"
	TierHardest = "hardest"
)

// Tiers is every allowed tier value (§5.1).
var Tiers = []string{TierFlash, TierDefault, TierHardest}

// tierCeilingS is the per-tier timeout_s ceiling applied by Normalize when
// a spec leaves timeout_s unset (zero) and names a tier.
var tierCeilingS = map[string]int{
	TierFlash:   3600,
	TierDefault: 7200,
	TierHardest: 14400,
}

// TierCeilingS returns the default timeout_s ceiling for tier: flash 1h,
// default 2h, hardest 4h. An empty or unknown tier gets DefaultTimeoutS.
func TierCeilingS(tier string) int {
	if n, ok := tierCeilingS[tier]; ok {
		return n
	}
	return DefaultTimeoutS
}

// Budget bounds a job's prompt size, output size, and spend (§5.1).
type Budget struct {
	MaxPromptBytes  int `json:"max_prompt_bytes"`
	MaxOutputTokens int `json:"max_output_tokens"`
	// MaxCostUSD caps the live spend estimate (default $20). It is an
	// API-equivalent estimate standing in for subscription usage-window
	// consumption (we do not pay per token), not an invoice amount.
	MaxCostUSD float64 `json:"max_cost_usd"`
}

func defaultBudget() Budget {
	return Budget{
		MaxPromptBytes:  DefaultMaxPromptBytes,
		MaxOutputTokens: DefaultMaxOutputTokens,
		MaxCostUSD:      DefaultMaxCostUSD,
	}
}

// JobSpec is the job contract carried by job.request envelopes (§5.1).
type JobSpec struct {
	ID           string          `json:"id,omitempty"`
	Task         string          `json:"task"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	ResultSchema json.RawMessage `json:"result_schema,omitempty"`
	Requires     []string        `json:"requires,omitempty"`
	// Secrets names credentials-store entries (spec §13.2, 2026-11-17
	// amendment) the job needs as env vars in its worker child. NAMES
	// ONLY: values never enter the spec, job.json, prompts or envelopes.
	Secrets []string `json:"secrets,omitempty"`
	// Foreman is the explicit routing pin (§6.7, 2026-11-16
	// foreman-pin amendment): when set, the job dispatches to that
	// foreman instead of the Matcher's capability pick. Full
	// `foreman:{id}` form at rest (the create paths normalize both
	// forms via registry.InboxID); requires stay informational on a
	// pinned job - the pin is the routing decision and is never
	// capability-matched.
	Foreman string `json:"foreman,omitempty"`
	Tier    string `json:"tier,omitempty"`
	// TimeoutS is the hard wall-clock ceiling in seconds; the job is killed
	// (reason=ceiling) when it passes even if it is still producing output.
	TimeoutS int `json:"timeout_s"`
	// IdleTimeoutS kills the job (reason=idle) after this many seconds
	// without any worker output; any output resets the timer. Zero means
	// DefaultIdleTimeoutS; it never exceeds the ceiling in effect.
	IdleTimeoutS int `json:"idle_timeout_s,omitempty"`
	// LoopLimit is the loop guard's threshold: that many consecutive tool
	// calls with identical name, input AND result kill the job
	// (reason=loop). 0 means DefaultLoopLimit (8); negative disables it.
	LoopLimit int    `json:"loop_limit,omitempty"`
	Priority  int    `json:"priority"`
	Budget    Budget `json:"budget"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	// ParentJobID is set on child jobs spawned by a worker via the
	// worker-MCP spawn_child tool (§9.2, 2026-11-15 sub-spawning
	// amendment): it names the parent job, whose per-job inbox receives a
	// copy of the child's terminal job.result. Empty for
	// factory-dispatched and brain-spawned jobs; spawn_child is the only
	// writer, and it refuses to spawn from a job that is itself a child
	// (max 1 level, §9.2).
	ParentJobID string `json:"parent_job_id,omitempty"`
}

// NewJobSpec returns a JobSpec for task with a fresh id, created_at set to
// now, and the pinned defaults applied (§5.1).
func NewJobSpec(task, createdBy string) JobSpec {
	return JobSpec{
		ID:        NewJobID(),
		Task:      task,
		TimeoutS:  DefaultTimeoutS,
		Priority:  DefaultPriority,
		Budget:    defaultBudget(),
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// Normalize applies the pinned defaults to fields left at zero, for specs
// decoded from JSON (§5.1, §11). It does not assign id or created_at;
// schedule templates get those when the schedule fires (§6.8).
func (s *JobSpec) Normalize() {
	if s.TimeoutS == 0 {
		s.TimeoutS = TierCeilingS(s.Tier)
	}
	if s.IdleTimeoutS == 0 {
		s.IdleTimeoutS = DefaultIdleTimeoutS
	}
	if s.Priority == 0 {
		s.Priority = DefaultPriority
	}
	if s.Budget.MaxPromptBytes == 0 {
		s.Budget.MaxPromptBytes = DefaultMaxPromptBytes
	}
	if s.Budget.MaxOutputTokens == 0 {
		s.Budget.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if s.Budget.MaxCostUSD == 0 {
		s.Budget.MaxCostUSD = DefaultMaxCostUSD
	}
}

// Validate checks the spec: id present with the job_ prefix, task non-empty,
// timeout_s > 0, priority 1-9, requires entries non-empty, budget fields
// > 0, and payload / result_schema well-formed when present (§5.1, §5.3).
func (s *JobSpec) Validate() error {
	return s.validate(true)
}

// ValidateTemplate validates a JobSpec used as a schedule template (§6.8): it
// applies the pinned defaults to unset fields and allows id and created_at to
// be empty, since they are assigned when the schedule fires. If an id is
// present it must still be job_-prefixed.
func ValidateTemplate(s *JobSpec) error {
	s.Normalize()
	return s.validate(false)
}

func (s *JobSpec) validate(requireID bool) error {
	if (requireID || s.ID != "") && !isJobID(s.ID) {
		return fmt.Errorf("id: must be a %s-prefixed id, got %q", JobIDPrefix, s.ID)
	}
	if s.Task == "" {
		return fmt.Errorf("task: required")
	}
	if s.TimeoutS <= 0 {
		return fmt.Errorf("timeout_s: must be > 0, got %d", s.TimeoutS)
	}
	if s.IdleTimeoutS < 0 {
		return fmt.Errorf("idle_timeout_s: must be >= 0, got %d", s.IdleTimeoutS)
	}
	if s.Priority < MinPriority || s.Priority > MaxPriority {
		return fmt.Errorf("priority: must be %d-%d, got %d", MinPriority, MaxPriority, s.Priority)
	}
	for _, r := range s.Requires {
		if r == "" {
			return fmt.Errorf("requires: entries must be non-empty")
		}
	}
	for _, n := range s.Secrets {
		if err := secrets.ValidateName(n); err != nil {
			return fmt.Errorf("secrets: %w", err)
		}
	}
	// The pin is normalized to the full foreman:{id} form by the create
	// paths before Validate (§6.7); a whitespace-only pin can only come
	// from a hand-crafted spec and would silently pin nothing.
	if s.Foreman != "" && strings.TrimSpace(s.Foreman) == "" {
		return fmt.Errorf("foreman: must be a foreman id, got %q", s.Foreman)
	}
	if s.ParentJobID != "" {
		if !isJobID(s.ParentJobID) {
			return fmt.Errorf("parent_job_id: must be a %s-prefixed id, got %q", JobIDPrefix, s.ParentJobID)
		}
		if s.ParentJobID == s.ID {
			return fmt.Errorf("parent_job_id: must differ from the job's own id")
		}
	}
	switch s.Tier {
	case "", TierFlash, TierDefault, TierHardest:
	default:
		return fmt.Errorf("tier: must be one of %v, got %q", Tiers, s.Tier)
	}
	if s.Budget.MaxPromptBytes <= 0 || s.Budget.MaxOutputTokens <= 0 || s.Budget.MaxCostUSD <= 0 {
		return fmt.Errorf("budget: fields must be > 0, got %+v", s.Budget)
	}
	if len(s.Payload) > 0 && !json.Valid(s.Payload) {
		return fmt.Errorf("payload: not valid JSON")
	}
	if len(s.ResultSchema) > 0 {
		if err := ValidateSchema(s.ResultSchema); err != nil {
			return fmt.Errorf("result_schema: %w", err)
		}
	}
	return nil
}
