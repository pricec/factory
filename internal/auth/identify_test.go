package auth_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pricec/factory/internal/auth"
)

func newIdentifyStore(t *testing.T) *auth.Store {
	t.Helper()
	s, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIdentify(t *testing.T) {
	ctx := context.Background()
	store := newIdentifyStore(t)
	foremanInbox := "foreman:alpha"

	worker, err := store.MintWorkerToken(ctx, foremanInbox, "job_abc")
	if err != nil {
		t.Fatalf("MintWorkerToken: %v", err)
	}
	pair, _, err := store.IssuePairingToken(ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	foreman, _, err := store.PairForeman(ctx, pair, "alpha")
	if err != nil {
		t.Fatalf("PairForeman: %v", err)
	}

	tests := []struct {
		name        string
		bearer      string
		identity    string
		jobID       string
		unauthRE    string // non-empty: expect *UnauthorizedError with this reason
		revokeFirst bool
	}{
		{name: "worker token", bearer: worker, identity: "worker:job_abc", jobID: "job_abc"},
		{name: "foreman token has no job scope", bearer: foreman, identity: "foreman:alpha", jobID: ""},
		{name: "missing bearer", bearer: "", unauthRE: "missing bearer token"},
		{name: "unknown bearer", bearer: "nope", unauthRE: "unknown bearer token"},
		{name: "revoked worker token", bearer: worker, unauthRE: "revoked credential", revokeFirst: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.revokeFirst {
				if err := store.Revoke(ctx, "worker:job_abc"); err != nil {
					t.Fatalf("Revoke: %v", err)
				}
			}
			identity, jobID, err := store.Identify(ctx, tc.bearer)
			if tc.unauthRE != "" {
				var ue *auth.UnauthorizedError
				if !errors.As(err, &ue) {
					t.Fatalf("Identify(%q) error = %v, want *UnauthorizedError", tc.bearer, err)
				}
				if ue.Reason != tc.unauthRE {
					t.Errorf("Identify(%q) reason = %q, want %q", tc.bearer, ue.Reason, tc.unauthRE)
				}
				return
			}
			if err != nil {
				t.Fatalf("Identify(%q): %v", tc.bearer, err)
			}
			if identity != tc.identity {
				t.Errorf("Identify(%q).identity = %q, want %q", tc.bearer, identity, tc.identity)
			}
			if jobID != tc.jobID {
				t.Errorf("Identify(%q).jobID = %q, want %q", tc.bearer, jobID, tc.jobID)
			}
		})
	}
}
