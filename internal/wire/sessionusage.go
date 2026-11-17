package wire

import "fmt"

// Session roles for the session-usage ledger (spec §6.5 reporting
// amendment, 2026-11-16): one row per completed LLM child, recorded
// from the opencode session store.
const (
	// SessionRolePlanner labels factory planner sessions.
	SessionRolePlanner = "planner"
	// SessionRoleForemanBrain labels foreman brain sessions.
	SessionRoleForemanBrain = "foreman_brain"
	// SessionRoleWorker labels worker job sessions.
	SessionRoleWorker = "worker"
)

// SessionUsage is the `session.usage` envelope payload (spec §3.2,
// 2026-11-16 amendment): one foreman post per completed LLM child on
// that host, carrying the provider-reported cost and token breakdown
// read from the opencode session store. The factory records it in the
// sessions ledger and backfills the job's empty `usage` when the
// session belongs to a worker run that self-reported none.
type SessionUsage struct {
	// SessionID is the opencode session id; the ledger's primary key
	// (duplicate deliveries upsert, they never double-count).
	SessionID string `json:"session_id"`
	// Role is one of the SessionRole* constants.
	Role string `json:"role"`
	// ForemanID is the reporting foreman's bare id; "" for factory-side
	// planner rows recorded in-process.
	ForemanID string `json:"foreman_id,omitempty"`
	// JobID links a worker session to its job; "" otherwise.
	JobID string `json:"job_id,omitempty"`
	// Model is the provider-qualified model string in the opencode `-m`
	// flag form.
	Model string `json:"model,omitempty"`

	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// DurationMs is the reporter's wall-clock duration of the child.
	DurationMs int64 `json:"duration_ms"`
}

// ValidSessionRole reports whether role is a known ledger role.
func ValidSessionRole(role string) bool {
	switch role {
	case SessionRolePlanner, SessionRoleForemanBrain, SessionRoleWorker:
		return true
	}
	return false
}

// Validate checks the payload ahead of ledger insertion: a known role,
// a session id, and non-negative accounting fields.
func (u *SessionUsage) Validate() error {
	if u.SessionID == "" {
		return fmt.Errorf("session_id: required")
	}
	if !ValidSessionRole(u.Role) {
		return fmt.Errorf("role: unknown %q", u.Role)
	}
	if u.CostUSD < 0 || u.InputTokens < 0 || u.OutputTokens < 0 ||
		u.ReasoningTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 {
		return fmt.Errorf("usage: fields must be >= 0")
	}
	return nil
}
