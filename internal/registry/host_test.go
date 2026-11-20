package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func TestGroupHosts(t *testing.T) {
	j := func(h wire.HostInfo) string { b, _ := json.Marshal(h); return string(b) }
	got := GroupHosts([]HostedForeman{
		{ID: "foreman:agentcloud", Host: j(wire.HostInfo{HostID: "bbb", Hostname: "box", Reported: "2026-01-01T00:00:00Z"})},
		{ID: "foreman:old"}, // no host: back-compat
		{ID: "foreman:null", Host: "null"},
		{ID: "foreman:junk", Host: "{not json"},
		{ID: "foreman:noid", Host: `{"hostname":"x"}`},
		{ID: "foreman:baker", Host: j(wire.HostInfo{HostID: "bbb", Hostname: "box-new", Reported: "2026-01-02T00:00:00Z"})},
		{ID: "foreman:solo", Host: j(wire.HostInfo{HostID: "aaa", Hostname: "other"})},
	})
	if len(got) != 2 {
		t.Fatalf("groups = %+v", got)
	}
	if got[0].HostID != "aaa" || strings.Join(got[0].Foremen, ",") != "foreman:solo" {
		t.Errorf("group0 = %+v", got[0])
	}
	if got[1].HostID != "bbb" || strings.Join(got[1].Foremen, ",") != "foreman:agentcloud,foreman:baker" {
		t.Errorf("group1 = %+v", got[1])
	}
	if got[1].Hostname != "box-new" {
		t.Errorf("freshest facts not kept: %q", got[1].Hostname)
	}
	if len(GroupHosts(nil)) != 0 {
		t.Error("nil input should give no groups")
	}
}

func TestRefreshHandlerHost(t *testing.T) {
	authz := func(_ *http.Request, id *string) error { *id = "foreman:a"; return nil }
	upsert := func(context.Context, string, map[string]string) error { return nil }
	tests := []struct {
		name   string
		body   string
		stored bool
	}{
		{"with host", `{"capabilities":{},"host":{"host_id":"abc","hostname":"h"}}`, true},
		{"old foreman, no host", `{"capabilities":{}}`, false},
		{"host without id dropped", `{"capabilities":{},"host":{"hostname":"h"}}`, false},
		{"malformed host does not fail refresh", `{"capabilities":{},"host":"oops"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stored *wire.HostInfo
			h := RefreshHandlerHosted(authz, upsert, nil, nil, nil, func(_ context.Context, _ string, hi wire.HostInfo) error {
				stored = &hi
				return nil
			})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body)))
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d", rec.Code)
			}
			if (stored != nil) != tt.stored {
				t.Errorf("stored = %v, want %v", stored != nil, tt.stored)
			}
		})
	}
}

func TestGroupHostsMetricsNotSummedAndFreshest(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	j := func(h wire.HostInfo) string { b, _ := json.Marshal(h); return string(b) }
	got := GroupHosts([]HostedForeman{
		{ID: "foreman:a", Host: j(wire.HostInfo{HostID: "h", SwapTotalBytes: 100, CPUUsagePct: pct(90), Load: &wire.HostLoad{One: 9}, Reported: "2026-01-02T00:00:00Z"})},
		{ID: "foreman:b", Host: j(wire.HostInfo{HostID: "h", SwapTotalBytes: 100, CPUUsagePct: pct(10), Load: &wire.HostLoad{One: 1}, Reported: "2026-01-03T00:00:00Z"})},
		{ID: "foreman:c", Host: j(wire.HostInfo{HostID: "h", SwapTotalBytes: 100, CPUUsagePct: pct(50), Load: &wire.HostLoad{One: 5}, Reported: "2026-01-01T00:00:00Z"})},
	})
	if len(got) != 1 || len(got[0].Foremen) != 3 {
		t.Fatalf("groups = %+v", got)
	}
	g := got[0]
	if g.SwapTotalBytes != 100 || g.CPUUsagePct == nil || *g.CPUUsagePct != 10 || g.Load == nil || g.Load.One != 1 {
		t.Errorf("host metrics must come from the freshest report only: %+v", g)
	}
}

func TestSanitizeClampsCPUUsage(t *testing.T) {
	v := 250.0
	h := wire.HostInfo{HostID: "x", CPUUsagePct: &v}
	if !h.Sanitize() || *h.CPUUsagePct != 100 {
		t.Errorf("cpu usage = %v", *h.CPUUsagePct)
	}
}
