// Package auth implements pairing and scoped bearer credentials (spec §7):
// one-time pairing tokens, SHA-256-hashed bearers with per-inbox read/post
// scopes, worker-token minting and job invalidation, and the inbox
// Authenticator that enforces 401 (unknown/invalid) vs 403 (wrong scope).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite"
)

// Pinned constants (spec §7, §11).
const (
	pairingTTL       = 15 * time.Minute
	factoryMainInbox = "factory:main"
)

// UnauthorizedError signals 401: unknown, malformed, expired, or revoked
// credentials (spec §7.3).
type UnauthorizedError struct{ Reason string }

func (e *UnauthorizedError) Error() string { return "unauthorized: " + e.Reason }

// HTTPStatus implements inbox.StatusError.
func (e *UnauthorizedError) HTTPStatus() int { return 401 }

// ForbiddenError signals 403: valid credential, missing scope for the
// requested action (spec §7.3).
type ForbiddenError struct{ Reason string }

func (e *ForbiddenError) Error() string { return "forbidden: " + e.Reason }

// HTTPStatus implements inbox.StatusError.
func (e *ForbiddenError) HTTPStatus() int { return 403 }

// Store is the credential database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
	// path is the sqlite file path (used by tests to inspect the DB at rest).
	path string
	// Now is the clock; injectable for TTL tests.
	Now func() time.Time
}

const dsnSuffix = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"

// Open opens (creating if needed) the auth database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+dsnSuffix)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path, Now: func() time.Time { return time.Now().UTC() }}, nil
}

func migrate(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS credentials (
	cred_id    TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	identity   TEXT NOT NULL,
	job_id     TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	revoked    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS credential_scopes (
	cred_id  TEXT NOT NULL,
	inbox_id TEXT NOT NULL,
	may_read INTEGER NOT NULL DEFAULT 0,
	may_post INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (cred_id, inbox_id)
);
CREATE TABLE IF NOT EXISTS pairing_tokens (
	token_hash TEXT PRIMARY KEY,
	expires_at TEXT NOT NULL,
	used       INTEGER NOT NULL DEFAULT 0
);
`
	_, err := db.Exec(ddl)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// now returns the store clock in RFC3339.
func (s *Store) now() string { return s.Now().UTC().Format(time.RFC3339) }

// IssuePairingToken generates a one-time pairing token (32B random hex,
// 15-minute TTL, spec §7.1) and stores only its hash.
func (s *Store) IssuePairingToken(ctx context.Context) (token string, expiresAt time.Time, err error) {
	token = randomHex()
	expiresAt = s.Now().Add(pairingTTL)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO pairing_tokens (token_hash, expires_at, used) VALUES (?, ?, 0)`,
		hashHex(token), expiresAt.UTC().Format(time.RFC3339))
	return token, expiresAt, err
}

// PairForeman burns a pairing token and mints a foreman credential (spec
// §7.1, §7.3): read+post on its own inbox foreman:{id}, post-only on
// factory:main. selfID is the bare foreman id; identity is the full inbox
// id foreman:{id}.
func (s *Store) PairForeman(ctx context.Context, pairingToken, selfID string) (bearer, inboxID string, err error) {
	if err := s.burnPairingToken(ctx, pairingToken); err != nil {
		return "", "", err
	}
	if selfID == "" {
		return "", "", &UnauthorizedError{Reason: "self.id: required"}
	}
	inboxID = "foreman:" + selfID
	bearer = randomHex()
	err = s.insertCredential(ctx, bearer, inboxID, "", []scope{
		{inboxID, true, true},
		{factoryMainInbox, false, true},
	})
	return bearer, inboxID, err
}

// MintWorkerToken mints a per-job worker credential (spec §7.3): post-only
// on the foreman's own inbox (job.result, job.question), read-only on the
// per-job inbox job:{job_id} (job.answer). A library call, not an HTTP
// endpoint.
func (s *Store) MintWorkerToken(ctx context.Context, parentInboxID, jobID string) (string, error) {
	bearer := randomHex()
	err := s.insertCredential(ctx, bearer, "worker:"+jobID, jobID, []scope{
		{parentInboxID, false, true},
		{"job:" + jobID, true, false},
	})
	return bearer, err
}

