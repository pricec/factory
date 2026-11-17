// Package ocusage reads per-session token accounting from the host's
// opencode database: the usage capture for opencode-runner children
// (GLM workers, a GLM foreman brain, a non-claude planner). Claude runs
// report usage on stdout (internal/claudecli); opencode prints none, so
// its runs read the session rows opencode maintains after the child
// exits - provider-reported cost plus the full token breakdown - and feed
// them into the same usage_events pipeline as claude runs
// (wire.UsageEvent via job.usage / usage.record / in-process recording).
//
// Salvaged from the 2026-11-16 session.usage ledger snapshot; only the
// opencode reader was ported - the parallel sessions table it fed was
// dropped in favour of usage_events.
//
// The reader is read-only and defensive: the session_v2 schema is
// opencode's, not this repo's, so a missing table or column is an error
// the caller logs and degrades to zero - accounting must never kill a
// run. Nothing here writes to the database.
package ocusage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/pricec/factory/internal/claudecli"
	_ "modernc.org/sqlite"
)

// modelInfo is opencode's session_v2 `model` column: a JSON object with
// provider-scoped ids, e.g.
// {"id":"z-ai/glm-5.3-flash","providerID":"openrouter","variant":"max"}.
// NULL (some rows) or empty degrades to "".
type modelInfo struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant"`
}

// Session is one recorded opencode session: the provider-reported cost
// plus the token breakdown.
type Session struct {
	// ID is the opencode session id (ULID); the usage event's session_id
	// (the usage_events dedupe key).
	ID        string `json:"session_id"`
	Directory string `json:"directory"`
	// Model is the provider-qualified model string in the opencode `-m`
	// flag form ("openrouter/z-ai/glm-5.3-flash", "#max" appended when a
	// reasoning-effort variant was pinned); "" when opencode recorded
	// none.
	Model string `json:"model"`

	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// CreatedMs is the session's time_created: milliseconds since the
	// Unix epoch (opencode stores ms).
	CreatedMs int64 `json:"created_ms"`
}

// parseModel renders the modelInfo JSON blob as the -m flag form.
// Unparseable input yields "".
func parseModel(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var m modelInfo
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m.ID == "" {
		return ""
	}
	out := m.ID
	if m.ProviderID != "" {
		out = m.ProviderID + "/" + m.ID
	}
	if m.Variant != "" {
		out += "#" + m.Variant
	}
	return out
}

// UsageReadTimeout bounds the session-accounting read's database
// discovery exec: `opencode debug paths db` normally returns in
// milliseconds, but the accounting path must never hang a host (the
// read runs after the child exits; a stuck binary costs at most this).
const UsageReadTimeout = 10 * time.Second

// DBPath discovers the host's opencode database path via
// `opencode debug paths db` (the pinned tool-facing path command; the
// path respects OPENCODE_DB and release channels, so it is discovered,
// never assumed). bin is the opencode binary; "" means "opencode" on
// PATH.
func DBPath(bin string) (string, error) {
	return DBPathCtx(context.Background(), bin)
}

// DBPathCtx is DBPath under a caller-supplied context (the accounting
// callers bound discovery with UsageReadTimeout).
func DBPathCtx(ctx context.Context, bin string) (string, error) {
	if bin == "" {
		bin = "opencode"
	}
	out, err := exec.CommandContext(ctx, bin, "debug", "paths", "db").Output()
	if err != nil {
		return "", fmt.Errorf("ocusage: %s debug paths db: %w", bin, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("ocusage: %s debug paths db: empty output", bin)
	}
	return path, nil
}

// ReadSince returns the sessions opencode recorded for directory dir
// with time_created >= sinceMs (ms since epoch - the caller passes the
// child's spawn time, so only the just-finished run matches). The query
// is read-only. Errors (unreadable database, missing session_v2 table)
// are returned for the caller to log; recording degrades to zero
// sessions, never blocks the run.
func ReadSince(dbPath, dir string, sinceMs int64) ([]Session, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("ocusage: open %s: %w", dbPath, err)
	}
	defer db.Close()
	rows, err := db.Query(`
		SELECT id, IFNULL(model, ''), cost, tokens_input, tokens_output,
		       tokens_reasoning, tokens_cache_read, tokens_cache_write,
		       time_created
		FROM session_v2
		WHERE directory = ? AND time_created >= ?
		ORDER BY time_created`, dir, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("ocusage: read sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var model string
		if err := rows.Scan(&s.ID, &model, &s.CostUSD, &s.InputTokens,
			&s.OutputTokens, &s.ReasoningTokens, &s.CacheReadTokens,
			&s.CacheWriteTokens, &s.CreatedMs); err != nil {
			return nil, fmt.Errorf("ocusage: scan session: %w", err)
		}
		s.Directory = dir
		s.Model = parseModel(model)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Reader reads the sessions opencode recorded for dir since sinceMs: the
// signature of Read and of the callers' test seams.
type Reader func(dir string, sinceMs int64) ([]Session, error)

// Read discovers the database via bin (see DBPathCtx; "" = "opencode" on
// PATH) under UsageReadTimeout, then reads the sessions for dir created
// at or after sinceMs (ReadSince).
func Read(bin, dir string, sinceMs int64) ([]Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), UsageReadTimeout)
	defer cancel()
	dbPath, err := DBPathCtx(ctx, bin)
	if err != nil {
		return nil, err
	}
	return ReadSince(dbPath, dir, sinceMs)
}

// ReaderFor returns Read bound to bin.
func ReaderFor(bin string) Reader {
	return func(dir string, sinceMs int64) ([]Session, error) { return Read(bin, dir, sinceMs) }
}

// Usage maps the session onto the claude-side usage shape every capture
// path records (claudecli.Usage -> wire.UsageEvent). Reasoning tokens
// fold into Output: providers bill them as output, and the claude CLI's
// output_tokens already include thinking, so usage_events keeps one
// output dimension across runners. Cache writes are CacheCreate. The
// model falls back to fallbackModel when opencode recorded none.
func (s Session) Usage(fallbackModel string, durationMs int64) claudecli.Usage {
	model := s.Model
	if model == "" {
		model = fallbackModel
	}
	return claudecli.Usage{
		Input:       int(s.InputTokens),
		Output:      int(s.OutputTokens + s.ReasoningTokens),
		CacheCreate: int(s.CacheWriteTokens),
		CacheRead:   int(s.CacheReadTokens),
		CostUSD:     s.CostUSD,
		Model:       model,
		SessionID:   s.ID,
		DurationMs:  durationMs,
	}
}
