// Schedules: factory-owned cron scheduling (spec §6.8, issue #8). A single
// writer (the Scheduler goroutine) instantiates jobs from enabled schedule
// templates; the unique (schedule_id, run_at) on schedule_runs makes ticks
// idempotent. No distributed locks in v1.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/wire"
	"github.com/robfig/cron/v3"
)

// EnsureScheduleTables creates the schedules and schedule_runs tables if
// absent (spec §6.8). It is called lazily from the schedule CRUD methods
// and Scheduler.Pump because the jobs.Open migration is owned elsewhere;
// CREATE TABLE IF NOT EXISTS is idempotent and cheap under MaxOpenConns(1).
func EnsureScheduleTables(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS schedules (
	id           TEXT PRIMARY KEY,
	cron_spec    TEXT NOT NULL,
	job_template TEXT NOT NULL,
	enabled      INTEGER NOT NULL DEFAULT 1,
	last_run_at  TEXT
);
CREATE TABLE IF NOT EXISTS schedule_runs (
	schedule_id TEXT NOT NULL,
	run_at      TEXT NOT NULL,
	PRIMARY KEY (schedule_id, run_at)
);
`
	_, err := db.Exec(ddl)
	return err
}

// ErrScheduleNotFound is returned by DeleteSchedule and SetScheduleEnabled
// when no schedule has the given id; handlers map it to 404.
var ErrScheduleNotFound = errors.New("jobs: schedule not found")

// errBadSchedule marks 400-level schedule failures (unparseable cron spec,
// invalid template); handlers map it to 400 bad_request.
type errBadSchedule struct{ msg string }

func (e *errBadSchedule) Error() string { return e.msg }

func badSchedule(format string, args ...any) error {
	return &errBadSchedule{msg: fmt.Sprintf(format, args...)}
}

// Schedule is one schedules row (spec §6.8): a 5-field cron spec plus the
// job template instantiated on every fire.
type Schedule struct {
	ID          string       `json:"id"`
	CronSpec    string       `json:"cron_spec"`
	JobTemplate wire.JobSpec `json:"job_template"`
	Enabled     bool         `json:"enabled"`
	// LastRunAt is the fire-time anchor: seeded with the schedule's
	// creation time (the pinned schema has no created_at column) and
	// advanced to each fire time, so the next fire is always the first
	// cron boundary after it.
	LastRunAt string `json:"last_run_at"`
}

func newScheduleID() string { return "sched_" + ulid.Make().String() }

// CreateSchedule validates cronSpec with robfig/cron's ParseStandard and
// template with wire.ValidateTemplate (defaults applied, id/created_at
// optional), then stores the schedule enabled (spec §6.8). Errors of type
// errBadSchedule are 400-level.
func (s *Store) CreateSchedule(ctx context.Context, cronSpec string, template wire.JobSpec) (Schedule, error) {
	if _, err := cron.ParseStandard(cronSpec); err != nil {
		return Schedule{}, badSchedule("cron_spec: %v", err)
	}
	if err := wire.ValidateTemplate(&template); err != nil {
		return Schedule{}, badSchedule("job_template: %v", err)
	}
	if err := EnsureScheduleTables(s.db); err != nil {
		return Schedule{}, err
	}
	sch := Schedule{
		ID:          newScheduleID(),
		CronSpec:    cronSpec,
		JobTemplate: template,
		Enabled:     true,
		LastRunAt:   s.now(),
	}
	tmplJSON, err := json.Marshal(sch.JobTemplate)
	if err != nil {
		return Schedule{}, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO schedules (id, cron_spec, job_template, enabled, last_run_at) VALUES (?, ?, ?, 1, ?)`,
		sch.ID, sch.CronSpec, string(tmplJSON), sch.LastRunAt); err != nil {
		return Schedule{}, err
	}
	return sch, nil
}

// Schedules lists all schedules, ordered by id.
func (s *Store) Schedules(ctx context.Context) ([]Schedule, error) {
	if err := EnsureScheduleTables(s.db); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, cron_spec, job_template, enabled, IFNULL(last_run_at, '') FROM schedules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var sch Schedule
		var tmplJSON string
		var enabled int
		if err := rows.Scan(&sch.ID, &sch.CronSpec, &tmplJSON, &enabled, &sch.LastRunAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(tmplJSON), &sch.JobTemplate); err != nil {
			return nil, fmt.Errorf("schedule %s template: %w", sch.ID, err)
		}
		sch.Enabled = enabled != 0
		out = append(out, sch)
	}
	return out, rows.Err()
}

