// Table-driven tests for the long-pending alert's no-match diagnosis
// (spec §6.7, §9.5; 2026-11-15 amendment): given a job's requires and the
// full foremen registry, classify why no online foreman matches.
package factoryapp

import (
	"reflect"
	"testing"
	"time"

	"github.com/pricec/factory/internal/registry"
)

func TestDiagnoseNoMatch(t *testing.T) {
	stamp := time.Date(2026, 11, 15, 21, 0, 0, 0, time.UTC).Format(time.RFC3339)
	foreman := func(id string, online bool, caps registry.Capabilities) alertForeman {
		return alertForeman{id: id, online: online, lastSeen: stamp, caps: caps}
	}

	tests := []struct {
		name      string
		requires  []string
		foremen   []alertForeman
		want      noMatchDiagnosis
		alertable bool
	}{
		{
			name:     "no foremen registered",
			requires: []string{"os:linux"},
			want: noMatchDiagnosis{
				reason: "no foremen are registered",
			},
			alertable: true,
		},
		{
			// Fix2 reconciliation: pre-fix2 Has("os:linux") was a literal
			// key lookup, so an online foreman with the native caps below
			// satisfied nothing - the incident shape. The amendment made
			// "os:linux" match os=linux, so the unsatisfiable-online-
			// foreman shape is now wrong VALUES; re-merged here to keep
			// the diagnosis covered under the new semantics.
			name:     "incident shape: online foreman advertises the wrong values",
			requires: []string{"os:linux", "arch:amd64"},
			foremen: []alertForeman{
				foreman("foreman:server", true, registry.Capabilities{"os": "darwin", "arch": "arm64"}),
			},
			want: noMatchDiagnosis{
				reason:  "no foreman advertises: arch:amd64, os:linux",
				missing: []string{"arch:amd64", "os:linux"},
				total:   1,
				online:  1,
			},
			alertable: true,
		},
		{
			name:     "required tags live only on an offline foreman",
			requires: []string{"os:linux", "arch:amd64"},
			foremen: []alertForeman{
				foreman("foreman:server", false, registry.Capabilities{"os": "linux", "arch": "amd64"}),
			},
			want: noMatchDiagnosis{
				reason:  "only offline foremen have: arch:amd64, os:linux",
				offline: []string{"foreman:server (last seen " + stamp + ")"},
				total:   1,
			},
			alertable: true,
		},
		{
			name:     "missing tags plus offline foremen combine",
			requires: []string{"gpu", "tools:docker"},
			foremen: []alertForeman{
				foreman("foreman:big", false, registry.Capabilities{"gpu": "a100"}),
			},
			want: noMatchDiagnosis{
				reason:  "no foreman advertises: tools:docker; only offline foremen have: gpu",
				missing: []string{"tools:docker"},
				offline: []string{"foreman:big (last seen " + stamp + ")"},
				total:   1,
			},
			alertable: true,
		},
		{
			name:     "empty requires with an all-offline registry",
			requires: nil,
			foremen: []alertForeman{
				foreman("foreman:server", false, registry.Capabilities{"os": "linux"}),
			},
			want: noMatchDiagnosis{
				reason: "all foremen are offline",
				total:  1,
			},
			alertable: true,
		},
		{
			name:     "tags split across online foremen",
			requires: []string{"gpu", "tools:docker"},
			foremen: []alertForeman{
				foreman("foreman:a", true, registry.Capabilities{"gpu": "a100"}),
				foreman("foreman:b", true, registry.Capabilities{"tools": "docker"}),
			},
			want: noMatchDiagnosis{
				reason: "no single online foreman has all required tags",
				total:  2,
				online: 2,
			},
			alertable: true,
		},
		{
			name:     "online foreman satisfies the requires: transient, skip",
			requires: []string{"os:linux"},
			foremen: []alertForeman{
				foreman("foreman:server", true, registry.Capabilities{"os": "linux"}),
			},
			want: noMatchDiagnosis{
				total:  1,
				online: 1,
			},
			alertable: false,
		},
		{
			name:     "offline foreman without the tags is not named",
			requires: []string{"os:linux"},
			foremen: []alertForeman{
				foreman("foreman:dark", false, registry.Capabilities{"gpu": "a100"}),
				foreman("foreman:server", true, registry.Capabilities{"os": "linux"}),
			},
			want: noMatchDiagnosis{
				total:  2,
				online: 1,
			},
			alertable: false,
		},
		{
			// Fix2 reconciliation: legacy latch keys - {"os:linux":
			// "latch"} from the incident's upsert - advertise nothing now
			// that Has cuts "os:linux" into the os=linux value check. A
			// foreman carrying only latch keys reads as not advertising,
			// exactly like the dispatch picker sees it.
			name:     "legacy latch keys advertise nothing (fix2)",
			requires: []string{"os:linux"},
			foremen: []alertForeman{
				foreman("foreman:server", true, registry.Capabilities{"os:linux": "latch"}),
			},
			want: noMatchDiagnosis{
				reason:  "no foreman advertises: os:linux",
				missing: []string{"os:linux"},
				total:   1,
				online:  1,
			},
			alertable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, alertable := diagnoseNoMatch(tt.requires, tt.foremen)
			if alertable != tt.alertable {
				t.Errorf("alertable = %v, want %v (reason %q)", alertable, tt.alertable, got.reason)
			}
			if got.reason != tt.want.reason {
				t.Errorf("reason = %q, want %q", got.reason, tt.want.reason)
			}
			if !reflect.DeepEqual(got.missing, tt.want.missing) {
				t.Errorf("missing = %v, want %v", got.missing, tt.want.missing)
			}
			if !reflect.DeepEqual(got.offline, tt.want.offline) {
				t.Errorf("offline = %v, want %v", got.offline, tt.want.offline)
			}
			if got.total != tt.want.total || got.online != tt.want.online {
				t.Errorf("totals = (%d, %d), want (%d, %d)", got.total, got.online, tt.want.total, tt.want.online)
			}
		})
	}
}
