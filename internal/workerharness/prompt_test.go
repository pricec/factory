package workerharness

import (
	"testing"

	"github.com/pricec/factory/internal/wire"
)

// wantPrompt is the §8.3 golden, spelled out independently of buildPrompt.
func wantPrompt(task, payload string) string {
	return task + "\n\nMachine input:\n" + payload +
		"\n\nReport with the report_result tool. If you cannot, your final stdout is used as fallback."
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
			want := wantPrompt(spec.Task, wantPayload)
			if got := buildPrompt(spec); got != want {
				t.Errorf("buildPrompt() =\n%q\nwant\n%q", got, want)
			}
		})
	}
}
