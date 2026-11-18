package jobs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/wire"
)

// TestPendingPickerExclude (spec §13.3): an excluded foreman never takes
// the job - a matched job goes to the next candidate, a pinned job pends.
func TestPendingPickerExclude(t *testing.T) {
	t.Parallel()
	blocked := func(_ context.Context, _ wire.JobSpec, id string) bool { return id == "foreman:alpha" }
	cases := []struct {
		name        string
		pin         string
		wantState   string
		wantForeman string
	}{
		{"matched job skips the excluded foreman", "", StateDispatched, "foreman:beta"},
		{"pinned job on an excluded foreman pends", "foreman:alpha", StatePending, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t)
			ctx := context.Background()
			if err := s.UpsertForeman(ctx, "foreman:alpha", json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
			NewLiveness(s).OnConnect("foreman:alpha")
			// alpha sorts first (fewest running, then id): without the
			// exclusion it would win.
			p := NewPendingPicker(s, &registry.CapabilityMatcher{Source: &stubForemen{rows: []registry.Foreman{
				{ID: "foreman:alpha", Online: true}, {ID: "foreman:beta", Online: true},
			}}})
			p.Exclude = blocked
			spec := newSpec(t, func(sp *wire.JobSpec) { sp.Foreman = tc.pin })
			if err := s.Create(ctx, spec); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Pump(ctx); err != nil {
				t.Fatal(err)
			}
			job, _, _ := s.Get(ctx, spec.ID)
			if job.State != tc.wantState || job.ForemanID != tc.wantForeman {
				t.Fatalf("state=%s foreman=%q, want %s %q", job.State, job.ForemanID, tc.wantState, tc.wantForeman)
			}
		})
	}
}
