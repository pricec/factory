package wire

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Summary is required on every JobResult: 1-240 chars (§5.4, §11).
const (
	MinSummaryChars = 1
	MaxSummaryChars = 240
)

// JobResult status values (§5.2).
const (
	StatusDone   = "done"
	StatusFailed = "failed"
)

// ErrorKind enumerates the pinned error kinds (§5.2).
type ErrorKind string

// The pinned error kinds (§5.2).
const (
	KindValidation ErrorKind = "validation"
	KindTimeout    ErrorKind = "timeout"
	KindCrash      ErrorKind = "crash"
	KindLLM        ErrorKind = "llm"
	KindOther      ErrorKind = "other"
	// KindSuperseded: the worker stood down because the job is a duplicate
	// of, or was superseded by, another job. Not a failure to retry: the
	// factory maps it to the terminal superseded state.
	KindSuperseded ErrorKind = "superseded"
)

// Valid reports whether k is one of the pinned error kinds.
func (k ErrorKind) Valid() bool {
	switch k {
	case KindValidation, KindTimeout, KindCrash, KindLLM, KindOther, KindSuperseded:
		return true
	default:
		return false
	}
}

// Kill reasons (JobError.Reason): why the worker harness terminated the
// child itself, so an operator can tell the limits apart.
const (
	// ReasonIdle: no worker output for idle_timeout_s.
	ReasonIdle = "idle"
	// ReasonCeiling: wall-clock ceiling timeout_s reached.
	ReasonCeiling = "ceiling"
	// ReasonBudget: live spend reached budget.max_cost_usd.
	ReasonBudget = "budget"
	// ReasonLoop: the same tool call repeated back to back too often.
	ReasonLoop = "loop"
)

// JobError explains a failed result (§5.2). Reason is set only when the
// harness killed the child (idle|ceiling|budget|loop).
type JobError struct {
	Kind    ErrorKind `json:"kind"`
	Message string    `json:"message"`
	Reason  string    `json:"reason,omitempty"`
}

// Usage reports a job's token and cost spend (§5.2).
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// JobResult is the worker's report for one job attempt (§5.2).
type JobResult struct {
	JobID   string          `json:"job_id"`
	Status  string          `json:"status"`
	Summary string          `json:"summary"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JobError       `json:"error,omitempty"`
	Usage   *Usage          `json:"usage,omitempty"`
}

// Validate checks the pinned JobResult rules (§5.4): summary required always,
// 1-240 chars; status must be done or failed; for done, result is required
// when resultSchema is present and must validate against it; for failed,
// error is required with a valid kind and result is ignored; usage is
// optional with all fields >= 0. resultSchema is the job's result_schema
// (§5.1); pass nil when the job has none.
func (r *JobResult) Validate(resultSchema json.RawMessage) error {
	chars := utf8.RuneCountInString(r.Summary)
	if chars < MinSummaryChars || chars > MaxSummaryChars {
		return fmt.Errorf("summary: must be %d-%d chars, got %d", MinSummaryChars, MaxSummaryChars, chars)
	}
	switch r.Status {
	case StatusDone:
		if len(resultSchema) > 0 {
			if len(r.Result) == 0 {
				return fmt.Errorf("result: required when result_schema is present")
			}
			if err := ValidateSchemaValue(resultSchema, r.Result); err != nil {
				return fmt.Errorf("result: %w", err)
			}
		}
	case StatusFailed:
		if r.Error == nil {
			return fmt.Errorf("error: required when status is %q", StatusFailed)
		}
		if !r.Error.Kind.Valid() {
			return fmt.Errorf("error.kind: unknown kind %q", r.Error.Kind)
		}
	default:
		return fmt.Errorf("status: must be %q or %q, got %q", StatusDone, StatusFailed, r.Status)
	}
	if u := r.Usage; u != nil {
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.CostUSD < 0 {
			return fmt.Errorf("usage: fields must be >= 0, got %+v", *u)
		}
	}
	return nil
}

// UsageEvent is one CLI run's captured spend, recorded append-only by the
// factory (envelope types job.usage from workers, usage.record from
// planner/brain sessions). Kind is worker|planner|brain. Limit carries any
// raw rate-limit text the CLI emitted.
type UsageEvent struct {
	Kind        string  `json:"kind"`
	JobID       string  `json:"job_id,omitempty"`
	TaskID      string  `json:"task_id,omitempty"`
	ForemanID   string  `json:"foreman_id,omitempty"`
	Model       string  `json:"model,omitempty"`
	Tier        string  `json:"tier,omitempty"`
	Input       int     `json:"input"`
	Output      int     `json:"output"`
	CacheCreate int     `json:"cache_create"`
	CacheRead   int     `json:"cache_read"`
	CostUSD     float64 `json:"cost_usd"`
	SessionID   string  `json:"session_id,omitempty"`
	Limit       string  `json:"limit,omitempty"`
	// RateLimit is the raw rate_limit_event.rate_limit_info JSON (stream-json).
	RateLimit string `json:"rate_limit,omitempty"`
}

// Usage event kinds and envelope types.
const (
	UsageKindWorker  = "worker"
	UsageKindPlanner = "planner"
	UsageKindBrain   = "brain"

	TypeJobUsage    = "job.usage"
	TypeUsageRecord = "usage.record"
)
