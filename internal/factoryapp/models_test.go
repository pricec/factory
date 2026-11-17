package factoryapp_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/models"
)

// fakeModelsBin writes a shell-script "opencode" whose `models` subcommand
// prints a fixed listing and appends one line to a counter file, so tests
// can assert how many execs actually ran.
func fakeModelsBin(t *testing.T, listing string) (bin, counter string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "opencode")
	counter = filepath.Join(dir, "calls")
	script := "#!/bin/sh\ncat >> " + counter + " <<'EOF'\nx\nEOF\nprintf '" + listing + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, counter
}

func callCount(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// TestModelsEndpoint: GET /v1/models lists every model the host's
// opencode reports, provider-split and sorted (spec §6.9); the listing is
// unauthenticated, cached for the pinned TTL, and re-executed after the
// TTL rolls over. No test invokes a real opencode - the fake-binary
// pattern of test/e2e.
func TestModelsEndpoint(t *testing.T) {
	e := newEnv(t)
	bin, counter := fakeModelsBin(t, "openrouter/z-ai/glm-5.3\\nollama/qwen3:8b\\nollama/gemma3:4b\\n")
	e.a.SetModelsBin(bin)

	var got []models.ModelInfo
	if status := e.do("GET", "/v1/models", "", nil, &got); status != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, want 200", status)
	}
	want := []models.ModelInfo{
		{ID: "ollama/gemma3:4b", Provider: "ollama", Model: "gemma3:4b"},
		{ID: "ollama/qwen3:8b", Provider: "ollama", Model: "qwen3:8b"},
		{ID: "openrouter/z-ai/glm-5.3", Provider: "openrouter", Model: "z-ai/glm-5.3"},
	}
	if len(got) != len(want) {
		t.Fatalf("models = %+v, want %+v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if n := callCount(t, counter); n != 1 {
		t.Fatalf("after first fetch: exec count = %d, want 1", n)
	}

	// Within the TTL the cache serves every refetch: no new exec.
	for i := 0; i < 3; i++ {
		if status := e.do("GET", "/v1/models", "", nil, &got); status != http.StatusOK {
			t.Fatalf("refetch %d status = %d, want 200", i, status)
		}
	}
	if n := callCount(t, counter); n != 1 {
		t.Fatalf("after refetches: exec count = %d, want 1 (cached)", n)
	}

	// Past the TTL the listing re-executes (fake clock: advance 5 min).
	e.clock.Advance(6 * time.Minute)
	if status := e.do("GET", "/v1/models", "", nil, &got); status != http.StatusOK {
		t.Fatalf("post-TTL fetch status = %d, want 200", status)
	}
	if n := callCount(t, counter); n != 2 {
		t.Fatalf("after TTL: exec count = %d, want 2", n)
	}
}

// TestModelsEndpointEmptyAndError: an empty listing is [] (never null -
// the UI renders it directly) and is NOT cached - empty is opencode's
// degraded mode when its background service is unreachable, so the next
// request retries instead of pinning a broken picker for a TTL. A broken
// opencode surfaces the normative error model (500), not a hang or a
// panic.
func TestModelsEndpointEmptyAndError(t *testing.T) {
	e := newEnv(t)

	// Empty listing -> [] twice; the second fetch re-execs (no cache).
	bin, counter := fakeModelsBin(t, "")
	e.a.SetModelsBin(bin)
	var got []models.ModelInfo
	for i := 0; i < 2; i++ {
		if status := e.do("GET", "/v1/models", "", nil, &got); status != http.StatusOK {
			t.Fatalf("empty listing %d: status = %d, want 200", i, status)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("empty listing %d = %+v, want []", i, got)
		}
	}
	if n := callCount(t, counter); n != 2 {
		t.Fatalf("empty listing: exec count = %d, want 2 (empty results are not cached)", n)
	}

	// Broken binary -> 500 + error model.
	e.a.SetModelsBin(filepath.Join(t.TempDir(), "no-such-binary"))
	var errResp map[string]any
	if status := e.do("GET", "/v1/models", "", nil, &errResp); status != http.StatusInternalServerError {
		t.Fatalf("broken binary status = %d, want 500", status)
	}
	if errResp["error"] == nil {
		t.Fatalf("error body = %+v, want the normative error model", errResp)
	}
}
