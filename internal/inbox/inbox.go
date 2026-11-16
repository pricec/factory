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

// ErrNoMessage is returned by SetThread when no stored message has the id.
var ErrNoMessage = errors.New("inbox: no such message")

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
	hook    func(inboxID string, env wire.MessageEnvelope)
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

// migrate brings the schema up to date in place. Every step is additive and
// idempotent (spec §4.3): it runs on every Open, against fresh files and
// against databases written by any earlier version, and never drops,
// rewrites or deletes rows.
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
	if _, err := db.Exec(ddl); err != nil {
		return err
	}
	// v2 (inbox redesign, §9.5): nullable thread_id + (thread_id, seq)
	// index. SQLite has no ADD COLUMN IF NOT EXISTS, so probe first.
	if err := addColumnIfMissing(db, "messages", "thread_id", "TEXT NULL"); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_thread ON messages(thread_id, seq)`)
	return err
}

// addColumnIfMissing runs ALTER TABLE ... ADD COLUMN unless the column is
// already present (idempotent re-open).
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	has, err := hasColumn(db, table, column)
	if err != nil || has {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl)
	if err != nil && strings.Contains(err.Error(), "duplicate column") {
		return nil // lost a race with another opener; same end state
	}
	return err
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	return n > 0, err
}

// DB returns the underlying database handle so sibling stores that share
// the inbox file (internal/tasks) can use the same single connection. The
// handle is owned by the Service: do not Close it; Service.Close does.
func (s *Service) DB() *sql.DB { return s.db }

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
// server-assigned id and ts (spec §4.1). A non-empty env.ThreadID is
// persisted (empty stores NULL). Duplicate job.result posts for the
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
		`INSERT INTO messages (id, inbox_id, type, job_id, sender, payload, ts, thread_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		env.ID, inboxID, env.Type, env.JobID, env.From, payload, env.TS, nullString(env.ThreadID))
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
	if hook := s.hookFunc(); hook != nil {
		hook(inboxID, env)
	}
	return env, nil
}

// SetPublishHook sets fn to be invoked after every successful Post, with the
// target inbox id and the full stored envelope. fn is called synchronously
// on the posting goroutine, so it must not block Post and must be safe to
// call concurrently from multiple goroutines. A nil fn (the default) is a
// no-op. Failed and duplicate posts do not fire it. The intended consumer is
// the notify Hub's SSE fan-out (spec §4.4), whose Publish is a non-blocking
// buffered send.
func (s *Service) SetPublishHook(fn func(inboxID string, env wire.MessageEnvelope)) {
	s.mu.Lock()
	s.hook = fn
	s.mu.Unlock()
}

func (s *Service) hookFunc() func(inboxID string, env wire.MessageEnvelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hook
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
		`SELECT `+envCols+`
		 FROM messages WHERE inbox_id = ? AND acked = 0 ORDER BY seq LIMIT ?`,
		inboxID, limit)
	if err != nil {
		return nil, err
	}
	return scanEnvelopes(rows)
}

// envCols is the column list scanEnvelope expects.
const envCols = `id, type, job_id, sender, payload, ts, IFNULL(thread_id, '')`

type rowScanner interface{ Scan(dest ...any) error }

func scanEnvelope(r rowScanner) (wire.MessageEnvelope, error) {
	var env wire.MessageEnvelope
	var payload string
	if err := r.Scan(&env.ID, &env.Type, &env.JobID, &env.From, &payload, &env.TS, &env.ThreadID); err != nil {
		return env, err
	}
	env.Payload = jsonRaw(payload)
	return env, nil
}

func scanEnvelopes(rows *sql.Rows) ([]wire.MessageEnvelope, error) {
	defer rows.Close()
	var envs []wire.MessageEnvelope
	for rows.Next() {
		env, err := scanEnvelope(rows)
		if err != nil {
			return nil, err
		}
		envs = append(envs, env)
	}
	return envs, rows.Err()
}