// DeleteSchedule removes the schedule and its run claims; wraps
// ErrScheduleNotFound when the id is absent.
func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	if err := EnsureScheduleTables(s.db); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("delete schedule %s: %w", id, ErrScheduleNotFound)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schedule_runs WHERE schedule_id = ?`, id); err != nil {
		return err
	}
	return nil
}

// SetScheduleEnabled toggles a schedule; wraps ErrScheduleNotFound when
// the id is absent.
func (s *Store) SetScheduleEnabled(ctx context.Context, id string, enabled bool) error {
	if err := EnsureScheduleTables(s.db); err != nil {
		return err
	}
	flag := 0
	if enabled {
		flag = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE schedules SET enabled = ? WHERE id = ?`, flag, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("set schedule %s enabled: %w", id, ErrScheduleNotFound)
	}
	return nil
}

// scheduleByID finds a schedule by id (handler responses).
func (s *Store) scheduleByID(ctx context.Context, id string) (Schedule, bool, error) {
	schedules, err := s.Schedules(ctx)
	if err != nil {
		return Schedule{}, false, err
	}
	for _, sch := range schedules {
		if sch.ID == id {
			return sch, true, nil
		}
	}
	return Schedule{}, false, nil
}

// Scheduler is the factory-owned schedule ticker (spec §6.8): a single
// writer that fires jobs from enabled schedules. Pump is one pass and the
// fake-clockable unit; all time decisions flow through store.Now, never
// time.Now.
type Scheduler struct {
	store   *Store
	matcher registry.Matcher
	// Tick is the Run-loop interval; default 1s.
	Tick time.Duration
}

// NewScheduler creates a scheduler over store dispatching via matcher.
func NewScheduler(store *Store, matcher registry.Matcher) *Scheduler {
	return &Scheduler{store: store, matcher: matcher, Tick: time.Second}
}

