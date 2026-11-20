package workerharness

import (
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

// wantPrompt builds the §8.3 golden, spelled out independently of
// buildPrompt. The MCP server is named "inbox" on every runner, but the
// agent-visible tool spelling is runner-specific (§8.3): opencode exposes
// MCP tools with an `inbox_` prefix (V2 `<server>_<tool>` naming), the
// Claude Code CLI as `mcp__inbox__<tool>`.
func wantPrompt(runner, task, payload string, owns bool) string {
	prefix := "inbox_"
	if runner == RunnerClaude {
		prefix = "mcp__inbox__"
	}
	note := ""
	if owns {
		note = "\n\nOwn this job to the end; no one follows up for you. If it changes code in a repository: work on a branch, " +
			"get CI green (`make ci`), push, then call the " + prefix + "request_merge tool yourself " +
			"(branch, head sha; after = branches of other jobs that must merge first) and put the merge request id in your report. " +
			"The factory merges, deploys, and dispatches a fix job if the merge fails. " +
			"Decide details yourself; use " + prefix + "ask_parent or a failed report only when you need coordination with other jobs " +
			"or a decision only the operator can make."
	}
	return task + "\n\nMachine input:\n" + payload +
		"\n\nTool: `vision [--prompt \"...\"] <image path|URL>` describes an image via the LAN vision model " +
		"(LAN-only, run serially, ~7s/img, weak on chart numbers and cropped text; see docs/vision.md)." + note +
		"\n\nReport with the " + prefix + "report_result tool. If you cannot, your final stdout is used as fallback." + supersededNote
}

// TestOwnershipNote: every job is told to own its work through to the
// merge request (§8.3, §6.11), except the jobs that must not file one:
// the merge job itself, auditor jobs (the factory queues those), and
// spawn_child children (the parent owns the branch).
func TestOwnershipNote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		createdBy string
		parent    string
		owns      bool
	}{
		{"planner job", "factory", "", true},
		{"ui job", "ui", "", true},
		{"merge-fix job", "factory:merge_fix", "", true},
		{"foreman-spawned job", "foreman:alpha", "", true},
		{"merge job", "factory:merge_queue", "", false},
		{"auditor job", "factory:auditor", "", false},
		{"spawn_child child", "worker:job_parent", "job_parent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := wire.NewJobSpec("Do the thing.", tc.createdBy)
			spec.ParentJobID = tc.parent
			if got := OwnsMerge(spec); got != tc.owns {
				t.Fatalf("OwnsMerge = %v, want %v", got, tc.owns)
			}
			for _, runner := range []string{RunnerOpencode, RunnerClaude} {
				if got, want := buildPrompt(spec, runner), wantPrompt(runner, spec.Task, "null", tc.owns); got != want {
					t.Errorf("buildPrompt(%s) =\n%q\nwant\n%q", runner, got, want)
				}
			}
		})
	}
}

func TestBuildPrompt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string // "" means absent
	}{
		{"payload object", `{"repo":"github.com/pricec/factory"}`},
		{"payload with spacing preserved", `{"a": 1, "b": [2, 3]}`},
		{"payload null literal", `null`},
		{"payload absent", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := wire.NewJobSpec("Fix the failing build and land the patch.", "factory")
			if tc.payload != "" {
				spec.Payload = []byte(tc.payload)
			}
			wantPayload := tc.payload
			if wantPayload == "" {
				wantPayload = "null"
			}
			for _, runner := range []string{RunnerOpencode, RunnerClaude} {
				want := wantPrompt(runner, spec.Task, wantPayload, true)
				if got := buildPrompt(spec, runner); got != want {
					t.Errorf("buildPrompt(%s) =\n%q\nwant\n%q", runner, got, want)
				}
			}
			// The runners differ ONLY in the tool-name spelling (§8.3):
			// with the spelling normalized the prompts are byte-identical.
			oc, cl := buildPrompt(spec, RunnerOpencode), buildPrompt(spec, RunnerClaude)
			if strings.ReplaceAll(cl, "mcp__inbox__", "inbox_") != oc {
				t.Errorf("runner prompts differ beyond tool names:\nopencode: %q\nclaude:   %q", oc, cl)
			}
		})
	}
}
