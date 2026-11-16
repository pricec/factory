// Long-pending job alert (spec §6.7, §9.5; 2026-11-15 amendment): on
// 2026-11-15 two jobs sat pending with no matching foreman for ~7h and the
// gap was invisible until the operator asked. This loop closes it: every
// pass it re-runs the Matcher over pending jobs older than the threshold
// and, for each one that matches no online foreman, posts ONE notify.pending
// envelope to the operator inbox (planner:main) naming the job id, its
// requires, and why nothing matched - which required tags no foreman
// advertises, or the tags living only on offline foremen.
//
// A pending job that DOES match an online foreman is not stuck: the pending
// picker (spec §6.7) dispatches it on its next 10s pass, so the alert skips
// it - a job merely queued behind a deliberate hold never pages the
// operator. Dedupe is per job per pending episode (process-local,
// at-least-once, the dispatch notifier's model): the alert is keyed by the
// job row's pending-since stamp, so a long-stuck job does not spam the
// operator every tick, while a deadletter requeue re-stamps the row and
// starts a fresh episode that re-alerts.
package factoryapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/registry"
)

const (
	// pendingAlertThreshold is how long a job may sit pending with no
	// matching online foreman before the operator is notified (spec §11:
	// strictly more than 30 minutes).
	pendingAlertThreshold = 30 * time.Minute
	// pendingAlertTick is the check cadence. Dispatching stays the pending
	// picker's job (10s tick, spec §6.7); a 30m threshold needs no faster
	// check.
	pendingAlertTick = time.Minute
	// pendingAlertLimit bounds one pass's pending scan (the other list
	// loops' limit; a larger backlog alerts over successive passes).
	pendingAlertLimit = 1000
)

// pendingAlertLoop ticks the check until ctx is done. Pass errors are
// logged, never fatal: a transient store hiccup must not kill the loop
// (the next tick retries).
func (a *App) pendingAlertLoop(ctx context.Context) {
	ticker := time.NewTicker(pendingAlertTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := a.PendingAlertPass(ctx); err != nil {
				log.Printf("factoryapp: pending alert: %v", err)
			}
		}
	}
}

// PendingAlertPass runs one long-pending check pass (spec §6.7) and returns
// how many notify.pending notifications it posted. Exported for tests and
// the ticker loop alike (the jobs.PendingPicker.Pump pattern).
func (a *App) PendingAlertPass(ctx context.Context) (int, error) {
	pending, err := a.jobs.List(ctx, jobs.StatePending, "", pendingAlertLimit)
	if err != nil {
		return 0, fmt.Errorf("list pending: %w", err)
	}
	stillPending := make(map[string]bool, len(pending))
	now := a.jobs.Now() // the App's clock propagates to the jobs store (New)
	posted := 0
	for _, job := range pending {
		stillPending[job.ID] = true
		// pending_since is the row's updated_at: set at Create (== created_at)
		// and re-stamped by the deadletter requeue, so for a job that has
		// stayed pending it is exactly when the current pending episode began.
		since, err := time.Parse(time.RFC3339, job.UpdatedAt)
		if err != nil {
			log.Printf("factoryapp: pending alert: job %s updated_at %q: %v", job.ID, job.UpdatedAt, err)
			continue
		}
		waited := now.Sub(since)
		if waited <= pendingAlertThreshold {
			continue // spec §11: strictly more than 30 minutes
		}
		if a.alreadyAlerted(job.ID, job.UpdatedAt) {
			continue // one alert per pending episode (spec §6.7)
		}
		// A pinned pending job never consults the Matcher (spec §6.7
		// foreman pin): it is stuck only while its pinned foreman is
		// offline or unregistered, and the alert says so - a
		// requires-based diagnosis would mislead (the pin, not the
		// tags, is why nothing ran).
		if job.Spec.Foreman != "" {
			pin, found, err := jobs.FindForeman(ctx, a.jobs, job.Spec.Foreman)
			if err != nil {
				return posted, fmt.Errorf("pin lookup %s: %w", job.ID, err)
			}
			if found && pin.Online {
				continue // the pending picker dispatches it on its next pass
			}
			reason := "pinned foreman " + job.Spec.Foreman + " is not registered"
			if found {
				reason = "pinned foreman " + pin.ID + " is offline"
				if pin.LastSeen != "" {
					reason += " (last seen " + pin.LastSeen + ")"
				}
			}
			a.raiseSystemAlert(ctx, alertPriorityPending,
				"Job "+job.ID+" pending "+waited.Truncate(time.Minute).String()+" on a pinned foreman",
				fmt.Sprintf("%s\nforeman pin: %s\nrequires (informational): %s; pending since %s",
					reason, job.Spec.Foreman, strings.Join(job.Spec.Requires, ", "), job.UpdatedAt),
				"factory:pending")
			a.markAlerted(job.ID, job.UpdatedAt)
			posted++
			log.Printf("factoryapp: pending alert: job %s pending %s pinned to %s: %s",
				job.ID, waited.Truncate(time.Minute), job.Spec.Foreman, reason)
			continue
		}
		// A pending job that matches an online foreman is about to be
		// dispatched by the pending picker (spec §6.7): not stuck, skip.
		if _, ok, err := a.matcher.Match(ctx, job.Spec.Requires); err != nil {
			return posted, fmt.Errorf("match %s: %w", job.ID, err)
		} else if ok {
			continue
		}
		diag, alertable := a.diagnoseNoMatch(ctx, job.Spec.Requires)
		if !alertable {
			continue // registry changed mid-pass; re-evaluate next tick
		}
		a.raiseSystemAlert(ctx, alertPriorityPending,
			"Job "+job.ID+" pending "+waited.Truncate(time.Minute).String()+" with no matching foreman",
			fmt.Sprintf("%s\nrequires: %s\nforemen online %d of %d; pending since %s",
				diag.reason, strings.Join(job.Spec.Requires, ", "), diag.online, diag.total, job.UpdatedAt),
			"factory:pending")
		a.markAlerted(job.ID, job.UpdatedAt)
		posted++
		log.Printf("factoryapp: pending alert: job %s pending %s with no match: %s",
			job.ID, waited.Truncate(time.Minute), diag.reason)
	}
	a.forgetNotPending(stillPending)
	return posted, nil
}

