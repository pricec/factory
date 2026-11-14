package factoryapp

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/pricec/factory/internal/jobs"
)

// Dispatch notifier (issue #20). The dispatch paths in the composed
// packages (mcpfactory create_job, jobs.Scheduler.Pump, RetryPicker.Pump)
// only mutate the jobs DB; none of them posts job.request envelopes to
// foreman inboxes. This loop bridges that gap: every 500ms it lists jobs
// in state dispatched and, for each id not in the in-memory notified set,
// posts a job.request envelope carrying the JobSpec JSON to the foreman's
// inbox via the inbox library API.
//
// Delivery is at-least-once: the notified set is process-local, so a
// factory restart re-notifies every still-dispatched job and the foreman
// sees a duplicate job.request. Foremen dedupe by job id (a request for a
// job they already accepted is dropped foreman-side); the inbox's own
// redelivery rules cover the rest.
//
// Jobs that leave the dispatched state are forgotten, so a retry
// re-dispatch of the SAME job id (spec §6.4) is notified again.

const (
	notifyTick      = 500 * time.Millisecond
	notifyListLimit = 1000
)

func hasForemanPrefix(id string) bool { return strings.HasPrefix(id, "foreman:") }

func (a *App) notifyLoop(ctx context.Context) {
	ticker := time.NewTicker(notifyTick)
	defer ticker.Stop()
	notified := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.notifyPass(ctx, notified)
		}
	}
}

func (a *App) notifyPass(ctx context.Context, notified map[string]bool) {
	listed, err := a.jobs.List(ctx, jobs.StateDispatched, "", notifyListLimit)
	if err != nil {
		log.Printf("factoryapp: notifier: list dispatched: %v", err)
		return
	}
	current := make(map[string]bool, len(listed))
	for _, job := range listed {
		current[job.ID] = true
		if notified[job.ID] {
			continue
		}
		if job.ForemanID == "" {
			continue
		}
		if err := a.notifyDispatched(ctx, job); err != nil {
			log.Printf("factoryapp: notifier: job %s: %v (retrying next tick)", job.ID, err)
			continue
		}
		notified[job.ID] = true
		log.Printf("factoryapp: notifier: job.request for %s -> foreman inbox", job.ID)
	}
	// Forget ids no longer dispatched so retries re-notify.
	for id := range notified {
		if !current[id] {
			delete(notified, id)
		}
	}
}
