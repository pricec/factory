package registry

import "testing"

func TestInboxID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare id", "server", "foreman:server"},
		{"already prefixed", "foreman:server", "foreman:server"},
		{"empty", "", "foreman:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := InboxID(tc.in); got != tc.want {
				t.Errorf("InboxID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBareID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"prefixed", "foreman:server", "server"},
		{"bare", "server", "server"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BareID(tc.in); got != tc.want {
				t.Errorf("BareID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSameForeman(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"both bare", "server", "alpha", false},
		{"bare vs full", "server", "foreman:server", true},
		{"full vs bare", "foreman:server", "server", true},
		{"both full", "foreman:server", "foreman:server", true},
		{"full vs full", "foreman:server", "foreman:alpha", false},
		{"empty vs full", "", "foreman:server", false},
		{"full vs empty", "foreman:server", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameForeman(tc.a, tc.b); got != tc.want {
				t.Errorf("SameForeman(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
