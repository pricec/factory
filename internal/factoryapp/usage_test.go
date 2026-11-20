package factoryapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

func TestHandleUsage(t *testing.T) {
	now := time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC)
	a, err := New(Config{
		DataDir: t.TempDir(), Addr: ":8091", AdvertiseURL: "http://factory.test",
		Now: func() time.Time { return now }, Usage: UsageConfig{F: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	get := func() map[string]any {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/usage", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := get(); m["state"] != "UNKNOWN" {
		t.Fatalf("empty state: %v", m)
	}
	// A usage event with a rate_limit_info feeds the cache and is stored.
	rl := `{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.4,"resetsAt":` +
		strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10) + `,"status":"allowed"},"seven_day":{"utilization":0.06,"resetsAt":` +
		strconv.FormatInt(now.Add(40*time.Hour).Unix(), 10) + `,"status":"allowed"}}}`
	if err := a.recordUsage(context.Background(), wire.UsageEvent{Kind: wire.UsageKindPlanner, JobID: "j1", Input: 100, Output: 50, CostUSD: 0.5, RateLimit: rl}); err != nil {
		t.Fatal(err)
	}
	m := get()
	wins := m["windows"].(map[string]any)
	five := wins["five_hour"].(map[string]any)
	if five["source"] != "stream" || five["utilization_pct"].(float64) != 40 || five["state"] != "RED" || five["pace"] == nil || five["resets_at"] == nil {
		t.Fatalf("five_hour: %v", five)
	}
	if wins["seven_day"].(map[string]any)["state"] != "GREEN" || m["state"] != "RED" {
		t.Fatalf("state: %v", m)
	}
	spend := m["factory_spend"].(map[string]any)
	for _, k := range []string{"5h", "7d", "today"} {
		if spend[k].(map[string]any)["cost_usd"].(float64) != 0.5 {
			t.Fatalf("spend %s: %v", k, spend)
		}
	}
	evs, err := a.jobs.UsageEvents(context.Background(), "j1")
	if err != nil || len(evs) != 1 || evs[0].Limit != rl {
		t.Fatalf("limit_raw: %+v err=%v", evs, err)
	}
}

func TestHandleUsageEvents(t *testing.T) {
	now := time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC)
	a, err := New(Config{
		DataDir: t.TempDir(), Addr: ":8091", AdvertiseURL: "http://factory.test",
		Now: func() time.Time { return now }, Usage: UsageConfig{F: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	for _, ev := range []wire.UsageEvent{
		{Kind: wire.UsageKindPlanner, TaskID: "task_1", SessionID: "p1", CostUSD: 0.5, DurationMs: 900},
		{Kind: wire.UsageKindBrain, ForemanID: "alpha", SessionID: "b1", CostUSD: 0.25},
	} {
		if err := a.recordUsage(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	get := func(q string) (int, []map[string]any) {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/usage/events"+q, nil))
		var out []map[string]any
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, out
	}
	if st, evs := get(""); st != 200 || len(evs) != 2 || evs[0]["session_id"] != "b1" || evs[1]["duration_ms"].(float64) != 900 {
		t.Fatalf("default (today) = %d %v", st, evs)
	}
	if st, evs := get("?kind=planner&day=2026-11-16"); st != 200 || len(evs) != 1 || evs[0]["task_id"] != "task_1" {
		t.Fatalf("planner filter = %d %v", st, evs)
	}
	if st, evs := get("?day=2026-11-15"); st != 200 || evs == nil || len(evs) != 0 {
		t.Fatalf("other day = %d %v, want empty array", st, evs)
	}
	if st, evs := get("?day=all&limit=1"); st != 200 || len(evs) != 1 {
		t.Fatalf("all/limit = %d %v", st, evs)
	}
	for _, q := range []string{"?day=yesterday", "?kind=ghost", "?limit=0", "?limit=x"} {
		if st, _ := get(q); st != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", q, st)
		}
	}
	// /v1/usage carries the per-kind breakdown of the same events.
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/usage", nil))
	var u struct {
		Spend map[string]struct {
			ByKind map[string]struct {
				Runs    int     `json:"runs"`
				CostUSD float64 `json:"cost_usd"`
			} `json:"by_kind"`
		} `json:"factory_spend"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if k := u.Spend["today"].ByKind; k["planner"].Runs != 1 || k["brain"].CostUSD != 0.25 || k["worker"].Runs != 0 {
		t.Fatalf("by_kind = %+v", k)
	}
}

// TestUsageEndpointsNoStore: live usage/throttle state must never be cached by
// the browser, a proxy or a service worker.
func TestUsageEndpointsNoStore(t *testing.T) {
	a, err := New(Config{DataDir: t.TempDir(), Addr: ":8091", AdvertiseURL: "http://factory.test", Usage: UsageConfig{F: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, path := range []string{"/v1/usage", "/v1/throttle"} {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, rec.Code, rec.Body)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("%s Cache-Control = %q, want no-store", path, cc)
		}
	}
}
