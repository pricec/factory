// Token throttle S3 (dispatch gate): wires internal/throttle into the
// factory. Every create path (MCP create_job, POST /v1/jobs, schedules)
// consults the gate through jobs.Store.Admit; a hold parks the job in the
// persisted `held` state, and releaserLoop (the pendingAlertLoop pattern)
// moves held jobs to pending in priority-then-FIFO order once the usage
// state allows. Operator rules: no model/tier changes, task-priority-0 work
// always passes, chat wakes are never gated (S3 gates no wakes at all),
// running jobs are never touched, stale/missing usage fails OPEN.
package factoryapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/throttle"
	"github.com/pricec/factory/internal/usagewin"
	"github.com/pricec/factory/internal/wire"
)

const (
	// releaseTick is the releaser's cadence (the pending picker's 10s
	// order of magnitude; YELLOW spacing is 60s, so finer is pointless).
	releaseTick = 15 * time.Second
	// releaseLimit bounds one releaser pass's held scan.
	releaseLimit = 1000
	// throttleOverrideKey is the settings key persisting POST /v1/throttle.
	throttleOverrideKey = "throttle_override"
	// Alert priorities: RED pages (P1 never expires), ORANGE is P2 (24h).
	alertPriorityThrottleRed    = 1
	alertPriorityThrottleOrange = 2
)

// ThrottleConfig configures the S3 dispatch gate.
type ThrottleConfig struct {
	// Off is the FACTORY_THROTTLE=off kill switch: the gate admits all.
	Off bool
	// Hysteresis overrides the 10 min level-drop delay (tests).
	Hysteresis time.Duration
}

// LoadThrottleConfig reads FACTORY_THROTTLE ("off"/"0"/"false" disables).
func LoadThrottleConfig() ThrottleConfig {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FACTORY_THROTTLE"))) {
	case "off", "0", "false", "no":
		return ThrottleConfig{Off: true}
	}
	return ThrottleConfig{}
}

// settingsOverrides persists the operator override in the tasks settings
// table (inbox.db), so an `off` override survives a restart until it
// expires.
type settingsOverrides struct{ s *tasks.Store }

func (o settingsOverrides) LoadOverride(ctx context.Context) (string, bool, error) {
	return o.s.GetSetting(ctx, throttleOverrideKey)
}

func (o settingsOverrides) SaveOverride(ctx context.Context, raw string) error {
	return o.s.SetSetting(ctx, throttleOverrideKey, raw)
}

func (a *App) initThrottle(cfg Config) {
	if cfg.Throttle.Off {
		log.Printf("factoryapp: FACTORY_THROTTLE=off: dispatch gate disabled (admits everything)")
	}
	a.gate = throttle.New(throttle.Config{
		Snapshot:   a.usage.Snapshot,
		Now:        cfg.Now,
		EnvOff:     cfg.Throttle.Off,
		Hysteresis: cfg.Throttle.Hysteresis,
		Overrides:  settingsOverrides{a.tasks},
		Pauses:     settingsOverrides{a.tasks},
		OnPause: func(ctx context.Context, title, body string) {
			if len(title) > maxAlertTitle {
				title = title[:maxAlertTitle]
			}
			a.raiseSystemAlert(ctx, alertPriorityPause, title, body, "factory:throttle")
		},
		OnResume: func(ctx context.Context, title, body string) {
			if len(title) > maxAlertTitle {
				title = title[:maxAlertTitle]
			}
			a.raiseSystemAlert(ctx, alertPriorityResume, title, body, "factory:throttle")
		},
		OnAlert: func(ctx context.Context, level usagewin.State, title, body string) {
			p := alertPriorityThrottleOrange
			if level == usagewin.Red {
				p = alertPriorityThrottleRed
			}
			if len(title) > maxAlertTitle {
				title = title[:maxAlertTitle]
			}
			a.raiseSystemAlert(ctx, p, title, body, "factory:throttle")
		},
	})
	a.jobs.SetAdmission(a.admitNew)
	// No dispatch of already-admitted pending jobs or due retries while the
	// usage-limit pause is active (override off dispatches as usual).
	hold := func(ctx context.Context) bool { return !a.gate.Dispatching(ctx) }
	a.pending.Hold, a.picker.Hold = hold, hold
	// Claude-auth routing (spec §13.3): never dispatch a claude job to a
	// foreman whose Claude CLI is logged out.
	a.pending.Exclude = a.claudeBlocks
}

// admitNew is the jobs.Admission hook for NEW jobs. A new non-P0 job also
// queues behind already-held jobs of equal or more urgent priority, so a
// fresh create cannot jump the FIFO while the gate is restricting.
func (a *App) admitNew(ctx context.Context, spec wire.JobSpec, taskID string) (bool, string) {
	if hold, reason := a.claudeHold(ctx, spec, false); hold {
		return true, reason // its foreman's Claude CLI is logged out (§13.3)
	}
	p0 := a.taskIsP0(ctx, taskID)
	ahead := 0
	if !p0 {
		n, err := a.jobs.CountHeldAhead(ctx, spec.Priority)
		if err != nil {
			log.Printf("factoryapp: throttle: count held: %v (ignored)", err)
		}
		ahead = n
	}
	return a.gate.Admit(ctx, spec.ID,
		throttle.Request{Kind: throttle.KindJob, P0: p0, JobPriority: spec.Priority},
		a.activeNonP0, ahead)
}

