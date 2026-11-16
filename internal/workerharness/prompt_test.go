package workerharness

import (
	"testing"

	"github.com/pricec/factory/internal/wire"
)

// wantPrompt builds the §8.3 golden, spelled out independently of
// buildPrompt. The MCP server is named "inbox" on every runner, but the
// agent-visible tool spelling is runner-specific (§8.3): opencode exposes
// MCP tools with an `inbox_` prefix (V2 `<server>_<tool>` naming), the
// Claude Code CLI as `mcp__inbox__<tool>`.
func wantPrompt(runner, task, payload string) string {
	tool := "inbox_report_result"
	if runner == RunnerClaude {
		tool = "mcp__inbox__report_result"
	}
	return task + "\n\nMachine input:\n" + payload +
		"\n\nReport with the " + tool + " tool. If you cannot, your final stdout is used as fallback."
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
				want := wantPrompt(runner, spec.Task, wantPayload)
				if got := buildPrompt(spec, runner); got != want {
					t.Errorf("buildPrompt(%s) =\n%q\nwant\n%q", runner, got, want)
				}
			}
			// The runners differ ONLY in the tool-name spelling (§8.3);
			// everything before the suffix is byte-identical.
			oc, cl := buildPrompt(spec, RunnerOpencode), buildPrompt(spec, RunnerClaude)
			if oc[:len(oc)-len(promptSuffix(RunnerOpencode))] != cl[:len(cl)-len(promptSuffix(RunnerClaude))] {
				t.Errorf("runner prompt prefixes differ:\nopencode: %q\nclaude:   %q", oc, cl)
			}
		})
	}
}
