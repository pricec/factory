package wire

import (
	"strings"

	"github.com/oklog/ulid/v2"
)

// ID prefixes pinned in the spec (§3.1).
const (
	MessageIDPrefix = "msg_"
	JobIDPrefix     = "job_"
)

// NewMessageID returns a fresh message id: a ULID with the msg_ prefix
// (§3.1). Assigned by the server on POST.
func NewMessageID() string {
	return MessageIDPrefix + ulid.Make().String()
}

// NewJobID returns a fresh job id: a ULID with the job_ prefix (§3.1).
func NewJobID() string {
	return JobIDPrefix + ulid.Make().String()
}

// isJobID reports whether id carries the job_ prefix and is more than the
// bare prefix.
func isJobID(id string) bool {
	return strings.HasPrefix(id, JobIDPrefix) && len(id) > len(JobIDPrefix)
}