// taskIsP0 reports whether taskID names a task with priority 0. Lookup
// errors (or no task) mean non-P0; an unknown task never escalates work.
func (a *App) taskIsP0(ctx context.Context, taskID string) bool {
	if taskID == "" || a.tasks == nil {
		return false
	}
	t, err := a.tasks.GetTask(ctx, taskID)
	if err != nil {
		if !errors.Is(err, tasks.ErrNotFound) {
			log.Printf("factoryapp: throttle: task %s: %v", taskID, err)
		}
		return false
	}
	return t.Priority == 0
}

// jobIsP0 resolves a job's task and reports whether it is P0.
func (a *App) jobIsP0(ctx context.Context, jobID string) (taskID string, p0 bool) {
	taskID, ok, err := a.tasks.TaskForJob(ctx, jobID)
	if err != nil || !ok {
		return "", false
	}
	return taskID, a.taskIsP0(ctx, taskID)
}

// activeNonP0 lists dispatched/running jobs not filed under a P0 task (the
// ORANGE concurrency cap counts these).
func (a *App) activeNonP0(ctx context.Context) ([]string, error) {
	ids, err := a.jobs.ActiveJobIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := ids[:0]
	for _, id := range ids {
		if _, p0 := a.jobIsP0(ctx, id); !p0 {
			out = append(out, id)
		}
	}
	return out, nil
}

// releaserLoop runs ReleasePass every releaseTick until ctx is done.
// Errors are logged, never fatal (the next tick retries).
func (a *App) releaserLoop(ctx context.Context) {
	t := time.NewTicker(releaseTick)
	defer t.Stop()
	for {
		if _, err := a.ReleasePass(ctx); err != nil && ctx.Err() == nil {
			log.Printf("factoryapp: throttle releaser: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReleasePass walks the held queue in release order (job priority, then
// FIFO) and moves every job the gate now admits to pending; the pending
// picker dispatches it on its next pass. Jobs still held get a refreshed
// held_reason. A later job in the order may release past an earlier one
// that stays held (e.g. a P0 job behind a RED-held one, or a priority-3 job
// in ORANGE). Returns how many were released. Exported for tests.
func (a *App) ReleasePass(ctx context.Context) (int, error) {
	held, err := a.jobs.ListHeld(ctx, releaseLimit)
	if err != nil {
		return 0, fmt.Errorf("list held: %w", err)
	}
	released := 0
	for _, j := range held {
		if hold, reason := a.claudeHold(ctx, j.Spec, true); hold {
			if err := a.jobs.SetHeldReason(ctx, j.ID, reason); err != nil {
				return released, fmt.Errorf("refresh held reason %s: %w", j.ID, err)
			}
			continue
		}
		taskID, p0 := a.jobIsP0(ctx, j.ID)
		hold, reason := a.gate.Admit(ctx, j.ID,
			throttle.Request{Kind: throttle.KindJob, P0: p0, JobPriority: j.Spec.Priority},
			a.activeNonP0, 0)
		if hold {
			if err := a.jobs.SetHeldReason(ctx, j.ID, reason); err != nil {
				return released, fmt.Errorf("refresh held reason %s: %w", j.ID, err)
			}
			continue
		}
		applied, err := a.jobs.Release(ctx, j.ID)
		if err != nil {
			return released, fmt.Errorf("release %s: %w", j.ID, err)
		}
		if !applied {
			continue // cancelled meanwhile
		}
		released++
		log.Printf("factoryapp: throttle: released held job %s (priority %d, held since %s)", j.ID, j.Spec.Priority, j.CreatedAt)
		if taskID != "" {
			a.publishTaskChanged(ctx, taskID)
		}
	}
	return released, nil
}

// gateView is the gate block of GET /v1/usage and GET/POST /v1/throttle.
type gateView struct {
	throttle.Eval
	Held int `json:"held"`
}

func (a *App) gateStatus(ctx context.Context) (gateView, error) {
	n, err := a.jobs.CountHeld(ctx)
	if err != nil {
		return gateView{}, err
	}
	return gateView{Eval: a.gate.Evaluate(ctx), Held: n}, nil
}

// handleGetThrottle serves GET /v1/throttle: the gate block alone.
func (a *App) handleGetThrottle(w http.ResponseWriter, r *http.Request) {
	v, err := a.gateStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handlePostThrottle serves POST /v1/throttle {mode: auto|off, until?}:
// the operator override. off disables the gate until `until` (RFC3339;
// default now+1h, capped at 7d) and expires on its own; auto clears it.
// Same unauthenticated UI-surface class as /v1/usage and POST /v1/jobs.
func (a *App) handlePostThrottle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode  string `json:"mode"`
		Until string `json:"until"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	var until time.Time
	if in.Until != "" {
		t, err := time.Parse(time.RFC3339, in.Until)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "until: want RFC3339: "+err.Error())
			return
		}
		until = t
	}
	if _, err := a.gate.SetOverride(r.Context(), throttle.Mode(in.Mode), until); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	a.handleGetThrottle(w, r)
}