type scope struct {
	inboxID          string
	mayRead, mayPost bool
}

func (s *Store) insertCredential(ctx context.Context, bearer, identity, jobID string, scopes []scope) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	tokenHash := hashHex(bearer)
	credID := "cred_" + ulid.Make().String()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO credentials (cred_id, token_hash, identity, job_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		credID, tokenHash, identity, jobID, s.now()); err != nil {
		return err
	}
	for _, sc := range scopes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO credential_scopes (cred_id, inbox_id, may_read, may_post) VALUES (?, ?, ?, ?)`,
			credID, sc.inboxID, b2i(sc.mayRead), b2i(sc.mayPost)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// burnPairingToken consumes a pairing token: exactly-once, within TTL. The
// conditional UPDATE makes double-use impossible even concurrently.
func (s *Store) burnPairingToken(ctx context.Context, token string) error {
	if token == "" {
		return &UnauthorizedError{Reason: "pairing_token: required"}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE pairing_tokens SET used = 1
		 WHERE token_hash = ? AND used = 0 AND expires_at > ?`,
		hashHex(token), s.now())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &UnauthorizedError{Reason: "pairing token expired or already used"}
	}
	return nil
}

// InvalidateJob revokes every credential minted for jobID (spec §7.4:
// terminal-transition invalidation, wired by #9 via OnTerminal).
func (s *Store) InvalidateJob(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE credentials SET revoked = 1 WHERE job_id = ? AND revoked = 0`, jobID)
	return err
}

// Revoke revokes every credential for identity (spec §7.2: delete/ revoke
// -> 401).
func (s *Store) Revoke(ctx context.Context, identity string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE credentials SET revoked = 1 WHERE identity = ? AND revoked = 0`, identity)
	return err
}

// credential is the lookup result for a bearer.
type credential struct {
	credID   string
	identity string
}

// lookup resolves a bearer to its credential. Unknown or revoked bearers
// return *UnauthorizedError. Only the SHA-256 of the presented token is
// ever compared (spec §7.2); the random 32B digest lookup is not a
// meaningful timing oracle.
func (s *Store) lookup(ctx context.Context, bearer string) (credential, error) {
	if bearer == "" {
		return credential{}, &UnauthorizedError{Reason: "missing bearer token"}
	}
	var cred credential
	var revoked int
	err := s.db.QueryRowContext(ctx,
		`SELECT cred_id, identity, revoked FROM credentials WHERE token_hash = ?`,
		hashHex(bearer)).Scan(&cred.credID, &cred.identity, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return credential{}, &UnauthorizedError{Reason: "unknown bearer token"}
	}
	if err != nil {
		return credential{}, err
	}
	if revoked != 0 {
		return credential{}, &UnauthorizedError{Reason: "revoked credential"}
	}
	return cred, nil
}

// canDo reports whether the credential may perform action on inboxID
// (spec §7.3: post -> may_post; read, ack -> may_read).
func (s *Store) canDo(ctx context.Context, credID, inboxID, action string) (bool, error) {
	mayRead, mayPost := 0, 0
	err := s.db.QueryRowContext(ctx,
		`SELECT may_read, may_post FROM credential_scopes WHERE cred_id = ? AND inbox_id = ?`,
		credID, inboxID).Scan(&mayRead, &mayPost)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch action {
	case "post":
		return mayPost != 0, nil
	case "read", "ack":
		return mayRead != 0, nil
	}
	return false, fmt.Errorf("unknown action %q", action)
}

func randomHex() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand failure: " + err.Error()) // unreachable per crypto/rand docs
	}
	return hex.EncodeToString(b[:])
}

func hashHex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// trimBearer extracts the bearer token from an Authorization header.
func trimBearer(h string) string {
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
