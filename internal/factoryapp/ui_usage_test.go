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

// TestUIUsagePanel: the served UI page carries the Factory usage panel, polls
// GET /v1/usage about every 60s, and reads exactly the JSON keys that
// /v1/usage emits (a rename on either side fails here; there is no JS runner).
func TestUIUsagePanel(t *testing.T) {
	now := time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC)
	a, err := New(Config{
		DataDir: t.TempDir(), Addr: ":8091", AdvertiseURL: "http://factory.test",
		Now: func() time.Time { return now }, Usage: UsageConfig{F: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	serve := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		return rec
	}

	page := serve("/").Body.String()
	for _, want := range []string{
		`id="usagepanel"`, "Factory usage", `getJSON("/v1/usage")`, "USAGE_POLL_MS = 60000",
		`["five_hour", "5-hour"]`, `["seven_day", "weekly"]`,
		"unavailable:", "stale data", "STALE", "factory spend",
		"w.utilization_pct", "w.resets_at", "w.pace", "w.state", "w.source", "w.age_s", "w.stale",
		"usageData.state", "usageData.factory_spend", "sp[k].cost_usd", "sp[k].tokens",
		`"5h"`, `"7d"`, `"today"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("UI page missing %q", want)
		}
	}
	// Every state the backend can emit has a badge/meter colour rule.
	for _, st := range []string{"GREEN", "YELLOW", "ORANGE", "RED"} {
		if !strings.Contains(page, ".badge."+st) || !strings.Contains(page, ".meter > i."+st) {
			t.Errorf("no colour rule for %s", st)
		}
	}

	rl := `{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.2,"resetsAt":` +
		strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10) + `,"status":"allowed"},"seven_day":{"utilization":0.05,"resetsAt":` +
		strconv.FormatInt(now.Add(40*time.Hour).Unix(), 10) + `,"status":"allowed"}}}`
	if err := a.recordUsage(context.Background(), wire.UsageEvent{Kind: wire.UsageKindPlanner, JobID: "j1", Input: 10, Output: 5, CostUSD: 0.25, RateLimit: rl}); err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(serve("/v1/usage").Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"state", "windows", "factory_spend"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("/v1/usage missing %q", k)
		}
	}
	var wins map[string]map[string]any
	if err := json.Unmarshal(m["windows"], &wins); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"five_hour", "seven_day"} {
		for _, k := range []string{"utilization_pct", "resets_at", "pace", "state", "source", "age_s", "stale"} {
			if _, ok := wins[w][k]; !ok {
				t.Errorf("window %s missing key %q", w, k)
			}
		}
	}
	var spend map[string]map[string]any
	if err := json.Unmarshal(m["factory_spend"], &spend); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"5h", "7d", "today"} {
		for _, k := range []string{"cost_usd", "tokens"} {
			if _, ok := spend[p][k]; !ok {
				t.Errorf("factory_spend.%s missing %q", p, k)
			}
		}
	}
}
