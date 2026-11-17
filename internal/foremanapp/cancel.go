package foremanapp

import "log"

// Worker cancellation (spec §3.2 job.cancel): the factory posts a job.cancel
// envelope to the owning foreman's inbox when a dispatched or running job
// is cancelled or superseded. The foreman stops that job's worker (SIGTERM
// to the process group, SIGKILL after the grace) and posts no result; the
// factory ignores results for terminal jobs anyway (§6.4).

// registerCancel returns the cancel channel for a job about to run. If the
// job was already cancelled (cancel arrived before its spawn), the channel
// comes back closed so the harness exits at once.
func (a *App) registerCancel(jobID string) <-chan struct{} {
	a.spawnedMu.Lock()
	defer a.spawnedMu.Unlock()
	ch := make(chan struct{})
	if a.cancelled[jobID] {
		close(ch)
	} else {
		a.cancels[jobID] = ch
	}
	return ch
}

func (a *App) clearCancel(jobID string) {
	a.spawnedMu.Lock()
	defer a.spawnedMu.Unlock()
	delete(a.cancels, jobID)
}

// jobCancelled reports whether a job.cancel was already seen for jobID.
func (a *App) jobCancelled(jobID string) bool {
	a.spawnedMu.Lock()
	defer a.spawnedMu.Unlock()
	return a.cancelled[jobID]
}

// cancelWorker handles a job.cancel for jobID. It is idempotent. The id is
// remembered so a job.request that arrives (or is redelivered) after the
// cancel is dropped instead of spawning a worker for a terminal job; this
// is also how an offline foreman's queued request is dropped on reconnect.
func (a *App) cancelWorker(jobID string) {
	a.spawnedMu.Lock()
	defer a.spawnedMu.Unlock()
	a.cancelled[jobID] = true
	if ch, ok := a.cancels[jobID]; ok {
		close(ch)
		delete(a.cancels, jobID)
		log.Printf("foremanapp: job.cancel %s: stopping worker", jobID)
		return
	}
	log.Printf("foremanapp: job.cancel %s: no live worker", jobID)
}
