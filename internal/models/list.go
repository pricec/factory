package models

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// listTimeout bounds one `opencode models` listing: the exec talks to the
// opencode background service, which must never hang an HTTP request.
const listTimeout = 10 * time.Second

// ModelInfo is one selectable model (spec §6.9 GET /v1/models): an
// opencode model id ("provider/model") split into its parts so the UI can
// group by provider. IDs are the exact strings `opencode run -m` accepts,
// e.g. "openrouter/z-ai/glm-5.3" or "ollama/qwen3:8b".
type ModelInfo struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// List runs `opencode models` on the factory host and returns the parsed
// listing (spec §6.9): every model id the host's opencode install reports,
// provider-split and sorted. bin "" means "opencode" on PATH; the seam
// lets tests point at a fake binary instead of shelling out to a real
// opencode.
func List(ctx context.Context, bin string) ([]ModelInfo, error) {
	if bin == "" {
		bin = "opencode"
	}
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "models")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%s models: %v: %s", bin, err, snippet(msg))
		}
		return nil, fmt.Errorf("%s models: %w", bin, err)
	}
	return ParseModels(out), nil
}

// ParseModels parses `opencode models` output: one "provider/model" id
// per line. Blank lines and lines without a provider split are skipped;
// duplicates collapse; the result is sorted by (provider, model) so the
// endpoint output is deterministic. Empty input yields nil.
func ParseModels(data []byte) []ModelInfo {
	seen := map[string]bool{}
	var out []ModelInfo
	for _, line := range strings.Split(string(data), "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		provider, model, ok := strings.Cut(id, "/")
		if !ok || provider == "" || model == "" {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ModelInfo{ID: id, Provider: provider, Model: model})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// snippet returns the first 200 chars of s for error messages.
func snippet(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}
