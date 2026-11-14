// Package registry implements foreman capabilities and deterministic job
// matching (spec §6.7): Match selects the online foreman whose capability
// tags are a superset of the job's requires list.
package registry

import "context"

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

// FirstOnline is the v1 stub matcher (#8): the first online foreman wins,
// ignoring requires. #12 replaces it with the capability matcher and this
// stub is deleted.
type FirstOnline struct {
	Source ForemanSource
}

// Match implements Matcher.
func (m *FirstOnline) Match(ctx context.Context, requires []string) (Foreman, bool, error) {
	foremen, err := m.Source.Foremen(ctx)
	if err != nil {
		return Foreman{}, false, err
	}
	for _, f := range foremen {
		if f.Online {
			return f, true, nil
		}
	}
	return Foreman{}, false, nil
}

var _ Matcher = (*FirstOnline)(nil)