// alertForeman is one registry row for the no-match diagnosis: what
// diagnoseNoMatch needs (registry.Foreman plus the liveness stamp for the
// offline wording).
type alertForeman struct {
	id       string
	online   bool
	lastSeen string // RFC3339; empty when the foreman was never seen
	caps     registry.Capabilities
}

// describe renders the foreman for an operator-facing reason string.
func (f alertForeman) describe() string {
	if f.lastSeen == "" {
		return f.id
	}
	return f.id + " (last seen " + f.lastSeen + ")"
}

// noMatchDiagnosis explains why no online foreman satisfied a pending job's
// requires (the notify.pending payload's reason/missing_tags/
// offline_foremen fields, spec §9.5).
type noMatchDiagnosis struct {
	reason  string
	missing []string // required tags no registered foreman advertises
	offline []string // foremen carrying required tags, offline (with last seen)
	total   int      // registered foremen rows
	online  int      // currently-online foremen rows
}

// diagnoseNoMatch classifies why no ONLINE foreman satisfies requires
// (spec §6.7 match semantics: online foremen whose tags are a superset of
// requires). alertable is false only when an online foreman would match -
// the Match call and this diagnosis read the registry a moment apart, so
// the pass skips the alert rather than posting a stale one.
func diagnoseNoMatch(requires []string, foremen []alertForeman) (noMatchDiagnosis, bool) {
	d := noMatchDiagnosis{total: len(foremen)}
	for _, f := range foremen {
		if f.online {
			d.online++
		}
	}
	if d.total == 0 {
		d.reason = "no foremen are registered"
		return d, true
	}

	// Per required tag: nobody advertises it at all, or every advertiser is
	// offline.
	var offlineOnly []string
	for _, tag := range requires {
		var advertised, advertisedOnline bool
		for _, f := range foremen {
			if !f.caps.Has(tag) {
				continue
			}
			advertised = true
			if f.online {
				advertisedOnline = true
			}
		}
		switch {
		case !advertised:
			d.missing = append(d.missing, tag)
		case !advertisedOnline:
			offlineOnly = append(offlineOnly, tag)
		}
	}
	// The offline foremen worth naming: those carrying at least one required
	// tag (they hold part of what the job needs but are dark).
	for _, f := range foremen {
		if f.online {
			continue
		}
		for _, tag := range requires {
			if f.caps.Has(tag) {
				d.offline = append(d.offline, f.describe())
				break
			}
		}
	}

	// Deterministic payloads regardless of the requires order the job
	// carried (the slices are diagnosis-local, safe to sort in place).
	slices.Sort(d.missing)
	slices.Sort(offlineOnly)
	slices.Sort(d.offline)

	var parts []string
	if len(d.missing) > 0 {
		parts = append(parts, "no foreman advertises: "+strings.Join(d.missing, ", "))
	}
	if len(offlineOnly) > 0 {
		parts = append(parts, "only offline foremen have: "+strings.Join(offlineOnly, ", "))
	}
	if len(d.missing) == 0 && len(offlineOnly) == 0 {
		// Every required tag has an online advertiser, yet the superset
		// match failed. Either no single online foreman carries them all
		// (alertable), or the registry changed between the Match call and
		// this diagnosis and an online foreman now satisfies the requires
		// (transient - skip; the next pass re-evaluates).
		for _, f := range foremen {
			if f.online && f.caps.HasAll(requires) {
				return d, false
			}
		}
		if d.online == 0 {
			// Empty (or fully advertised) requires with an all-offline
			// registry: nothing to dispatch to right now.
			parts = append(parts, "all foremen are offline")
		} else {
			parts = append(parts, "no single online foreman has all required tags")
		}
	}
	if len(parts) == 0 {
		return d, false // an online foreman matches after all: transient
	}
	d.reason = strings.Join(parts, "; ")
	return d, true
}

