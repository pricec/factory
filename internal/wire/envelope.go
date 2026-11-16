package wire

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MessageEnvelope is the wire envelope for every message (§3.1). ID and TS
// are assigned by the server on POST; clients supply type, job_id, from, and
// payload. ThreadID (optional, inbox redesign §9.5) files the message under a
// task thread (a tasks.Task id); it is omitted from JSON when empty, so
// envelopes without it are byte-identical to the pre-thread wire format.
type MessageEnvelope struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	JobID    string          `json:"job_id"`
	From     string          `json:"from"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	TS       string          `json:"ts"`
	ThreadID string          `json:"thread_id,omitempty"`
}

// JobScoped reports whether msgType is job-scoped - prefixed "job." - and so
// requires a job_id (§3.1, §3.2).
func JobScoped(msgType string) bool {
	return strings.HasPrefix(msgType, "job.")
}

// Validate checks the envelope's client-side fields (§3.1): type and from
// required, job_id required (and job_-prefixed) for job-scoped types, and
// payload valid JSON when present. It does not check the server-assigned id
// and ts.
func (e *MessageEnvelope) Validate() error {
	if e.Type == "" {
		return fmt.Errorf("type: required")
	}
	if e.From == "" {
		return fmt.Errorf("from: required")
	}
	if e.JobID != "" && !isJobID(e.JobID) {
		return fmt.Errorf("job_id: must be a %s-prefixed id, got %q", JobIDPrefix, e.JobID)
	}
	if JobScoped(e.Type) && e.JobID == "" {
		return fmt.Errorf("job_id: required for job-scoped type %q", e.Type)
	}
	if len(e.Payload) > 0 && !json.Valid(e.Payload) {
		return fmt.Errorf("payload: not valid JSON")
	}
	return nil
}
