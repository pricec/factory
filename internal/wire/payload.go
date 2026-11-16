package wire

import (
	"bytes"
	"encoding/json"
)

// MarshalPayload encodes a tool-supplied free-form value (an `any`-typed
// MCP argument) for an envelope payload or spec field. LLM clients often
// send an object as a JSON *string* ("{\"text\": ...}") because the
// `any` schema does not pin the type; marshalling that verbatim yields a
// quoted string the consumer cannot parse (Malformed alert.raise, raw
// JSON as a task title). A string whose trimmed content is a valid JSON
// object or array is therefore sent as that structured JSON; every other
// value, including plain strings, marshals unchanged.
func MarshalPayload(v any) (json.RawMessage, error) {
	if s, ok := v.(string); ok {
		t := bytes.TrimSpace([]byte(s))
		if len(t) > 0 && (t[0] == '{' || t[0] == '[') && json.Valid(t) {
			var buf bytes.Buffer
			if err := json.Compact(&buf, t); err != nil {
				return nil, err
			}
			return buf.Bytes(), nil
		}
	}
	return json.Marshal(v)
}
