package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestJobSpecSecretsValidation: spec validation table for the secrets
// list (names only; existence is checked by the create paths).
func TestJobSpecSecretsValidation(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		wantErr string
	}{
		{"nil", nil, ""},
		{"one", []string{"github-push"}, ""},
		{"several", []string{"a", "b-2", "c"}, ""},
		{"empty name", []string{""}, "secrets"},
		{"uppercase", []string{"GitHub"}, "secrets"},
		{"underscore", []string{"gh_token"}, "secrets"},
		{"too long", []string{strings.Repeat("a", 65)}, "secrets"},
		{"one bad of two", []string{"ok", "no way"}, "secrets"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := NewJobSpec("task", "test")
			spec.Secrets = tc.secrets
			err := spec.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestJobSpecSecretsJSON: secrets round-trips as names and is omitted when
// empty.
func TestJobSpecSecretsJSON(t *testing.T) {
	spec := NewJobSpec("task", "test")
	b, _ := json.Marshal(spec)
	if strings.Contains(string(b), "secrets") {
		t.Errorf("empty secrets must be omitted: %s", b)
	}
	spec.Secrets = []string{"github-push"}
	b, _ = json.Marshal(spec)
	var back JobSpec
	if err := json.Unmarshal(b, &back); err != nil || len(back.Secrets) != 1 || back.Secrets[0] != "github-push" {
		t.Errorf("round trip = %+v, %v", back.Secrets, err)
	}
}
