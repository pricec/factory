package jobs

import (
	"context"
	"testing"

	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/wire"
)

// stubForemen is a fixed registry snapshot for picker tests (the
// foremanSource twin; jobs cannot import factoryapp).
type stubForemen struct{ rows []registry.Foreman }

func (s *stubForemen) Foremen(context.Context) ([]registry.Foreman, error) {
	return s.rows, nil
}

// TestPendingPickerPump covers the pending -> dispatched decision (spec
// §6.7): only an online foreman satisfying every required tag dispatches;
// everything else stays pending, and a second pass never re-dispatches.
func TestPendingPickerPump(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		foremen     []registry.Foreman
		requires    []string
		wantState   string
		wantForeman string
	}{
		{
			name:      "no foremen registered",
			wantState: StatePending,
		},
		{
			name:      "offline foreman is unmatchable",
			foremen:   []registry.Foreman{{ID: "foreman:alpha", Capabilities: map[string]string{"gpu": "true"}}},
			wantState: StatePending,
		},
		{
			name:      "capability mismatch stays pending",
			foremen:   []registry.Foreman{{ID: "foreman:alpha", Online: true, Capabilities: map[string]string{"os": "linux"}}},
			requires:  []string{"gpu"},
			wantState: StatePending,
		},
		{
			name:        "online matching foreman dispatches",
			foremen:     []registry.Foreman{{ID: "foreman:alpha", Online: true}},
			wantState:   StateDispatched,
			wantForeman: "foreman:alpha",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t)
			p := NewPendingPicker(s, &registry.CapabilityMatcher{Source: &stubForemen{rows: tc.foremen}})
			ctx := context.Background()

			spec := newSpec(t, func(sp *wire.JobSpec) { sp.Requires = tc.requires })
			if err := s.Create(ctx, spec); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Pump(ctx); err != nil {
				t.Fatalf("Pump: %v", err)
			}
			job, ok, err := s.Get(ctx, spec.ID)
			if err != nil || !ok {
				t.Fatalf("Get: %v (ok %v)", err, ok)
			}
			if job.State != tc.wantState {
				t.Errorf("state = %s, want %s", job.State, tc.wantState)
			}
			if job.ForemanID != tc.wantForeman {
				t.Errorf("foreman_id = %q, want %q", job.ForemanID, tc.wantForeman)
			}

			// Idempotence: a dispatched job left the pending set, so a
			// second pass is a no-op; a pending one just re-matches.
			n, err := p.Pump(ctx)
			if err != nil {
				t.Fatalf("second Pump: %v", err)
			}
			if n != 0 {
				t.Errorf("second Pump dispatched %d, want 0", n)
			}
		})
	}
}

// TestPendingPickerDispatchesEveryPendingJob: one qualifying foreman picks
// up the whole pending backlog in a single pass.
func TestPendingPickerDispatchesEveryPendingJob(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	p := NewPendingPicker(s, &registry.CapabilityMatcher{Source: &stubForemen{
		rows: []registry.Foreman{{ID: "foreman:alpha", Online: true, Capabilities: map[string]string{"gpu": "true"}}},
	}})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		spec := newSpec(t, func(sp *wire.JobSpec) { sp.Requires = []string{"gpu"} })
		if err := s.Create(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	n, err := p.Pump(ctx)
	if err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if n != 3 {
		t.Fatalf("Pump dispatched %d, want 3", n)
	}
	pending, err := s.List(ctx, StatePending, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending jobs left = %d, want 0", len(pending))
	}
}
