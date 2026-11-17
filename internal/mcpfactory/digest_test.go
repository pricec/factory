package mcpfactory_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/jobs"
)

// TestReadResultDigest: with a current digest read_result serves it in
// place of the last result's `result` plus a marker; full=true and a
// stale digest (index not the last result) return the verbatim results
// (spec §6.13). The stored results are never touched.
func TestReadResultDigest(t *testing.T) {
	big := json.RawMessage(`"` + strings.Repeat("verbatim ", 1000) + `"`)
	tests := []struct {
		name       string
		full       bool
		digestIdx  int
		noDigest   bool
		wantDigest bool
	}{
		{name: "digest served", wantDigest: true},
		{name: "full=true", full: true},
		{name: "stale digest ignored", digestIdx: 7},
		{name: "no digest", noDigest: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			spec := e.seedFinished(t, "long job", "done long", big, 0.1)
			if !tc.noDigest {
				if err := e.store.SetResultDigest(context.Background(), spec.ID, jobs.ResultDigest{
					ResultIndex: tc.digestIdx, Text: "- condensed: 1000 words", Model: "qwen3:8b", InputBytes: 9000,
				}); err != nil {
					t.Fatal(err)
				}
			}
			e.digest = func(ctx context.Context, id string, last int) (jobs.ResultDigest, bool) {
				d, ok, err := e.store.ResultDigest(ctx, id)
				if err != nil || !ok || d.ResultIndex != last {
					return jobs.ResultDigest{}, false
				}
				return d, true
			}
			args := map[string]any{"job_id": spec.ID}
			if tc.full {
				args["full"] = true
			}
			sc := structured(t, resultOf(t, e.callTool("read_result", args)))
			resultsJSON, _ := json.Marshal(sc["results"])
			hasVerbatim := strings.Contains(string(resultsJSON), "verbatim verbatim")
			info, hasInfo := sc["result_digest"].(map[string]any)
			if tc.wantDigest {
				if hasVerbatim || !hasInfo || !strings.Contains(string(resultsJSON), "condensed: 1000 words") {
					t.Fatalf("digest not served: %s %v", resultsJSON, sc["result_digest"])
				}
				if !strings.Contains(info["note"].(string), "full=true") || info["model"] != "qwen3:8b" {
					t.Fatalf("marker = %v", info)
				}
				if !strings.Contains(string(resultsJSON), `"summary":"done long"`) {
					t.Fatalf("other result fields dropped: %s", resultsJSON)
				}
			} else if !hasVerbatim || hasInfo {
				t.Fatalf("want verbatim results without marker: %s %v", resultsJSON[:80], sc["result_digest"])
			}
			// Storage untouched either way.
			job, _, _ := e.store.Get(context.Background(), spec.ID)
			if !strings.Contains(string(job.Results), "verbatim verbatim") {
				t.Fatal("stored results modified")
			}
		})
	}
}
