package registry

import (
	"encoding/json"
	"sort"

	"github.com/pricec/factory/internal/wire"
)

// HostGroup is the planner's per-machine view: every foreman reporting the
// same host_id, with the host facts from the most recently reporting one.
type HostGroup struct {
	wire.HostInfo
	Foremen []string `json:"foremen"`
}

// HostedForeman is one foreman's id plus its raw stored host JSON ("" or
// "null" when the foreman never reported one: older foremen).
type HostedForeman struct {
	ID   string
	Host string
}

// GroupHosts groups foremen by host_id. Foremen with no (or unparseable)
// host report are skipped - they simply have no host info. Output is
// sorted by host_id; each group's foremen are in input order.
func GroupHosts(in []HostedForeman) []HostGroup {
	byID := map[string]*HostGroup{}
	var order []string
	for _, f := range in {
		if f.Host == "" || f.Host == "null" {
			continue
		}
		var h wire.HostInfo
		if err := json.Unmarshal([]byte(f.Host), &h); err != nil || h.HostID == "" {
			continue
		}
		g, ok := byID[h.HostID]
		if !ok {
			g = &HostGroup{HostInfo: h}
			byID[h.HostID] = g
			order = append(order, h.HostID)
		} else if h.Reported > g.Reported {
			// Keep the freshest facts for the shared machine.
			g.HostInfo = h
		}
		g.Foremen = append(g.Foremen, f.ID)
	}
	sort.Strings(order)
	out := make([]HostGroup, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}
