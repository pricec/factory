package jobs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pricec/factory/internal/registry"
)

// ValidateRequires checks a job's requires entries against the foremen
// registry (spec §6.7, 2026-11-15 amendment): entries must be tag names -
// bare or "name:value" - validated against the union of registered
// foremen tag keys, never capability values, which no tag key can ever
// match and which park jobs pending silently. An error (registry.
// UnknownRequiresError) rejects creation; warnings are loud but
// non-fatal (e.g. no foremen registered yet).
func (s *Store) ValidateRequires(ctx context.Context, requires []string) ([]string, error) {
	rows, err := s.Foremen(ctx)
	if err != nil {
		return nil, err
	}
	foremen := make([]registry.Foreman, 0, len(rows))
	for _, row := range rows {
		f := registry.Foreman{ID: row.ID, Online: row.Online}
		if err := json.Unmarshal([]byte(row.Capabilities), &f.Capabilities); err != nil {
			return nil, fmt.Errorf("foreman %s capabilities: %w", row.ID, err)
		}
		foremen = append(foremen, f)
	}
	return registry.ValidateRequires(requires, foremen)
}
