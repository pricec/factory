package factoryapp

import (
	"context"
	"testing"
	"time"

	"github.com/pricec/factory/internal/ocusage"
	"github.com/pricec/factory/internal/wire"
)

// An opencode planner session's usage lands in usage_events as a planner
// event tagged with the woken task, through the same OnUsage wiring as
// claude runs.
func TestPlannerRecordOpencodeUsage(t *testing.T) {
	a, err := New(Config{DataDir: t.TempDir(), Addr: ":8092", AdvertiseURL: "http://factory.test", Usage: UsageConfig{F: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.planner.Model = "openrouter/z-ai/glm-5.3"
	a.planner.taskID = "task_1"
	a.planner.ReadUsage = func(dir string, _ int64) ([]ocusage.Session, error) {
		if dir != a.planner.WorkDir {
			t.Errorf("read dir %q, want %q", dir, a.planner.WorkDir)
		}
		return []ocusage.Session{{ID: "ses_p", InputTokens: 9, OutputTokens: 1, CostUSD: 0.2}}, nil
	}
	for i := 0; i < 2; i++ { // a re-read of the same session is deduped
		a.planner.recordOpencodeUsage(context.Background(), time.Now())
	}
	evs, err := a.jobs.ListUsageEvents(context.Background(), "", wire.UsageKindPlanner, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].TaskID != "task_1" || evs[0].SessionID != "ses_p" || evs[0].Model != a.planner.Model || evs[0].CostUSD != 0.2 {
		t.Fatalf("planner events = %+v", evs)
	}
}
