// Pending picker (spec §6.7): jobs created while no online foreman matched
// stay pending - the create paths (MCP create_job, POST /v1/jobs, schedule
// instantiation) match exactly once, at creation. This loop closes the gap:
// every pass re-runs the Matcher over the pending set and dispatches each
// job as soon as a foreman qualifies, so a foreman coming online picks up
// the backlog instead of leaving it pending forever.
package jobs

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/wire"
)

// pendingMatchLimit bounds one picker pass (matches the notifier's list
// limit); a larger backlog is drained over successive passes.
const pendingMatchLimit = 1000

// PendingPicker re-dispatches pending jobs once a foreman qualifies (spec
// §6.7). The dispatch itself is the plain pending -> dispatched transition
// (spec §6.1); envelope delivery to the foreman inbox is the dispatch
// notifier's job (at-least-once), same as every other dispatch path.
type PendingPicker struct {
	store   *Store
	matcher registry.Matcher
	// Tick is the loop interval; default 10s.
	Tick time.Duration
	// Hold, when set and true, skips the whole pass: no dispatching while
	// the usage-limit pause (token throttle S5) is active.
	Hold func(ctx context.Context) bool
	// Exclude, when set, reports that foremanID must not take spec now
	// (spec §13.3: its Claude CLI is logged out and the job would run
	// claude there). A pinned job whose pin is excluded stays pending; a
	// matched job is matched among the remaining foremen.
	Exclude func(ctx context.Context, spec wire.JobSpec, foremanID string) bool
}

// NewPendingPicker creates a picker over store dispatching via matcher.
func NewPendingPicker(store *Store, matcher registry.Matcher) *PendingPicker {
	return &PendingPicker{store: store, matcher: matcher, Tick: 10 * time.Second}
}

// Pump runs one match pass: every pending job (oldest first) whose requires
// list matches an online foreman is dispatched to the Matcher's pick; the
// rest stay pending for the next pass. Returns how many were dispatched.
func (p *PendingPicker) Pump(ctx context.Context) (int, error) {
	if p.Hold != nil && p.Hold(ctx) {
		return 0, nil
	}
	rows, err := p.store.db.QueryContext(ctx,
		`SELECT id, spec FROM jobs WHERE state = ? ORDER BY created_at ASC LIMIT ?`,
		StatePending, pendingMatchLimit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id   string
		spec wire.JobSpec
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var specJSON string
		if err := rows.Scan(&c.id, &specJSON); err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal([]byte(specJSON), &c.spec); err != nil {
			continue // unreachable: specs are validated on Create
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	dispatched := 0
	for _, c := range candidates {
		// A pinned job never goes through the Matcher (spec §6.7
		// foreman pin): it dispatches when its pinned foreman is
		// online and pends otherwise - the pin is the operator's
		// routing decision, so no capability fallback can move the
		// job to a different foreman.
		if c.spec.Foreman != "" {
			pin, found, err := FindForeman(ctx, p.store, c.spec.Foreman)
			if err != nil {
				return dispatched, err
			}
			if !found || !pin.Online {
				continue
			}
			if p.Exclude != nil && p.Exclude(ctx, c.spec, pin.ID) {
				continue
			}
			if err := p.store.Dispatch(ctx, c.id, pin.ID); err != nil {
				return dispatched, err
			}
			log.Printf("jobs: dispatched pending %s to pinned %s", c.id, pin.ID)
			dispatched++
			continue
		}
		var f registry.Foreman
		var ok bool
		if em, isEM := p.matcher.(registry.ExcludingMatcher); isEM && p.Exclude != nil {
			spec := c.spec
			f, ok, err = em.MatchExcept(ctx, spec.Requires, func(f registry.Foreman) bool { return p.Exclude(ctx, spec, f.ID) })
		} else {
			f, ok, err = p.matcher.Match(ctx, c.spec.Requires)
		}
		if err != nil {
			return dispatched, err
		}
		if !ok {
			continue
		}
		if err := p.store.Dispatch(ctx, c.id, f.ID); err != nil {
			return dispatched, err
		}
		log.Printf("jobs: dispatched pending %s to %s", c.id, f.ID)
		dispatched++
	}
	return dispatched, nil
}

// Run pumps every Tick until ctx is done.
func (p *PendingPicker) Run(ctx context.Context) error {
	tick := p.Tick
	if tick <= 0 {
		tick = 10 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if _, err := p.Pump(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
