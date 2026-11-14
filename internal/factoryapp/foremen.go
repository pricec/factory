package factoryapp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/registry"
)

// foremanSource adapts the jobs store to registry.ForemanSource: rows come
// from Store.Foremen with full "foreman:x" identities, the capabilities
// JSON string is decoded to a map, and RunningJobs is the count of the
// foreman's jobs currently running.
type foremanSource struct{ store *jobs.Store }

var _ registry.ForemanSource = (*foremanSource)(nil)

func (fs *foremanSource) Foremen(ctx context.Context) ([]registry.Foreman, error) {
	rows, err := fs.store.Foremen(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]registry.Foreman, 0, len(rows))
	for _, row := range rows {
		f := registry.Foreman{ID: row.ID, Online: row.Online}
		if err := json.Unmarshal([]byte(row.Capabilities), &f.Capabilities); err != nil {
			return nil, fmt.Errorf("foreman %s capabilities: %w", row.ID, err)
		}
		running, err := fs.store.List(ctx, jobs.StateRunning, row.ID, notifyListLimit)
		if err != nil {
			return nil, err
		}
		f.RunningJobs = len(running)
		out = append(out, f)
	}
	return out, nil
}
