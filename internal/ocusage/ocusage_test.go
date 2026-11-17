package ocusage

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// open opens (creating if needed) the test database read-write (the
// test owns it).
func open(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path)
}

// openTestDB creates a sqlite database with the session_v2 columns the
// reader selects (the subset of opencode's schema that matters here)
// and returns its path.
func openTestDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	const ddl = `
CREATE TABLE session_v2 (
	id TEXT PRIMARY KEY,
	directory TEXT NOT NULL,
	model TEXT,
	cost REAL DEFAULT 0 NOT NULL,
	tokens_input INTEGER DEFAULT 0 NOT NULL,
	tokens_output INTEGER DEFAULT 0 NOT NULL,
	tokens_reasoning INTEGER DEFAULT 0 NOT NULL,
	tokens_cache_read INTEGER DEFAULT 0 NOT NULL,
	tokens_cache_write INTEGER DEFAULT 0 NOT NULL,
	time_created INTEGER NOT NULL
);`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	return path
}

func TestReadSinceFiltersAndParses(t *testing.T) {
	dbPath := openTestDB(t)
	db, err := open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	seed := []struct {
		id, dir, model string
		cost           float64
		in, created    int64
	}{
		{"ses_old", "/var/f/brain", `{"id":"glm-5.3","providerID":"ollama-cloud"}`, 0.1, 100, 1000},
		{"ses_new", "/var/f/brain", `{"id":"z-ai/glm-5.3-flash","providerID":"openrouter","variant":"max"}`, 0.39, 578107, 2000},
		{"ses_other", "/var/f/planner", "", 0.05, 50, 3000},
		{"ses_nullmodel", "/var/f/brain", "", 0.02, 10, 1500},
	}
	for _, s := range seed {
		if _, err := db.Exec(`INSERT INTO session_v2
			(id, directory, model, cost, tokens_input, time_created)
			VALUES (?, ?, ?, ?, ?, ?)`,
			s.id, s.dir, s.model, s.cost, s.in, s.created); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}
	db.Close()

	got, err := ReadSince(dbPath, "/var/f/brain", 1000)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	want := []Session{
		{ID: "ses_old", Directory: "/var/f/brain", Model: "ollama-cloud/glm-5.3", CostUSD: 0.1, InputTokens: 100, CreatedMs: 1000},
		{ID: "ses_nullmodel", Directory: "/var/f/brain", CostUSD: 0.02, InputTokens: 10, CreatedMs: 1500},
		{ID: "ses_new", Directory: "/var/f/brain", Model: "openrouter/z-ai/glm-5.3-flash#max", CostUSD: 0.39, InputTokens: 578107, CreatedMs: 2000},
	}
	if len(got) != len(want) {
		t.Fatalf("ReadSince returned %d sessions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("session[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The since filter excludes older sessions.
	got, err = ReadSince(dbPath, "/var/f/brain", 1600)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ses_new" {
		t.Errorf("ReadSince since=1600 = %+v, want only ses_new", got)
	}
}

func TestReadSinceMissingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	db, err := open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.Close()
	if _, err := ReadSince(path, "/var/f/brain", 0); err == nil {
		t.Fatal("ReadSince on a database with no session_v2 table: want error, got nil")
	}
}

func TestParseModel(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"provider-qualified", `{"id":"z-ai/glm-5.3","providerID":"openrouter"}`, "openrouter/z-ai/glm-5.3"},
		{"variant suffix", `{"id":"glm-5.3-flash","providerID":"openrouter","variant":"max"}`, "openrouter/glm-5.3-flash#max"},
		{"id only", `{"id":"qwen3:8b"}`, "qwen3:8b"},
		{"empty", "", ""},
		{"null column", "NULL", ""},
		{"not json", "not-json", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseModel(tc.raw); got != tc.want {
				t.Errorf("parseModel(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestDBPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-opencode")
	script := "#!/bin/sh\ncat <<'EOF'\n/tmp/fake/opencode.db\nEOF\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	got, err := DBPath(bin)
	if err != nil {
		t.Fatalf("DBPath: %v", err)
	}
	if got != "/tmp/fake/opencode.db" {
		t.Errorf("DBPath = %q, want /tmp/fake/opencode.db", got)
	}

	// A binary that fails is an error, never a panic.
	if _, err := DBPath(filepath.Join(dir, "missing")); err == nil {
		t.Error("DBPath on a missing binary: want error, got nil")
	}
}

func TestDBPathEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	if _, err := DBPath(bin); err == nil {
		t.Error("DBPath on empty output: want error, got nil")
	}
}

func TestSessionUsageMapping(t *testing.T) {
	s := Session{ID: "ses_x", Model: "openrouter/z-ai/glm-5.3", InputTokens: 10, OutputTokens: 4, ReasoningTokens: 6, CacheReadTokens: 30, CacheWriteTokens: 2, CostUSD: 0.125}
	u := s.Usage("fallback", 1500)
	if u.Input != 10 || u.Output != 10 || u.CacheCreate != 2 || u.CacheRead != 30 || u.CostUSD != 0.125 ||
		u.Model != "openrouter/z-ai/glm-5.3" || u.SessionID != "ses_x" || u.DurationMs != 1500 {
		t.Fatalf("usage = %+v", u)
	}
	if got := (Session{ID: "y"}).Usage("fallback", 0).Model; got != "fallback" {
		t.Fatalf("model fallback = %q", got)
	}
}

func TestReadMissingBinary(t *testing.T) {
	if _, err := ReaderFor(filepath.Join(t.TempDir(), "no-opencode"))("/x", 0); err == nil {
		t.Fatal("missing binary: want an error")
	}
}
