package jobs

import (
	"encoding/json"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func TestMergeJobTiers(t *testing.T) {
	mr := MergeRequest{ID: "mr_01X", Repo: "pricec/factory", Branch: "feat/q", Head: testHead}
	if got := MergeJobSpec(mr, "job_1", "foreman:server").Tier; got != wire.TierDefault {
		t.Errorf("default merge tier = %q, want default", got)
	}
	if got := MergeEscalatedJobSpec(mr, "job_2", "foreman:server").Tier; got != wire.TierHardest {
		t.Errorf("escalated merge tier = %q, want hardest", got)
	}
}

func TestMergeEscalatable(t *testing.T) {
	failed := func(tier, stage string) Job {
		res, _ := json.Marshal([]map[string]any{{"summary": "x", "result": map[string]string{"stage": stage}}})
		return Job{Spec: wire.JobSpec{Tier: tier}, Results: res}
	}
	for _, tc := range []struct {
		name        string
		tier, stage string
		want        bool
	}{
		{"default ci", wire.TierDefault, "ci", true},
		{"default conflict", wire.TierDefault, "conflict", true},
		{"hardest ci (no loop)", wire.TierHardest, "ci", false},
		{"default head drift", wire.TierDefault, "head_drift", false},
		{"default merge", wire.TierDefault, "merge", false},
		{"default deploy", wire.TierDefault, "deploy", false},
		{"default preflight", wire.TierDefault, "preflight", false},
		{"default unreported", wire.TierDefault, "", false},
	} {
		if got := MergeEscalatable(failed(tc.tier, tc.stage)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
