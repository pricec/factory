package workerharness

import (
	"sort"
	"strings"
)

// redactedMarker replaces a secret value found in child output.
const redactedMarker = "[REDACTED]"

// redactSecrets replaces every exact occurrence of a secret value in s
// (longest first, so a value containing another is fully covered). It is a
// best-effort backstop for output the agent echoed, not a guarantee:
// transformed values (base64, split) are not caught. Env var NAMES are
// never revealed.
func redactSecrets(secretEnv map[string]string, s string) string {
	if len(secretEnv) == 0 || s == "" {
		return s
	}
	vals := make([]string, 0, len(secretEnv))
	for _, v := range secretEnv {
		if v != "" {
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, redactedMarker)
	}
	return s
}

// redactedError carries an already-redacted message.
type redactedError struct{ msg string }

func (e redactedError) Error() string { return e.msg }
