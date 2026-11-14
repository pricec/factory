package auth_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pricec/factory/internal/auth"
)

func TestMintPlannerToken(t *testing.T) {
	ctx := context.Background()
	store, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	bearer, err := store.MintPlannerToken(ctx, "factory:main")
	if err != nil {
		t.Fatalf("MintPlannerToken: %v", err)
	}
	if bearer == "" {
		t.Fatal("MintPlannerToken: empty bearer")
	}

	// Identity is pinned factory:planner with no job scope.
	identity, jobID, err := store.Identify(ctx, bearer)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if identity != auth.FactoryPlannerIdentity {
		t.Errorf("identity = %q, want %q", identity, auth.FactoryPlannerIdentity)
	}
	if jobID != "" {
		t.Errorf("jobID = %q, want empty (no job scope)", jobID)
	}

	// No inbox scopes: the inbox Authenticator refuses every action on
	// every inbox for this credential.
	for _, action := range []string{"read", "post", "ack"} {
		req := httptest.NewRequest("POST", "/v1/messages/foreman:alpha", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		err := auth.NewAuthenticator(store).Authorize(req, "foreman:alpha", action)
		var fe *auth.ForbiddenError
		if !errors.As(err, &fe) {
			t.Errorf("Authorize(%s) with planner token: error = %v, want *ForbiddenError", action, err)
		}
	}

	// A second mint yields a distinct bearer.
	other, err := store.MintPlannerToken(ctx, "factory:main")
	if err != nil {
		t.Fatalf("second MintPlannerToken: %v", err)
	}
	if other == bearer {
		t.Error("second mint produced the same bearer")
	}
}
