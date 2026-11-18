package jobs

import (
	"context"
	"time"

	"github.com/pricec/factory/internal/notify"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/wire"
)

// Liveness implements notify.Observer (spec §6.3): activity and connects
// bump last_seen and mark the foreman online; disconnects mark offline.
// identity is the foreman's inbox id ("foreman:alpha").
type Liveness struct {
	store *Store
}

// NewLiveness wires liveness updates into store.
func NewLiveness(store *Store) *Liveness { return &Liveness{store: store} }

var _ notify.Observer = (*Liveness)(nil)

// OnActivity records an authenticated request from identity at ts.
func (l *Liveness) OnActivity(identity string, ts time.Time) {
	l.touch(identity, ts)
}

// OnConnect records a stream connection from identity.
func (l *Liveness) OnConnect(identity string) {
	l.touch(identity, l.store.Now())
}

// OnDisconnect marks identity offline.
func (l *Liveness) OnDisconnect(identity string) {
	_, _ = l.store.db.Exec(
		`UPDATE foremen SET online = 0 WHERE id = ?`, identity)
}

func (l *Liveness) touch(identity string, ts time.Time) {
	_, _ = l.store.db.Exec(
		`INSERT INTO foremen (id, capabilities, last_seen, online) VALUES (?, '{}', ?, 1)
		 ON CONFLICT(id) DO UPDATE SET last_seen = excluded.last_seen, online = 1`,
		identity, ts.UTC().Format(time.RFC3339))
}

// Foreman is a registry row (spec §6.9).
type Foreman struct {
	ID           string `json:"id"`
	Online       bool   `json:"online"`
	Capabilities string `json:"capabilities"`
	LastSeen     string `json:"last_seen,omitempty"`
	// Host is the stored host-facts JSON (spec §6.7.1); "" for foremen
	// that never reported one.
	Host string `json:"host,omitempty"`
}

// Foremen lists the registry (spec §6.9).
func (s *Store) Foremen(ctx context.Context) ([]Foreman, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, online, capabilities, IFNULL(last_seen, ''), IFNULL(host, '') FROM foremen ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Foreman
	for rows.Next() {
		var f Foreman
		var online int
		if err := rows.Scan(&f.ID, &online, &f.Capabilities, &f.LastSeen, &f.Host); err != nil {
			return nil, err
		}
		f.Online = online != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

// FindForeman resolves a bare or foreman:-prefixed foreman reference
// against the registry snapshot (spec §6.7 foreman pin): the returned
// row is the registered foreman ref names; ok is false when ref names
// no registered foreman, so the create paths reject it and the pickers
// keep the job pending. found-but-offline is the caller's dispatch
// decision: the pin never falls back to the Matcher.
func FindForeman(ctx context.Context, s *Store, ref string) (f Foreman, ok bool, err error) {
	foremen, err := s.Foremen(ctx)
	if err != nil {
		return Foreman{}, false, err
	}
	for _, row := range foremen {
		if registry.SameForeman(row.ID, ref) {
			return row, true, nil
		}
	}
	return Foreman{}, false, nil
}

// Sweeper marks timed-out jobs and offline foremen (spec §6.2, §6.3). Pump
// is one pass, for tests and the ticker loop alike.
type Sweeper struct {
	store *Store
	// Tick is the loop interval; default 10s.
	Tick time.Duration
}

// NewSweeper creates a sweeper over store.
func NewSweeper(store *Store) *Sweeper {
	return &Sweeper{store: store, Tick: 10 * time.Second}
}

// Pump runs one sweep pass:
//  1. running jobs past timeout_s + 60s grace -> timed_out (terminal hook
//     fires). timeout_s is the hard CEILING (default 4h): the worker
//     harness enforces idle_timeout_s and the spend cap itself and always
//     posts its result by the ceiling, so the sweeper's deadline never
//     precedes a job the harness still considers alive;
//  2. foremen online with last_seen 90s or more old -> offline (the 90s
//     threshold covers the 60s capabilities push cadence; spec §6.3);
//  3. running jobs on a foreman offline for 10 minutes -> reaped (failed,
//     retry armed, or dead; spec §6.2);
//  4. failed jobs with no retry armed for 2 minutes -> retry armed or dead.
func (s *Sweeper) Pump(ctx context.Context) error {
	now := s.store.Now()

	rows, err := s.store.db.QueryContext(ctx,
		`SELECT id, spec, updated_at FROM jobs WHERE state = ?`, StateRunning)
	if err != nil {
		return err
	}
	type candidate struct {
		id       string
		deadline time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var id, specJSON, updatedAt string
		if err := rows.Scan(&id, &specJSON, &updatedAt); err != nil {
			rows.Close()
			return err
		}
		var spec struct {
			TimeoutS int `json:"timeout_s"`
		}
		if err := unmarshalJSON([]byte(specJSON), &spec); err != nil {
			continue // unreachable: specs are validated on Create
		}
		ceiling := spec.TimeoutS
		if ceiling <= 0 {
			ceiling = wire.DefaultTimeoutS
		}
		started, err := time.Parse(time.RFC3339, updatedAt)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{
			id:       id,
			deadline: started.Add(time.Duration(ceiling)*time.Second + timeoutGrace),
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, c := range candidates {
		if now.After(c.deadline) {
			if _, applied, err := s.store.transition(ctx, c.id, StateTimedOut); err != nil {
				return err
			} else if applied {
				s.store.fireTerminal(c.id, StateTimedOut)
			}
		}
	}

	_, err = s.store.db.ExecContext(ctx,
		`UPDATE foremen SET online = 0 WHERE online = 1 AND last_seen <= ?`,
		now.Add(-offlineAfter).UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	if err := s.store.reapOfflineForemen(ctx); err != nil {
		return err
	}
	return s.store.settleStrandedFailed(ctx)
}

// Run pumps every Tick until ctx is done.
func (s *Sweeper) Run(ctx context.Context) error {
	tick := s.Tick
	if tick <= 0 {
		tick = 10 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if err := s.Pump(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
