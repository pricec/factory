package factoryapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
