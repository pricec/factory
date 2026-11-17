package factoryapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/ocusage"
	"github.com/pricec/factory/internal/wire"
)

// postSessionUsage posts one session.usage envelope from a paired
// foreman into factory:main (the §3.2 producer side of the ledger).
func postSessionUsage(t *testing.T, e *testEnv, bearer, foreman string, u wire.SessionUsage) {
	t.Helper()
	u.ForemanID = "" // the consumer derives it from the envelope's From
	payload, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	if status := e.postToFactoryMain(bearer, wire.MessageEnvelope{
		Type:    "session.usage",
		JobID:   u.JobID,
		From:    "foreman:" + foreman,
		Payload: payload,
	}); status != http.StatusCreated {
		t.Fatalf("post session.usage status = %d, want 201", status)
	}
}

// TestSessionUsageLedgerAndBackfill: the factory:main consumer records
// one session.usage envelope per completed LLM child (spec §3.2, §6.5
// reporting amendment) into the ledger - keyed by the opencode session
// id, role- and foreman-tagged - and backfills the worker job's
// still-empty usage, so /v1/spend's per-role totals and the session log
// see spend that JobResult alone never carried. Malformed payloads drop
// silently; duplicate deliveries upsert, never double-count.
func TestSessionUsageLedgerAndBackfill(t *testing.T) {
	e := newEnv(t)
	alpha := e.pair("alpha", nil)

	// A running job whose agent never self-reported usage.
	spec := wire.NewJobSpec("work", "factory")
	if err := e.a.Jobs().Create(e.ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := e.a.Jobs().Dispatch(e.ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if err := e.a.Jobs().ApplyEvent(e.ctx, spec.ID, "started"); err != nil {
		t.Fatalf("ApplyEvent: %v", err)
	}

	usage := wire.SessionUsage{
		SessionID: "ses_w1", Role: wire.SessionRoleWorker, JobID: spec.ID,
		Model:       "openrouter/z-ai/glm-5.3-flash",
		InputTokens: 578107, OutputTokens: 29891,
		ReasoningTokens: 37653, CacheReadTokens: 12171904,
		CostUSD: 0.3937, DurationMs: 87000,
	}
	postSessionUsage(t, e, alpha, "alpha", usage)
	// Duplicate delivery (inbox at-least-once): upserts, never
	// double-counts.
	postSessionUsage(t, e, alpha, "alpha", usage)

	waitFor(t, "ledger row + job backfill", 5*time.Second, func() bool {
		rows, err := e.a.Jobs().ListSessions(e.ctx, "", "", 0)
		if err != nil || len(rows) != 1 {
			return false
		}
		job, ok, err := e.a.Jobs().Get(e.ctx, spec.ID)
		if err != nil || !ok {
			return false
		}
		var u wire.Usage
		return json.Unmarshal(job.Usage, &u) == nil && u.CostUSD == 0.3937
	})

	rows, err := e.a.Jobs().ListSessions(e.ctx, "", "", 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := rows[0]
	if got.ID != "ses_w1" || got.Role != wire.SessionRoleWorker ||
		got.ForemanID != "alpha" || got.JobID != spec.ID {
		t.Errorf("ledger row = %+v, want ses_w1 worker for %s on alpha", got, spec.ID)
	}
	if got.InputTokens != 578107 || got.ReasoningTokens != 37653 || got.CostUSD != 0.3937 {
		t.Errorf("ledger accounting = %+v, want the posted numbers", got)
	}

	// The additive /v1/spend fields: role totals from the ledger.
	var spend struct {
		SessionTotalUSD float64 `json:"session_total_usd"`
		Roles           []struct {
			Role     string  `json:"role"`
			Sessions int     `json:"sessions"`
			TotalUSD float64 `json:"total_usd"`
		} `json:"roles"`
	}
	if status := e.do("GET", "/v1/spend", "", nil, &spend); status != http.StatusOK {
		t.Fatalf("GET /v1/spend = %d", status)
	}
	if spend.SessionTotalUSD != 0.3937 {
		t.Errorf("session_total_usd = %v, want 0.3937", spend.SessionTotalUSD)
	}
	var workerTotal *struct {
		Role     string  `json:"role"`
		Sessions int     `json:"sessions"`
		TotalUSD float64 `json:"total_usd"`
	}
	for i, r := range spend.Roles {
		if r.Role == wire.SessionRoleWorker {
			workerTotal = &spend.Roles[i]
		}
	}
	if workerTotal == nil || workerTotal.Sessions != 1 || workerTotal.TotalUSD != 0.3937 {
		t.Errorf("worker role totals = %+v, want 1 session at $0.3937", workerTotal)
	}

	// A malformed payload drops silently: no row, no backfill.
	bad := wire.MessageEnvelope{
		Type: "session.usage", From: "foreman:alpha",
		Payload: json.RawMessage(`{"session_id":"ses_bad"}`),
	}
	if status := e.postToFactoryMain(alpha, bad); status != http.StatusCreated {
		t.Fatalf("post malformed session.usage status = %d, want 201 (the inbox accepts it)", status)
	}
	waitFor(t, "no row for the malformed payload", 2*time.Second, func() bool {
		rows, err := e.a.Jobs().ListSessions(e.ctx, "", "", 0)
		return err == nil && len(rows) == 1 && rows[0].ID == "ses_w1"
	})
}

// TestPlannerUsageRecorded: planner sessions are recorded in-process
// after each wake (spec §6.5 reporting amendment) - the planner runs on
// the factory host, so its accounting bypasses the session.usage
// envelope path and lands straight in the ledger.
func TestPlannerUsageRecorded(t *testing.T) {
	e := newEnv(t)

	var mu sync.Mutex
	var woke int
	e.a.SetPlannerExec(func(ctx context.Context, prompt string) (string, error) {
		mu.Lock()
		woke++
		mu.Unlock()
		return "planner handled it", nil
	})
	e.a.SetPlannerUsageReader(func(dir string, sinceMs int64) ([]ocusage.Session, error) {
		if !strings.Contains(dir, "planner") {
			t.Errorf("usage read for dir %q, want the planner work dir", dir)
		}
		return []ocusage.Session{{
			ID: "ses_p1", Directory: dir, Model: "openrouter/z-ai/glm-5.3",
			InputTokens: 124180, OutputTokens: 8592,
			ReasoningTokens: 81631, CostUSD: 0.4507,
		}}, nil
	})

	if status := e.do("POST", "/v1/chat", "", map[string]string{"text": "status?"}, nil); status != http.StatusCreated {
		t.Fatalf("POST /v1/chat = %d", status)
	}
	waitFor(t, "planner session recorded", 5*time.Second, func() bool {
		rows, err := e.a.Jobs().ListSessions(e.ctx, "", wire.SessionRolePlanner, 0)
		return err == nil && len(rows) == 1
	})
	rows, err := e.a.Jobs().ListSessions(e.ctx, "", wire.SessionRolePlanner, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if got := rows[0]; got.ID != "ses_p1" || got.Model != "openrouter/z-ai/glm-5.3" ||
		got.CostUSD != 0.4507 || got.ReasoningTokens != 81631 {
		t.Errorf("planner ledger row = %+v, want the recorded session", got)
	}
}
