package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// EnvPlannerToken is the environment variable carrying a pre-arranged
// factory planner bearer (issue #20): when set, the factory registers it
// instead of minting a fresh token.
const EnvPlannerToken = "FACTORY_PLANNER_TOKEN"

// RegisterPlannerBearer registers a pre-arranged planner bearer under the
// factory:planner identity with no scopes (spec §9.4): it authorizes only
// the planner MCP surface. Idempotent: registering a bearer whose SHA-256
// is already stored is a no-op; an empty bearer is an error.
func (s *Store) RegisterPlannerBearer(ctx context.Context, bearer string) error {
	if bearer == "" {
		return errors.New("auth: RegisterPlannerBearer: bearer must be non-empty")
	}
	hash := hashHex(bearer)
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM credentials WHERE token_hash = ?`, hash).Scan(&one)
	switch {
	case err == nil:
		return nil // same hash already registered: no-op
	case errors.Is(err, sql.ErrNoRows):
		if err := s.insertCredential(ctx, bearer, FactoryPlannerIdentity, "", nil); err != nil {
			// A concurrent registration of the same hash wins the unique
			// token_hash race; that still satisfies the contract.
			if strings.Contains(err.Error(), "UNIQUE") {
				return nil
			}
			return err
		}
		return nil
	default:
		return err
	}
}
