package foremanapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pricec/factory/internal/claudecli"
	"github.com/pricec/factory/internal/ocusage"
	"github.com/pricec/factory/internal/wire"
)

// An opencode brain session's usage is read back per session from the
// opencode database and handed to OnUsage (-> usage.record), even when the
// session context is already done.
func TestBrainRecordOpencodeUsage(t *testing.T) {
	started := time.Now().Add(-2 * time.Second)
	var got []claudecli.Usage
	b := &BrainRunner{
		Model:   "openrouter/z-ai/glm-5.3",
		WorkDir: "/data/brain",
		ReadUsage: func(dir string, sinceMs int64) ([]ocusage.Session, error) {
			if dir != "/data/brain" || sinceMs != started.UnixMilli() {
				t.Errorf("read(%q, %d)", dir, sinceMs)
			}
			return []ocusage.Session{{ID: "ses_b", InputTokens: 5, OutputTokens: 1, ReasoningTokens: 1, CostUSD: 0.1}}, nil
		},
		OnUsage: func(ctx context.Context, u claudecli.Usage) {
			if ctx.Err() != nil {
				t.Errorf("OnUsage ctx done: %v", ctx.Err())
			}
			got = append(got, u)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.recordOpencodeUsage(ctx, started)
	if len(got) != 1 || got[0].SessionID != "ses_b" || got[0].Model != b.Model || got[0].Output != 2 || got[0].DurationMs < 2000 {
		t.Fatalf("usage = %+v", got)
	}

	b.ReadUsage = func(string, int64) ([]ocusage.Session, error) { return nil, errors.New("no db") }
	b.recordOpencodeUsage(context.Background(), started)
	if len(got) != 1 {
		t.Fatalf("read error recorded usage: %+v", got)
	}
}

// Worker harness configs carry the opencode usage reader.
func TestDefaultHarnessConfigWiresUsageReader(t *testing.T) {
	a := &App{}
	if c := a.defaultHarnessConfig(wire.JobSpec{ID: "job_x"}, "tok"); c.ReadUsage == nil {
		t.Fatal("ReadUsage not wired")
	}
}
