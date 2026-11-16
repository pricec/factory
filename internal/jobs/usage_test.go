package jobs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func TestRecordUsageOverridesAgentUsage(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	spec := newSpec(t, nil)
	if _, err := s.RegisterSpawned(ctx, spec, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyEvent(ctx, spec.ID, "started"); err != nil {
		t.Fatal(err)
	}
	ev := wire.UsageEvent{Kind: wire.UsageKindWorker, JobID: spec.ID, SessionID: "s1", Input: 10, CacheRead: 90, Output: 5, CostUSD: 0.5}
	for i := 0; i < 2; i++ { // redelivery is a no-op
		if err := s.RecordUsage(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := s.UsageEvents(ctx, spec.ID)
	if len(evs) != 1 || evs[0].ForemanID != "alpha" {
		t.Fatalf("events = %+v", evs)
	}
	agent := &wire.Usage{InputTokens: 1, OutputTokens: 1, CostUSD: 99}
	if err := s.ApplyResult(ctx, wire.JobResult{JobID: spec.ID, Status: wire.StatusDone, Summary: "ok", Usage: agent}); err != nil {
		t.Fatal(err)
	}
	j, _, _ := s.Get(ctx, spec.ID)
	var u wire.Usage
	if err := json.Unmarshal(j.Usage, &u); err != nil {
		t.Fatal(err)
	}
	if u.CostUSD != 0.5 || u.InputTokens != 100 || u.OutputTokens != 5 {
		t.Fatalf("rollup = %+v", u)
	}
}
