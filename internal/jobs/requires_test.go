package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/registry"
)

// TestStoreValidateRequires: the creation paths' validation wrapper (spec
// §6.7, 2026-11-15 amendment) - tag names against the union of registered
// foremen tag keys, never capability values.
func TestStoreValidateRequires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)

	// No foremen registered: loud warning, creation allowed (the pending
	// picker dispatches when a foreman arrives).
	warnings, err := s.ValidateRequires(ctx, []string{"os:linux"})
	if err != nil {
		t.Fatalf("ValidateRequires with no foremen: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatalf("warnings = %v, want a loud warning", warnings)
	}
	if !strings.Contains(warnings[0], "no foremen registered") {
		t.Errorf("warning = %q, want it to say no foremen are registered", warnings[0])
	}

	// Register the standard fleet tags.
	for _, tt := range []struct {
		id  string
		cap map[string]string
	}{
		{"foreman:a", map[string]string{"os": "linux", "arch": "amd64", "tools": "git,docker"}},
		{"foreman:b", map[string]string{"os": "darwin", "arch": "arm64"}},
	} {
		b, err := json.Marshal(tt.cap)
		if err != nil {
			t.Fatalf("marshal caps: %v", err)
		}
		if err := s.UpsertForeman(ctx, tt.id, b); err != nil {
			t.Fatalf("UpsertForeman(%s): %v", tt.id, err)
		}
	}

	// Accept: known tag names, bare and name:value.
	for _, requires := range [][]string{
		{"os"},
		{"os:linux", "arch:amd64", "tools:git"},
	} {
		warnings, err := s.ValidateRequires(ctx, requires)
		if err != nil {
			t.Errorf("ValidateRequires(%v): %v", requires, err)
		}
		if warnings != nil {
			t.Errorf("ValidateRequires(%v) warnings = %v, want nil", requires, warnings)
		}
	}

	// Reject: capability values are not tag names (the incident case), and
	// invented tags. The store rows' capabilities JSON must be decoded, so
	// this exercises the wrapper, not just registry.ValidateRequires.
	for _, tt := range []struct {
		requires []string
		hint     bool
	}{
		{[]string{"linux", "amd64"}, true},
		{[]string{"quantum"}, false},
	} {
		warnings, err := s.ValidateRequires(ctx, tt.requires)
		var ure *registry.UnknownRequiresError
		if !errors.As(err, &ure) {
			t.Fatalf("ValidateRequires(%v) error = %v, want *registry.UnknownRequiresError", tt.requires, err)
		}
		if !slices.Equal(ure.Unknown, tt.requires) {
			t.Errorf("Unknown = %v, want %v", ure.Unknown, tt.requires)
		}
		if tt.hint && len(ure.Hints) == 0 {
			t.Errorf("Hints = %v, want the values-not-names hint for %v", ure.Hints, tt.requires)
		}
		if warnings != nil {
			t.Errorf("warnings = %v, want nil on error", warnings)
		}
	}
}
