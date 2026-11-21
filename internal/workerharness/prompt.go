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
	return "\n\nReport with the " + toolName(runner, "report_result") + " tool. If you cannot, your final stdout is used as fallback." + supersededNote
}

// toolName spells a worker MCP tool the way runner's agent sees it.
func toolName(runner, tool string) string {
	if runner == RunnerClaude {
		return "mcp__inbox__" + tool
	}
	return "inbox_" + tool
}

// Creators whose jobs do NOT own a merge (§8.3): the merge job itself
// (jobs.MergeJobCreatedBy) and auditor jobs (auditor.CreatedBy; the
// factory queues an auditor fix from its result, §6.12). Literals, so
// the harness does not import the factory's packages; a factoryapp test
// pins them to the real constants.
const (
	createdByMergeQueue = "factory:merge_queue"
	createdByAuditor    = "factory:auditor"
)

// OwnsMerge reports whether a job gets the ownership note (§8.3): every
// job except merge jobs, auditor jobs, and spawn_child children (their
// parent owns the branch and its merge).
func OwnsMerge(spec wire.JobSpec) bool {
	return spec.CreatedBy != createdByMergeQueue && spec.CreatedBy != createdByAuditor && spec.ParentJobID == ""
}

// ownershipNote is the job-owned-merge instruction (§8.3, §6.11): a job
// carries its work through to the merge request itself and talks to the
// planner only for coordination or an operator decision.
func ownershipNote(runner string) string {
	return "\n\nOwn this job to the end; no one follows up for you. If it changes code in a repository: work on a branch, " +
		"get CI green (`make ci`), push, then call the " + toolName(runner, "request_merge") + " tool yourself " +
		"(branch, head sha; after = branches of other jobs that must merge first) and put the merge request id in your report. " +
		"The factory merges, deploys, and dispatches a fix job if the merge fails. " +
		"A change is not done until it is deployed and smoke-tested: a code job that changes a deployable service must include or keep the repo's " +
		"scripts/deploy.sh and a smoke check, and your report must state the running commit once the merge job has deployed it (or say plainly that it is NOT deployed). " +
		"Decide details yourself; use " + toolName(runner, "ask_parent") + " or a failed report only when you need coordination with other jobs " +
		"or a decision only the operator can make."
}

// supersededNote tells workers how to stand down without being retried
// (§5.2, §6.4, §8.3).
const supersededNote = "\nIf this job is a duplicate of, or superseded by, another job, report status=failed with error.kind=superseded (it is never retried). Use error.kind=validation for failures that will fail the same way again (permissions, auth, configuration)."

// visionNote tells every job about the vision CLI (docs/vision.md); the
// binary is installed on the foreman's service PATH by install.sh.
const visionNote = "\n\nTool: `vision [--prompt \"...\"] <image path|URL>` describes an image via the LAN vision model " +
	"(LAN-only, run serially, ~7s/img, weak on chart numbers and cropped text; see docs/vision.md)."

// payloadNull is the payload stand-in when the spec has no payload (§8.3).
const payloadNull = "null"

// buildPrompt assembles the agent prompt exactly per §8.3: task, machine
// input, the ownership note (jobs that own a merge only), and the
// reporting instruction. A missing payload becomes null.
// runner selects only the tool-name spelling (§8.3); everything else is
// byte-identical across runners. The vision tool line follows the machine
// input.
func buildPrompt(spec wire.JobSpec, runner string) string {
	payload := payloadNull
	if len(spec.Payload) > 0 {
		payload = string(spec.Payload)
	}
	note := ""
	if OwnsMerge(spec) {
		note = ownershipNote(runner)
	}
	return spec.Task + "\n\nMachine input:\n" + payload + visionNote + note + promptSuffix(runner)
}
