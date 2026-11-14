// Package registry implements foreman capabilities and deterministic job
// matching (spec §6.7): Match selects the online foreman whose capability
// tags are a superset of the job's requires list.
package registry

import (
	"cmp"
	"context"
	"slices"
)

// Foreman is a registry snapshot row for matching.
type Foreman struct {
	ID           string
	Online       bool
	RunningJobs  int
	Capabilities map[string]string
}

// ForemanSource supplies live registry rows; implemented by the jobs store
// in integration wiring.
type ForemanSource interface {
	Foremen(ctx context.Context) ([]Foreman, error)
}

// Matcher selects the foreman to receive a job with the given capability
// requirements (spec §6.7). ok is false when no foreman qualifies.
type Matcher interface {
	Match(ctx context.Context, requires []string) (foreman Foreman, ok bool, err error)
}

// CapabilityMatcher is the real matcher (spec §6.7): online foremen whose
// capability tags are a superset of requires, AND semantics, no negation;
// deterministic pick by (running_jobs asc, id asc).
type CapabilityMatcher struct{ Source ForemanSource }

// Match implements Matcher: it filters to online foremen satisfying every
// required tag, then picks the deterministic winner by (running_jobs asc,
// id asc). No qualifying foreman yields (zero, false, nil).
func (m *CapabilityMatcher) Match(ctx context.Context, requires []string) (Foreman, bool, error) {
	foremen, err := m.Source.Foremen(ctx)
	if err != nil {
		return Foreman{}, false, err
	}
	var candidates []Foreman
	for _, f := range foremen {
		if !f.Online {
			continue
		}
		if !Capabilities(f.Capabilities).HasAll(requires) {
			continue
		}
		candidates = append(candidates, f)
	}
	if len(candidates) == 0 {
		return Foreman{}, false, nil
	}
	slices.SortFunc(candidates, func(a, b Foreman) int {
		return cmp.Or(
			cmp.Compare(a.RunningJobs, b.RunningJobs),
			cmp.Compare(a.ID, b.ID),
		)
	})
	return candidates[0], true, nil
}

var _ Matcher = (*CapabilityMatcher)(nil)
