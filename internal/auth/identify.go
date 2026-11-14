package auth

import (
	"context"
	"database/sql"
	"errors"
)

// Identify resolves a bearer token to its credential identity and job scope
// (spec §7.3). Worker tokens carry a job_id; foreman and factory
// credentials carry an empty one. Missing, unknown, or revoked bearers
// return *UnauthorizedError. Only the SHA-256 of the presented token is
// compared, as in Store.lookup.
func (s *Store) Identify(ctx context.Context, bearer string) (identity string, jobID string, err error) {
	if bearer == "" {
		return "", "", &UnauthorizedError{Reason: "missing bearer token"}
	}
	var revoked int
	err = s.db.QueryRowContext(ctx,
		`SELECT identity, job_id, revoked FROM credentials WHERE token_hash = ?`,
		hashHex(bearer)).Scan(&identity, &jobID, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", &UnauthorizedError{Reason: "unknown bearer token"}
	}
	if err != nil {
		return "", "", err
	}
	if revoked != 0 {
		return "", "", &UnauthorizedError{Reason: "revoked credential"}
	}
	return identity, jobID, nil
}
