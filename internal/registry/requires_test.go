package registry

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRequiresTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		entry string
		want  string
	}{
		{"os", "os"},
		{"os:linux", "os"},
		{"tools:git", "tools"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := RequiresTag(tt.entry); got != tt.want {
			t.Errorf("RequiresTag(%q) = %q, want %q", tt.entry, got, tt.want)
		}
	}
}

func TestTagKeys(t *testing.T) {
	t.Parallel()
	foremen := []Foreman{
		{ID: "foreman:beta", Capabilities: map[string]string{"arch": "amd64", "dead": ""}},
		{ID: "foreman:alpha", Capabilities: map[string]string{"os": "linux", "arch": "arm64"}},
		{ID: "foreman:gamma"},
	}
	// Sorted union; empty-valued "dead" is skipped (could never match).
	got := TagKeys(foremen)
	want := []string{"arch", "os"}
	if !slices.Equal(got, want) {
		t.Errorf("TagKeys() = %v, want %v", got, want)
	}
	if got := TagKeys(nil); len(got) != 0 {
		t.Errorf("TagKeys(nil) = %v, want empty", got)
	}
}

func TestValidateRequires(t *testing.T) {
	t.Parallel()
	// The standard deployed fleet: tag keys os/arch/tools, values
	// linux/amd64 and a comma-separated tools list.
	fleet := []Foreman{
		{ID: "foreman:a", Online: true, Capabilities: map[string]string{"os": "linux", "arch": "amd64", "tools": "git,docker"}},
		{ID: "foreman:b", Online: false, Capabilities: map[string]string{"os": "darwin", "arch": "arm64"}},
	}
	tests := []struct {
		name         string
		requires     []string
		foremen      []Foreman
		wantErr      bool
		wantWarnings bool
		wantUnknown  []string
		wantHints    []string
	}{
		{
			name:     "empty requires always validates",
			requires: nil,
			foremen:  fleet,
		},
		{
			name:     "accept bare known tag name",
			requires: []string{"os", "tools"},
			foremen:  fleet,
		},
		{
			name:     "accept name:value for known tag",
			requires: []string{"os:linux", "arch:amd64", "tools:git"},
			foremen:  fleet,
		},
		{
			name:        "reject values-as-names: the 2026-11-15 incident case",
			requires:    []string{"linux", "amd64"},
			foremen:     fleet,
			wantErr:     true,
			wantUnknown: []string{"linux", "amd64"},
			wantHints:   []string{"linux", "amd64"},
		},
		{
			name:        "reject unknown invented tag, no hint",
			requires:    []string{"quantum"},
			foremen:     fleet,
			wantErr:     true,
			wantUnknown: []string{"quantum"},
		},
		{
			name:        "unknown among known entries rejects the unknown one",
			requires:    []string{"os:linux", "linux", "gpu"},
			foremen:     fleet,
			wantErr:     true,
			wantUnknown: []string{"linux", "gpu"},
			wantHints:   []string{"linux"},
		},
		{
			name:         "no foremen registered: loud warning, not an error",
			requires:     []string{"os:linux"},
			foremen:      nil,
			wantWarnings: true,
		},
		{
			name:     "empty requires with no foremen: nothing to validate",
			requires: nil,
			foremen:  nil,
		},
		{
			name:     "offline foremen still count as registered keys",
			requires: []string{"arch:arm64"},
			foremen:  []Foreman{{ID: "foreman:offline", Capabilities: map[string]string{"os": "darwin", "arch": "arm64"}}},
		},
		{
			name:         "only empty-valued tag keys: nothing usable registered, warning",
			requires:     []string{"gpu"},
			foremen:      []Foreman{{ID: "foreman:x", Capabilities: map[string]string{"gpu": ""}}},
			wantWarnings: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			warnings, err := ValidateRequires(tt.requires, tt.foremen)
			if tt.wantErr {
				var ure *UnknownRequiresError
				if !errors.As(err, &ure) {
					t.Fatalf("ValidateRequires(%v) error = %v, want *UnknownRequiresError", tt.requires, err)
					return
				}
				if len(tt.wantUnknown) > 0 && !slices.Equal(ure.Unknown, tt.wantUnknown) {
					t.Errorf("Unknown = %v, want %v", ure.Unknown, tt.wantUnknown)
				}
				if len(tt.wantHints) > 0 && !slices.Equal(ure.Hints, tt.wantHints) {
					t.Errorf("Hints = %v, want %v", ure.Hints, tt.wantHints)
				}
				if len(ure.Known) == 0 {
					t.Errorf("Known = %v, want the registered tag keys", ure.Known)
				}
				if warnings != nil {
					t.Errorf("warnings = %v, want nil on error", warnings)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateRequires(%v) unexpected error: %v", tt.requires, err)
			}
			if tt.wantWarnings && len(warnings) == 0 {
				t.Errorf("ValidateRequires(%v) warnings = %v, want a loud warning", tt.requires, warnings)
			}
			if !tt.wantWarnings && warnings != nil {
				t.Errorf("warnings = %v, want nil", warnings)
			}
		})
	}
}

func TestUnknownRequiresErrorMessage(t *testing.T) {
	t.Parallel()
	// The message must teach the fix: tag names, not values.
	err := (&UnknownRequiresError{
		Unknown: []string{"linux", "amd64"},
		Known:   []string{"arch", "os", "tools"},
		Hints:   []string{"linux", "amd64"},
	}).Error()
	for _, want := range []string{"linux", "amd64", "os:linux", "arch:amd64", "tools:git", "capability values"} {
		if !strings.Contains(err, want) {
			t.Errorf("error message %q missing %q", err, want)
		}
	}
}