func reverse(envs []wire.MessageEnvelope) {
	for i, j := 0, len(envs)-1; i < j; i, j = i+1, j-1 {
		envs[i], envs[j] = envs[j], envs[i]
	}
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// InboxSummary is one row of the UI inbox listing (spec §4.1): aggregate
// counts over a single inbox. Rows are retained after ack, so every inbox
// that has ever received a message appears; acked_at is included so the UI
// can show recent activity even for fully-drained inboxes.
type InboxSummary struct {
	InboxID string `json:"inbox_id"`
	Total   int    `json:"total"`
	Unacked int    `json:"unacked"`
	LastTS  string `json:"last_ts,omitempty"`
}

// Summaries returns one InboxSummary per inbox that has ever received a
// message, ordered by inbox id (spec §4.1: UI listing). An empty store
// returns a nil slice.
func (s *Service) Summaries(ctx context.Context) ([]InboxSummary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT inbox_id, COUNT(*), SUM(acked = 0), IFNULL(MAX(ts), '')
		 FROM messages GROUP BY inbox_id ORDER BY inbox_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sums []InboxSummary
	for rows.Next() {
		var sum InboxSummary
		if err := rows.Scan(&sum.InboxID, &sum.Total, &sum.Unacked, &sum.LastTS); err != nil {
			return nil, err
		}
		sums = append(sums, sum)
	}
	return sums, rows.Err()
}

// Recent returns up to limit envelopes for inboxID in chronological order,
// including acked ones (spec §4.1: UI replay path - the streaming GET
// excludes acked, so the UI needs this to show history). It reads the newest
// rows then reverses them client-side of the query.
func (s *Service) Recent(ctx context.Context, inboxID string, limit int) ([]wire.MessageEnvelope, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+envCols+`
		 FROM messages WHERE inbox_id = ? ORDER BY seq DESC LIMIT ?`,
		inboxID, limit)
	if err != nil {
		return nil, err
	}
	envs, err := scanEnvelopes(rows)
	if err != nil {
		return nil, err
	}
	reverse(envs) // chronological: the query walked newest-first
	return envs, nil
}

// ThreadMessages returns the last limit messages filed under threadID (a
// task id), oldest to newest, across every inbox and including acked rows
// (§9.5 storage). limit <= 0 returns the whole thread. An unknown or empty
// threadID yields an empty (nil) slice. Served by idx_messages_thread.
func (s *Service) ThreadMessages(ctx context.Context, threadID string, limit int) ([]wire.MessageEnvelope, error) {
	if threadID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = -1 // SQLite: negative LIMIT means unbounded
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+envCols+`
		 FROM messages WHERE thread_id = ? ORDER BY seq DESC LIMIT ?`,
		threadID, limit)
	if err != nil {
		return nil, err
	}
	envs, err := scanEnvelopes(rows)
	if err != nil {
		return nil, err
	}
	reverse(envs)
	return envs, nil
}

// SetThread files the stored message msgID under threadID (an empty
// threadID clears it back to NULL). It is a pure storage update: it does not
// change ack state and does not fire the publish hook. Returns ErrNoMessage
// when no message has that id.
func (s *Service) SetThread(ctx context.Context, msgID, threadID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET thread_id = ? WHERE id = ?`, nullString(threadID), msgID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoMessage
	}
	return nil
}

// GetMessage returns the stored envelope with the given id (spec §4.1:
// replay/UI path), including acked messages. ok is false when absent.
func (s *Service) GetMessage(ctx context.Context, mid string) (env wire.MessageEnvelope, ok bool, err error) {
	env, err = scanEnvelope(s.db.QueryRowContext(ctx,
		`SELECT `+envCols+` FROM messages WHERE id = ?`, mid))
	if errors.Is(err, sql.ErrNoRows) {
		return wire.MessageEnvelope{}, false, nil
	}
	if err != nil {
		return wire.MessageEnvelope{}, false, err
	}
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