// alertForemen lists every registered foreman (online and offline) with
// decoded capabilities for the no-match diagnosis. Unlike foremanSource it
// skips the running-jobs count, which matching-for-alerts does not need;
// a row with undecodable capabilities is skipped (logged), not fatal.
func (a *App) alertForemen(ctx context.Context) ([]alertForeman, error) {
	rows, err := a.jobs.Foremen(ctx)
	if err != nil {
		return nil, fmt.Errorf("list foremen: %w", err)
	}
	out := make([]alertForeman, 0, len(rows))
	for _, row := range rows {
		f := alertForeman{id: row.ID, online: row.Online, lastSeen: row.LastSeen}
		if err := json.Unmarshal([]byte(row.Capabilities), &f.caps); err != nil {
			log.Printf("factoryapp: pending alert: foreman %s capabilities: %v (skipped)", row.ID, err)
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// diagnoseNoMatch lists the registry and classifies the no-match reason.
func (a *App) diagnoseNoMatch(ctx context.Context, requires []string) (noMatchDiagnosis, bool) {
	foremen, err := a.alertForemen(ctx)
	if err != nil {
		// Registry unreadable: no specific reason available, but the job
		// has demonstrably sat pending past the threshold, so say so.
		log.Printf("factoryapp: pending alert: foremen: %v", err)
		return noMatchDiagnosis{reason: "foremen registry unavailable: " + err.Error()}, true
	}
	return diagnoseNoMatch(requires, foremen)
}

// alreadyAlerted reports whether jobID already got its notify.pending for
// the pending episode that began at since (the row's updated_at stamp).
func (a *App) alreadyAlerted(jobID, since string) bool {
	a.pendingAlertMu.Lock()
	defer a.pendingAlertMu.Unlock()
	return a.pendingAlertedJobs[jobID] == since
}

// markAlerted records jobID as alerted for the pending episode that began
// at since.
func (a *App) markAlerted(jobID, since string) {
	a.pendingAlertMu.Lock()
	defer a.pendingAlertMu.Unlock()
	a.pendingAlertedJobs[jobID] = since
}

// forgetNotPending drops ids that left the pending set, so the dedupe map
// stays bounded; a requeued job re-alerts via its new pending-since stamp
// (spec §6.4).
func (a *App) forgetNotPending(stillPending map[string]bool) {
	a.pendingAlertMu.Lock()
	defer a.pendingAlertMu.Unlock()
	for id := range a.pendingAlertedJobs {
		if !stillPending[id] {
			delete(a.pendingAlertedJobs, id)
		}
	}
}
