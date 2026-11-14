// Package inbox implements the durable inbox service (spec §4): a
// SQLite-backed, FIFO, explicit-ack, at-least-once message store with
// long-poll wake-ups. It is shared by the factory and foremen.
package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pricec/factory/internal/wire"
	_ "modernc.org/sqlite"
)

// ErrClosed is returned by operations on a closed Service.
var ErrClosed = errors.New("inbox: closed")

// DuplicateError reports a job.result POST whose (inbox_id, job_id) already
// exists (spec §4.2). OriginalID is the id of the already-stored envelope;
// it rides the 409 response (spec §10).
type DuplicateError struct {
	InboxID    string
	JobID      string
	OriginalID string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("duplicate job.result for (%s, %s): original message %s", e.InboxID, e.JobID, e.OriginalID)
}

// Service is a durable inbox store. It is safe for concurrent use.
type Service struct {
	db *sql.DB

	mu      sync.Mutex
	waiters map[string][]chan struct{}
	closed  bool
}

const dsnSuffix = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"

// Open opens (creating if needed) the inbox database at path (spec §4:
// WAL mode, busy_timeout 5s).
func Open(path string) (*Service, error) {
	db, err := sql.Open("sqlite", "file:"+path+dsnSuffix)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Service{
		db:      db,
		waiters: make(map[string][]chan struct{}),
	}, nil
}

