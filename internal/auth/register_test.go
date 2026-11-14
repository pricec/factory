package auth

import (
	"context"
	"testing"
)

func TestRegisterPlannerBearer(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	const bearer = "planner-bearer-0123456789abcdef"
	if err := s.RegisterPlannerBearer(ctx, bearer); err != nil {
		t.Fatalf("RegisterPlannerBearer: %v", err)
	}

	if err := s.RegisterPlannerBearer(ctx, bearer); err != nil {
		t.Fatalf("RegisterPlannerBearer (idempotent): %v", err)
	}

	// The no-op did not add a second credential for the same hash.
	var rows int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM credentials WHERE token_hash = ?`, hashHex(bearer)).Scan(&rows); err != nil {
		t.Fatalf("count credentials: %v", err)
	}
	if rows != 1 {
		t.Errorf("credentials rows for the hash = %d, want 1", rows)
	}

	identity, jobID, err := s.Identify(ctx, bearer)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if identity != FactoryPlannerIdentity {
		t.Errorf("identity = %q, want %q", identity, FactoryPlannerIdentity)
	}
	if jobID != "" {
		t.Errorf("jobID = %q, want empty (no job scope)", jobID)
	}

	// A second, different bearer is a distinct credential.
	if err := s.RegisterPlannerBearer(ctx, "planner-bearer-second-987654321"); err != nil {
		t.Fatalf("RegisterPlannerBearer (second): %v", err)
	}
	id2, _, err := s.Identify(ctx, "planner-bearer-second-987654321")
	if err != nil {
		t.Fatalf("Identify second: %v", err)
	}
	if id2 != FactoryPlannerIdentity {
		t.Errorf("second identity = %q, want %q", id2, FactoryPlannerIdentity)
	}
}

func TestRegisterPlannerBearerEmpty(t *testing.T) {
	s := openStore(t)
	if err := s.RegisterPlannerBearer(context.Background(), ""); err == nil {
		t.Error("RegisterPlannerBearer(\"\") = nil, want error")
	}
}
