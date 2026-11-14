package workerharness

import (
	"github.com/pricec/factory/internal/wire"
)

// promptSuffix is the reporting instruction pinned in §8.3.
const promptSuffix = "\n\nReport with the report_result tool. If you cannot, your final stdout is used as fallback."

// payloadNull is the payload stand-in when the spec has no payload (§8.3).
const payloadNull = "null"

// buildPrompt assembles the agent prompt exactly per §8.3: task, machine
// input, and the reporting instruction. A missing payload becomes null.
func buildPrompt(spec wire.JobSpec) string {
	payload := payloadNull
	if len(spec.Payload) > 0 {
		payload = string(spec.Payload)
	}
	return spec.Task + "\n\nMachine input:\n" + payload + promptSuffix
}