func migrate(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS messages (
	id       TEXT NOT NULL UNIQUE,
	inbox_id TEXT NOT NULL,
	type     TEXT NOT NULL,
	job_id   TEXT NOT NULL DEFAULT '',
	sender   TEXT NOT NULL,
	payload  TEXT NOT NULL,
	ts       TEXT NOT NULL,
	seq      INTEGER PRIMARY KEY AUTOINCREMENT,
	acked    INTEGER NOT NULL DEFAULT 0,
	acked_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_messages_inbox_unacked
	ON messages(inbox_id, seq) WHERE acked = 0;
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_result_dedupe
	ON messages(inbox_id, job_id) WHERE type = 'job.result';
`
	_, err := db.Exec(ddl)
	return err
}

// Close stops all long-poll waiters and closes the database.
func (s *Service) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for _, ws := range s.waiters {
			for _, ch := range ws {
				close(ch)
			}
		}
		s.waiters = make(map[string][]chan struct{})
	}
	s.mu.Unlock()
	return s.db.Close()
}

// Post stores env in inboxID and returns the full envelope with
// server-assigned id and ts (spec §4.1). Duplicate job.result posts for the
// same (inboxID, job_id) return *DuplicateError and store nothing.
func (s *Service) Post(ctx context.Context, inboxID string, env wire.MessageEnvelope) (wire.MessageEnvelope, error) {
	env.ID = wire.NewMessageID()
	env.TS = time.Now().UTC().Format(time.RFC3339)

	if env.Type == "job.result" {
		orig, dup, err := s.existingResult(ctx, inboxID, env.JobID)
		if err != nil {
			return env, err
		}
		if dup {
			return env, &DuplicateError{InboxID: inboxID, JobID: env.JobID, OriginalID: orig}
		}
	}

	payload := "null"
	if len(env.Payload) > 0 {
		payload = string(env.Payload)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (id, inbox_id, type, job_id, sender, payload, ts)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		env.ID, inboxID, env.Type, env.JobID, env.From, payload, env.TS)
	if err != nil {
		// Concurrent duplicate post lost the race; the unique partial index
		// is the second line of defense (spec §4.2, §4.3).
		if env.Type == "job.result" && strings.Contains(err.Error(), "UNIQUE") {
			if orig, dup, qerr := s.existingResult(ctx, inboxID, env.JobID); qerr == nil && dup {
				return env, &DuplicateError{InboxID: inboxID, JobID: env.JobID, OriginalID: orig}
			}
		}
		return env, err
	}
	s.notify(inboxID)
	return env, nil
}

func (s *Service) existingResult(ctx context.Context, inboxID, jobID string) (id string, dup bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM messages WHERE inbox_id = ? AND job_id = ? AND type = 'job.result'`,
		inboxID, jobID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// Get returns up to limit unacked envelopes for inboxID in FIFO order
// (spec §4.1). When wait > 0 and the inbox is empty, it blocks until a
// message arrives, the context is canceled, or wait elapses. Waiters are
// registered before the first query so a POST landing between check and
// sleep cannot be missed.
func (s *Service) Get(ctx context.Context, inboxID string, limit int, wait time.Duration) ([]wire.MessageEnvelope, error) {
	if wait <= 0 {
		return s.list(ctx, inboxID, limit)
	}
	ch, err := s.registerWaiter(inboxID)
	if err != nil {
		return nil, err
	}
	defer s.removeWaiter(inboxID, ch)

	deadline := time.Now().Add(wait)
	for {
		envs, err := s.list(ctx, inboxID, limit)
		if err != nil || len(envs) > 0 {
			return envs, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return envs, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch: // woke by a POST; re-list
		case <-time.After(remaining):
			return envs, nil
		}
	}
}

func (s *Service) list(ctx context.Context, inboxID string, limit int) ([]wire.MessageEnvelope, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, type, job_id, sender, payload, ts
		 FROM messages WHERE inbox_id = ? AND acked = 0 ORDER BY seq LIMIT ?`,
		inboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var envs []wire.MessageEnvelope
	for rows.Next() {
		var env wire.MessageEnvelope
		var payload string
		if err := rows.Scan(&env.ID, &env.Type, &env.JobID, &env.From, &payload, &env.TS); err != nil {
			return nil, err
		}
		env.Payload = jsonRaw(payload)
		envs = append(envs, env)
	}
	return envs, rows.Err()
}

// GetMessage returns the stored envelope with the given id (spec §4.1:
// replay/UI path), including acked messages. ok is false when absent.
func (s *Service) GetMessage(ctx context.Context, mid string) (env wire.MessageEnvelope, ok bool, err error) {
	var payload string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, type, job_id, sender, payload, ts FROM messages WHERE id = ?`, mid).
		Scan(&env.ID, &env.Type, &env.JobID, &env.From, &payload, &env.TS)
	if errors.Is(err, sql.ErrNoRows) {
		return env, false, nil
	}
	if err != nil {
		return env, false, err
	}
	env.Payload = jsonRaw(payload)
	return env, true, nil
}

// Ack marks the given message ids in inboxID as acknowledged (spec §4.1).
// Acking an unknown or already-acked id is not an error.
func (s *Service) Ack(ctx context.Context, inboxID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+2)
	args = append(args, time.Now().UTC().Format(time.RFC3339), inboxID)
	ph := make([]string, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET acked = 1, acked_at = ? WHERE inbox_id = ? AND id IN (`+strings.Join(ph, ",")+`)`,
		args...)
	return err
}

func (s *Service) registerWaiter(inboxID string) (chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	ch := make(chan struct{})
	s.waiters[inboxID] = append(s.waiters[inboxID], ch)
	return ch, nil
}

func (s *Service) removeWaiter(inboxID string, ch chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws := s.waiters[inboxID]
	for i, c := range ws {
		if c == ch {
			ws = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(ws) == 0 {
		delete(s.waiters, inboxID)
	} else {
		s.waiters[inboxID] = ws
	}
}

// notify wakes all current waiters for inboxID. Closing the shared channels
// is a broadcast; woken waiters re-list.
func (s *Service) notify(inboxID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.waiters[inboxID] {
		close(ch)
	}
	delete(s.waiters, inboxID)
}

// jsonNull is the payload stand-in for messages posted without one.
var jsonNull = []byte("null")

func jsonRaw(payload string) []byte {
	if payload == "" {
		return jsonNull
	}
	return []byte(payload)
}
