package workerharness

import (
	"github.com/pricec/factory/internal/wire"
)

// promptSuffix returns the reporting instruction pinned in §8.3. The MCP
// server is named "inbox" on every runner, but the agent-visible tool
// spelling is runner-specific (§8.3: the prompt must use the name the
// agent actually sees): opencode exposes MCP tools with an `inbox_`
// prefix (V2 `<server>_<tool>` naming), the Claude Code CLI as
// `mcp__inbox__<tool>`. Task and machine-input block are identical.
func promptSuffix(runner string) string {
	tool := "inbox_report_result"
	if runner == RunnerClaude {
		tool = "mcp__inbox__report_result"
	}
	return "\n\nReport with the " + tool + " tool. If you cannot, your final stdout is used as fallback." + supersededNote
}

// supersededNote tells workers how to stand down without being retried
// (§5.2, §6.4, §8.3).
const supersededNote = "\nIf this job is a duplicate of, or superseded by, another job, report status=failed with error.kind=superseded (it is never retried). Use error.kind=validation for failures that will fail the same way again (permissions, auth, configuration)."

// payloadNull is the payload stand-in when the spec has no payload (§8.3).
const payloadNull = "null"

// buildPrompt assembles the agent prompt exactly per §8.3: task, machine
// input, and the reporting instruction. A missing payload becomes null.
// runner selects only the tool-name spelling (§8.3); everything else is
// byte-identical across runners.
func buildPrompt(spec wire.JobSpec, runner string) string {
	payload := payloadNull
	if len(spec.Payload) > 0 {
		payload = string(spec.Payload)
	}
	return spec.Task + "\n\nMachine input:\n" + payload + promptSuffix(runner)
}
