package registry

import (
	"context"
	"errors"
	"testing"
)

// stubSource is a ForemanSource with controllable rows, for tests.
type stubSource struct {
	rows []Foreman
	err  error
}

func (s stubSource) Foremen(context.Context) ([]Foreman, error) {
	return s.rows, s.err
}

func TestCapabilityMatcherMatch(t *testing.T) {
	t.Parallel()
	f := func(id string, online bool, running int, caps map[string]string) Foreman {
		return Foreman{ID: id, Online: online, RunningJobs: running, Capabilities: caps}
	}
	tests := []struct {
		name     string
		rows     []Foreman
		requires []string
		wantOK   bool
		wantID   string
	}{
		{
			name:     "empty requires matches any online foreman, fewest running jobs wins",
			rows:     []Foreman{f("foreman:busy", true, 3, nil), f("foreman:alpha", true, 1, nil)},
			requires: nil,
			wantOK:   true,
			wantID:   "foreman:alpha",
		},
		{
			name:     "single tag",
			rows:     []Foreman{f("foreman:a", true, 0, map[string]string{"os": "linux"}), f("foreman:b", true, 0, nil)},
			requires: []string{"os"},
			wantOK:   true,
			wantID:   "foreman:a",
		},
		{
			name:     "multiple tags AND semantics",
			rows:     []Foreman{f("foreman:os-only", true, 0, map[string]string{"os": "linux"}), f("foreman:full", true, 0, map[string]string{"os": "linux", "gpu": "a100"})},
			requires: []string{"os", "gpu"},
			wantOK:   true,
			wantID:   "foreman:full",
		},
		{
			name:     "missing one of the required tags disqualifies",
			rows:     []Foreman{f("foreman:os-only", true, 0, map[string]string{"os": "linux"})},
			requires: []string{"os", "gpu"},
			wantOK:   false,
		},
		{
			name:     "name:value entries match tag name + value (2026-11-15 amendment)",
			rows:     []Foreman{f("foreman:linux-amd64", true, 0, map[string]string{"os": "linux", "arch": "amd64", "tools": "git,docker"})},
			requires: []string{"os:linux", "arch:amd64", "tools:git"},
			wantOK:   true,
			wantID:   "foreman:linux-amd64",
		},
		{
			name:     "name:value value mismatch disqualifies",
			rows:     []Foreman{f("foreman:darwin", true, 0, map[string]string{"os": "darwin", "arch": "arm64"})},
			requires: []string{"os:linux", "arch:amd64"},
			wantOK:   false,
		},
		{
			name:     "bare capability values are not tag names",
			rows:     []Foreman{f("foreman:linux", true, 0, map[string]string{"os": "linux", "arch": "amd64"})},
			requires: []string{"linux", "amd64"},
			wantOK:   false,
		},
		{
			name:     "empty tag value counts as absent",
			rows:     []Foreman{f("foreman:empty-gpu", true, 0, map[string]string{"gpu": ""}), f("foreman:none", true, 0, nil)},
			requires: []string{"gpu"},
			wantOK:   false,
		},
		{
			name:     "offline foreman excluded even when capable",
			rows:     []Foreman{f("foreman:offline", false, 0, map[string]string{"gpu": "a100"}), f("foreman:online", true, 0, nil)},
			requires: []string{"gpu"},
			wantOK:   false,
		},
		{
			name:     "offline excluded, online without the tag picked",
			rows:     []Foreman{f("foreman:offline", false, 0, map[string]string{"os": "linux"})},
			requires: nil,
			wantOK:   false,
		},
		{
			name:     "tie-break: same running_jobs -> lexicographic id",
			rows:     []Foreman{f("foreman:zeta", true, 2, map[string]string{"gpu": "a100"}), f("foreman:beta", true, 2, map[string]string{"gpu": "h100"})},
			requires: []string{"gpu"},
			wantOK:   true,
			wantID:   "foreman:beta",
		},
		{
			name:     "running_jobs asc beats id order",
			rows:     []Foreman{f("foreman:a", true, 5, map[string]string{"gpu": "a100"}), f("foreman:z", true, 1, map[string]string{"gpu": "h100"})},
			requires: []string{"gpu"},
			wantOK:   true,
			wantID:   "foreman:z",
		},
		{
			name:     "capable but loaded foreman loses to idle one",
			rows:     []Foreman{f("foreman:loaded", true, 4, map[string]string{"gpu": "a100"}), f("foreman:idle", true, 0, map[string]string{"gpu": "a100"})},
			requires: []string{"gpu"},
			wantOK:   true,
			wantID:   "foreman:idle",
		},
		{
			name:     "no foremen at all",
			rows:     nil,
			requires: []string{"gpu"},
			wantOK:   false,
		},
		{
			name:     "all foremen offline",
			rows:     []Foreman{f("foreman:a", false, 0, map[string]string{"gpu": "a100"})},
			requires: nil,
			wantOK:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &CapabilityMatcher{Source: stubSource{rows: tt.rows}}
			got, ok, err := m.Match(context.Background(), tt.requires)
			if err != nil {
				t.Fatalf("Match: unexpected error: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got.ID != tt.wantID {
				t.Fatalf("picked %q, want %q", got.ID, tt.wantID)
			}
		})
	}
}

// TestCapabilityMatcherSourceOrderIndependent proves the FirstOnline stub
// behavior is gone: a requires=["gpu"] match must pick the capable foreman
// even when a non-capable one appears earlier in the source list.
func TestCapabilityMatcherSourceOrderIndependent(t *testing.T) {
	t.Parallel()
	rows := []Foreman{
		{ID: "foreman:first", Online: true, RunningJobs: 0, Capabilities: map[string]string{"os": "linux"}},
		{ID: "foreman:gpu", Online: true, RunningJobs: 0, Capabilities: map[string]string{"os": "linux", "gpu": "a100"}},
	}
	m := &CapabilityMatcher{Source: stubSource{rows: rows}}
	got, ok, err := m.Match(context.Background(), []string{"gpu"})
	if err != nil {
		t.Fatalf("Match: unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got.ID != "foreman:gpu" {
		t.Fatalf("picked %q, want the GPU foreman %q (FirstOnline would pick the first row)", got.ID, "foreman:gpu")
	}
}

func TestCapabilityMatcherSourceError(t *testing.T) {
	t.Parallel()
	srcErr := errors.New("registry: store unavailable")
	m := &CapabilityMatcher{Source: stubSource{err: srcErr}}
	got, ok, err := m.Match(context.Background(), nil)
	if !errors.Is(err, srcErr) {
		t.Fatalf("err = %v, want %v", err, srcErr)
	}
	if ok || got.ID != "" || got.Online || got.RunningJobs != 0 || got.Capabilities != nil {
		t.Fatalf("got (%v, %v), want zero foreman, ok=false", got, ok)
	}
}