// Pump runs one pass and returns the number of jobs enqueued. For each
// enabled schedule it computes the next fire time after last_run_at (the
// creation anchor before the first fire); when it is due (fire <= now) it
// claims the tick in schedule_runs (INSERT OR IGNORE - zero rows affected
// means another pass already claimed it and the tick is skipped),
// instantiates the template with a fresh job id and created_at = fire time,
// creates the job, and dispatches through the Matcher. With no matching
// foreman the job stays pending, logged once. At most one tick per
// schedule drains per pass, so a backlog (e.g. after re-enabling) drains
// one tick per pass.
func (s *Scheduler) Pump(ctx context.Context) (int, error) {
	if err := EnsureScheduleTables(s.store.db); err != nil {
		return 0, err
	}
	schedules, err := s.store.Schedules(ctx)
	if err != nil {
		return 0, err
	}
	now := s.store.Now()
	enqueued := 0
	for _, sch := range schedules {
		if !sch.Enabled {
			continue
		}
		sched, err := cron.ParseStandard(sch.CronSpec)
		if err != nil {
			log.Printf("scheduler: schedule %s: bad cron %q: %v", sch.ID, sch.CronSpec, err)
			continue
		}
		anchor := now
		if sch.LastRunAt != "" {
			anchor, err = time.Parse(time.RFC3339, sch.LastRunAt)
			if err != nil {
				log.Printf("scheduler: schedule %s: bad last_run_at %q: %v", sch.ID, sch.LastRunAt, err)
				continue
			}
		}
		fire := sched.Next(anchor)
		if fire.After(now) {
			continue
		}
		runAt := fire.UTC().Format(time.RFC3339)
		res, err := s.store.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO schedule_runs (schedule_id, run_at) VALUES (?, ?)`, sch.ID, runAt)
		if err != nil {
			return enqueued, err
		}
		claimed, err := res.RowsAffected()
		if err != nil {
			return enqueued, err
		}
		if claimed == 0 {
			// Duplicate tick (a crashed pass already claimed it): no
			// second job, but still advance the anchor.
			if _, err := s.store.db.ExecContext(ctx,
				`UPDATE schedules SET last_run_at = ? WHERE id = ?`, runAt, sch.ID); err != nil {
				return enqueued, err
			}
			continue
		}
		spec := sch.JobTemplate
		spec.ID = wire.NewJobID()
		spec.CreatedAt = runAt
		if err := s.store.Create(ctx, spec); err != nil {
			return enqueued, err
		}
		enqueued++
		// A pinned template job bypasses the Matcher (spec §6.7 foreman
		// pin): it dispatches when the pinned foreman is online, else
		// pends for the pending picker. The pin rides the template, so
		// every scheduled run lands on the same foreman.
		if spec.Foreman != "" {
			pin, found, err := FindForeman(ctx, s.store, spec.Foreman)
			switch {
			case err != nil:
				log.Printf("scheduler: job %s left pending: pin lookup: %v", spec.ID, err)
			case !found:
				log.Printf("scheduler: job %s left pending: pinned %s not registered", spec.ID, spec.Foreman)
			case !pin.Online:
				log.Printf("scheduler: job %s left pending: pinned %s offline", spec.ID, spec.Foreman)
			default:
				if err := s.store.Dispatch(ctx, spec.ID, pin.ID); err != nil {
					return enqueued, err
				}
			}
		} else if s.matcher == nil {
			log.Printf("scheduler: job %s left pending: no matcher wired", spec.ID)
		} else {
			foreman, ok, err := s.matcher.Match(ctx, spec.Requires)
			switch {
			case err != nil:
				log.Printf("scheduler: job %s left pending: match: %v", spec.ID, err)
			case !ok:
				log.Printf("scheduler: job %s left pending: no foreman (requires %v)", spec.ID, spec.Requires)
			default:
				if err := s.store.Dispatch(ctx, spec.ID, foreman.ID); err != nil {
					return enqueued, err
				}
			}
		}
		if _, err := s.store.db.ExecContext(ctx,
			`UPDATE schedules SET last_run_at = ? WHERE id = ?`, runAt, sch.ID); err != nil {
			return enqueued, err
		}
	}
	return enqueued, nil
}

// Run pumps every Tick until ctx is done. The ticker only paces the loop;
// Pump is the fake-clockable unit.
func (s *Scheduler) Run(ctx context.Context) error {
	tick := s.Tick
	if tick <= 0 {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if _, err := s.Pump(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// SchedulesHandler exposes the schedules control-plane API (spec §6.9):
// POST /v1/schedules {cron_spec, job_template}, GET /v1/schedules,
// DELETE /v1/schedules/{id}, POST /v1/schedules/{id}/enable, and
// POST /v1/schedules/{id}/disable. It is mounted alongside APIHandler by
// the integrator; errors follow the §10 model via writeError.
func (s *Store) SchedulesHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/schedules", s.handleCreateSchedule)
	mux.HandleFunc("GET /v1/schedules", s.handleListSchedules)
	mux.HandleFunc("DELETE /v1/schedules/{id}", s.handleDeleteSchedule)
	mux.HandleFunc("POST /v1/schedules/{id}/enable", s.handleSetScheduleEnabled(true))
	mux.HandleFunc("POST /v1/schedules/{id}/disable", s.handleSetScheduleEnabled(false))
	return mux
}

func (s *Store) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CronSpec    string       `json:"cron_spec"`
		JobTemplate wire.JobSpec `json:"job_template"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "malformed body: "+err.Error(), false)
		return
	}
	sch, err := s.CreateSchedule(r.Context(), body.CronSpec, body.JobTemplate)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sch)
}

func (s *Store) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	schedules, err := s.Schedules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, schedules)
}

func (s *Store) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.DeleteSchedule(r.Context(), r.PathValue("id")); err != nil {
		writeScheduleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleSetScheduleEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := s.SetScheduleEnabled(r.Context(), id, enabled); err != nil {
			writeScheduleError(w, err)
			return
		}
		sch, ok, err := s.scheduleByID(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no such schedule", false)
			return
		}
		writeJSON(w, http.StatusOK, sch)
	}
}

// writeScheduleError maps store errors to the §10 model: errBadSchedule to
// 400, ErrScheduleNotFound to 404, anything else to 500.
func writeScheduleError(w http.ResponseWriter, err error) {
	var bad *errBadSchedule
	if errors.As(err, &bad) {
		writeError(w, http.StatusBadRequest, "bad_request", bad.Error(), false)
		return
	}
	if errors.Is(err, ErrScheduleNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such schedule", false)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
}
