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

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/jobs"
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
	// ConflictOff is the FACTORY_CONFLICT=off kill switch: the conflict
	// coordinator (conflict.go) admits everything.
	ConflictOff bool
	// ConflictMaxWait bounds a conflict hold (0 = DefaultConflictMaxWait).
	ConflictMaxWait time.Duration
	// FableMaxConcurrent caps active approved Fable jobs (fable.go).
	FableMaxConcurrent int
}

// LoadThrottleConfig reads FACTORY_THROTTLE ("off"/"0"/"false" disables).
func LoadThrottleConfig() ThrottleConfig {
	var cfg ThrottleConfig
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FACTORY_THROTTLE"))) {
	case "off", "0", "false", "no":
		cfg.Off = true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FACTORY_CONFLICT"))) {
	case "off", "0", "false", "no":
		cfg.ConflictOff = true
	}
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv("FACTORY_CONFLICT_MAX_WAIT"))); err == nil && d > 0 {
		cfg.ConflictMaxWait = d
	}
	cfg.FableMaxConcurrent = LoadFableMaxConcurrent()
	return cfg
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
	a.conflictOff = cfg.Throttle.ConflictOff
	a.fableMax = cfg.Throttle.FableMaxConcurrent
	a.conflictMaxWait = cfg.Throttle.ConflictMaxWait
	if a.conflictMaxWait <= 0 {
		a.conflictMaxWait = DefaultConflictMaxWait
	}
	if a.conflictOff {
		log.Printf("factoryapp: FACTORY_CONFLICT=off: conflict coordinator disabled")
	}
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
	if hold, reason := a.fableAdmit(ctx, spec, taskID); hold {
		return true, reason // Fable needs operator approval first
	}
	if hold, reason := a.claudeHold(ctx, spec, false); hold {
		return true, reason // its foreman's Claude CLI is logged out (§13.3)
	}
	p0 := a.taskIsP0(ctx, taskID)
	if o, ok := ctx.Value(prioOverrideKey{}).(int); ok {
		p0 = o == 0 // merge job inheriting its source job's override
	}
	ahead := 0
	if !p0 {
		n, err := a.jobs.CountHeldAhead(ctx, spec.Priority)
		if err != nil {
			log.Printf("factoryapp: throttle: count held: %v (ignored)", err)
		}
		ahead = n
	}
	if hold, reason := a.gate.Admit(ctx, spec.ID,
		throttle.Request{Kind: throttle.KindJob, P0: p0, JobPriority: spec.Priority},
		a.activeNonP0, ahead); hold {
		return true, reason // precedence: the usage reason wins
	}
	// Second stage: the conflict coordinator (conflict.go).
	return a.conflictAdmit(ctx, spec, "", taskID, p0, false)
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

// jobIsP0 resolves a job's task and reports whether the job is P0: a
// job-level priority override (set_job_priority) wins over its task's.
func (a *App) jobIsP0(ctx context.Context, jobID string) (taskID string, p0 bool) {
	taskID, ok, err := a.tasks.TaskForJob(ctx, jobID)
	if err != nil || !ok {
		taskID = ""
	}
	if o, set, err := a.jobs.PriorityOverride(ctx, jobID); err == nil && set {
		return taskID, o == 0
	}
	if taskID == "" {
		return "", false
	}
	return taskID, a.taskIsP0(ctx, taskID)
}

// releaseNow re-evaluates the held queue after a priority change and pumps
// the pending picker, so a job just released dispatches without waiting for
// the next releaser/picker tick.
func (a *App) releaseNow(ctx context.Context) {
	if _, err := a.ReleasePass(ctx); err != nil {
		log.Printf("factoryapp: throttle: release after priority change: %v", err)
		return
	}
	if _, err := a.pending.Pump(ctx); err != nil {
		log.Printf("factoryapp: throttle: pump after priority change: %v", err)
	}
}

// taskPriorityChanged is the update_task hook: a task's priority change
// re-evaluates its held jobs immediately.
func (a *App) taskPriorityChanged(ctx context.Context, taskID string) {
	a.releaseNow(ctx)
	a.publishTaskChanged(ctx, taskID)
}

// jobPriorityChanged is the set_job_priority hook: it records the change
// (with the planner's reason) on the job's task thread and re-evaluates the
// throttle hold at once.
func (a *App) jobPriorityChanged(ctx context.Context, jobID string, oldPrio, newPrio int, reason string) {
	text := fmt.Sprintf("job %s priority set to P%d (was %d): %s", jobID, newPrio, oldPrio, reason)
	log.Printf("factoryapp: %s", text)
	if taskID, ok, err := a.tasks.TaskForJob(ctx, jobID); err == nil && ok && a.inbox != nil {
		data, _ := json.Marshal(map[string]any{
			"wake": false, "action": "priority", "job_id": jobID,
			"priority": newPrio, "old_priority": oldPrio, "reason": reason, "text": text,
		})
		if _, err := a.inbox.Post(ctx, auth.PlannerMainInbox, wire.MessageEnvelope{
			Type: notifyConflictType, From: "factory", Payload: data, JobID: jobID, ThreadID: taskID,
		}); err != nil {
			log.Printf("factoryapp: priority note for %s: %v", jobID, err)
		}
		a.publishTaskChanged(ctx, taskID)
	}
	a.releaseNow(ctx)
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
		if hold, reason := a.fableRelease(ctx, j); hold {
			if err := a.jobs.SetHeldReason(ctx, j.ID, reason); err != nil {
				return released, fmt.Errorf("refresh held reason %s: %w", j.ID, err)
			}
			continue
		}
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
		wasConflict := jobs.HeldKind(j.HeldReason) == "conflict"
		if hold, reason := a.conflictAdmit(ctx, j.Spec, j.CreatedAt, taskID, p0, true); hold {
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
		if wasConflict {
			a.noteConflict(ctx, taskID, j.ID, conflictReleased, "", "",
				"job "+j.ID+" is no longer waiting on a conflicting job and was released")
		}
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
	// HeldBy breaks Held down by hold kind: usage, conflict, claude_auth.
	HeldBy map[string]int `json:"held_by,omitempty"`
}

func (a *App) gateStatus(ctx context.Context) (gateView, error) {
	n, err := a.jobs.CountHeld(ctx)
	if err != nil {
		return gateView{}, err
	}
	by, err := a.jobs.CountHeldByKind(ctx)
	if err != nil {
		return gateView{}, err
	}
	return gateView{Eval: a.gate.Evaluate(ctx), Held: n, HeldBy: by}, nil
}

// handleGetThrottle serves GET /v1/throttle: the gate block alone.
func (a *App) handleGetThrottle(w http.ResponseWriter, r *http.Request) {
	noStore(w)
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
