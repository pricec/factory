package wire

import (
	"encoding/json"
	"fmt"
	"time"
)

// Pinned JobSpec defaults (§5.1, §11).
const (
	DefaultTimeoutS        = 300
	DefaultPriority        = 5
	MinPriority            = 1
	MaxPriority            = 9
	DefaultMaxPromptBytes  = 32768
	DefaultMaxOutputTokens = 8192
	DefaultMaxCostUSD      = 5.0
)

// Budget bounds a job's prompt size, output size, and spend (§5.1).
type Budget struct {
	MaxPromptBytes  int     `json:"max_prompt_bytes"`
	MaxOutputTokens int     `json:"max_output_tokens"`
	MaxCostUSD      float64 `json:"max_cost_usd"`
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
	TimeoutS     int             `json:"timeout_s"`
	Priority     int             `json:"priority"`
	Budget       Budget          `json:"budget"`
	CreatedBy    string          `json:"created_by,omitempty"`
	CreatedAt    string          `json:"created_at,omitempty"`
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
		s.TimeoutS = DefaultTimeoutS
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
	if s.Priority < MinPriority || s.Priority > MaxPriority {
		return fmt.Errorf("priority: must be %d-%d, got %d", MinPriority, MaxPriority, s.Priority)
	}
	for _, r := range s.Requires {
		if r == "" {
			return fmt.Errorf("requires: entries must be non-empty")
		}
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
