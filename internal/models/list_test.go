package models_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/models"
)

// TestParseModels: `opencode models` output parses into provider-split,
// deduped, (provider, model)-sorted ModelInfo rows; junk lines are
// skipped, never fatal (spec §6.9 GET /v1/models).
func TestParseModels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []models.ModelInfo
	}{
		{
			name: "empty input yields no models",
			in:   "",
			want: nil,
		},
		{
			name: "one model per line, provider split",
			in:   "ollama/qwen3:8b\n",
			want: []models.ModelInfo{{ID: "ollama/qwen3:8b", Provider: "ollama", Model: "qwen3:8b"}},
		},
		{
			name: "sorted by provider then model",
			in:   "openrouter/z-ai/glm-5.3\nollama/qwen3:8b\nollama/gemma3:4b\nopenrouter/anthropic/claude-haiku-4.5\n",
			want: []models.ModelInfo{
				{ID: "ollama/gemma3:4b", Provider: "ollama", Model: "gemma3:4b"},
				{ID: "ollama/qwen3:8b", Provider: "ollama", Model: "qwen3:8b"},
				{ID: "openrouter/anthropic/claude-haiku-4.5", Provider: "openrouter", Model: "anthropic/claude-haiku-4.5"},
				{ID: "openrouter/z-ai/glm-5.3", Provider: "openrouter", Model: "z-ai/glm-5.3"},
			},
		},
		{
			name: "duplicate ids collapse",
			in:   "ollama/qwen3:8b\nollama/qwen3:8b\n",
			want: []models.ModelInfo{{ID: "ollama/qwen3:8b", Provider: "ollama", Model: "qwen3:8b"}},
		},
		{
			name: "blank lines and whitespace are skipped",
			in:   "\n  \nollama/qwen3:8b\n",
			want: []models.ModelInfo{{ID: "ollama/qwen3:8b", Provider: "ollama", Model: "qwen3:8b"}},
		},
		{
			name: "lines without a provider split are skipped",
			in:   "qwen3:8b\n/\n/provider\nprovider/\nollama/qwen3:8b\n",
			want: []models.ModelInfo{{ID: "ollama/qwen3:8b", Provider: "ollama", Model: "qwen3:8b"}},
		},
		{
			name: "variant and multi-segment ids keep their full form",
			in:   "openrouter/z-ai/glm-5.3-flash#max\nopenrouter/~z-ai/glm-flash-latest\n",
			want: []models.ModelInfo{
				{ID: "openrouter/z-ai/glm-5.3-flash#max", Provider: "openrouter", Model: "z-ai/glm-5.3-flash#max"},
				{ID: "openrouter/~z-ai/glm-flash-latest", Provider: "openrouter", Model: "~z-ai/glm-flash-latest"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := models.ParseModels([]byte(tc.in))
			if len(got) != len(tc.want) {
				t.Fatalf("ParseModels = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("row %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestListRunsOpencodeModels: List shells out to `<bin> models` and
// returns the parsed listing; a missing binary is an error, never a panic
// (no test ever invokes a real opencode - the fake-binary pattern of
// test/e2e).
func TestListRunsOpencodeModels(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\nprintf 'ollama/qwen3:8b\\nopenrouter/z-ai/glm-5.3\\n'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := models.List(context.Background(), bin)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []models.ModelInfo{
		{ID: "ollama/qwen3:8b", Provider: "ollama", Model: "qwen3:8b"},
		{ID: "openrouter/z-ai/glm-5.3", Provider: "openrouter", Model: "z-ai/glm-5.3"},
	}
	if len(got) != len(want) {
		t.Fatalf("List = %+v, want %+v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Failing binary: the error surfaces the exit status and stderr.
	badBin := filepath.Join(dir, "fail")
	if err := os.WriteFile(badBin, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := models.List(context.Background(), badBin); err == nil {
		t.Fatal("List with failing binary: want error, got nil")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry stderr, got %q", err)
	}

	if _, err := models.List(context.Background(), filepath.Join(dir, "nope")); err == nil {
		t.Fatal("List with missing binary: want error, got nil")
	}
}
