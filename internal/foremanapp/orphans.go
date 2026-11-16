package foremanapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// Orphan recovery (spec §8.7, 2026-11-16 amendment). A foreman restart kills
// its in-process worker goroutines and their child processes, but the
// factory registry never hears about it: every job the old process was
// running stays `running` forever. At startup, before any loop can spawn,
// the foreman reconciles the registry against reality.

const (
	// defaultProcRoot is the process table the live-worker probe scans.
	defaultProcRoot = "/proc"
	// orphanListLimit is the factory's maximum page for GET /v1/jobs.
	orphanListLimit = 1000
	// orphanStartupTries bounds the startup pass: the factory can be briefly
	// unreachable right after a restart.
	orphanStartupTries   = 5
	orphanStartupBackoff = 2 * time.Second
	// orphanHTTPTimeout bounds one registry query.
	orphanHTTPTimeout = 15 * time.Second
	// deletedSuffix is what the kernel appends to a cwd whose directory was
	// removed; the job directory is still the process's working directory.
	deletedSuffix = " (deleted)"
)

// Reconciliation is the outcome of one ReconcileOrphans pass. Every job the
// registry listed as running for this foreman lands in exactly one bucket.
type Reconciliation struct {
	// Listed is how many running jobs the registry reported for this foreman.
	Listed int
	// Orphaned are jobs with no live worker process; each was marked failed
	// (kind crash) so the factory arms its retry (spec §6.4).
	Orphaned []string
	// Live are jobs with a live worker process; they stay running.
	Live []string
	// Local are jobs this process spawned itself; the harness owns them.
	Local []string
}

// registryJob is the subset of a factory job record the pass needs.
type registryJob struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	ForemanID string `json:"foreman_id"`
}

// ReconcileOrphans runs one startup reconciliation pass (spec §8.7):
//
//  1. list the registry's running jobs owned by THIS foreman (never another
//     foreman's jobs - the query is owner-filtered and every row is checked
//     against the owner forms again);
//  2. for each one, skip it when this process spawned it, skip it when a
//     live worker process runs in its job directory, and otherwise post a
//     failed job.result (kind crash) to the factory. The factory applies it
//     through the ordinary terminal path and arms the crash retry, so the
//     picker re-dispatches the SAME job id (spec §6.4).
//
// It fails closed: if the process table cannot be read completely, nothing
// is failed. Jobs whose post fails are reported in the returned error and
// the rest of the pass continues. Run calls it once, before any loop can
// spawn a worker; it is safe to call again (a failed job is no longer
// listed as running, and a second post is a terminal-idempotent no-op).
func (a *App) ReconcileOrphans(ctx context.Context) (Reconciliation, error) {
	var rec Reconciliation
	jobs, err := a.listOwnedRunning(ctx)
	if err != nil {
		return rec, fmt.Errorf("orphan recovery: list running jobs: %w", err)
	}
	rec.Listed = len(jobs)
	if len(jobs) == 0 {
		return rec, nil
	}
	cwds, err := processCwds(a.procRoot)
	if err != nil {
		return rec, fmt.Errorf("orphan recovery: read process table (nothing failed): %w", err)
	}
	jobsRoot := jobsRootFor(a.dataDir)

	var errs []error
	for _, j := range jobs {
		switch {
		case a.spawnedHere(j.ID):
			rec.Local = append(rec.Local, j.ID)
		case hasWorkerIn(filepath.Join(jobsRoot, j.ID), cwds):
			rec.Live = append(rec.Live, j.ID)
		default:
			if err := a.failOrphan(ctx, j.ID); err != nil {
				errs = append(errs, err)
				continue
			}
			rec.Orphaned = append(rec.Orphaned, j.ID)
		}
	}
	return rec, errors.Join(errs...)
}

// recoverOrphans is the startup wrapper around ReconcileOrphans: bounded
// retries while the factory is unreachable, LOUD logging either way. When
// every attempt fails the foreman still starts; the stranded jobs then fall
// to the sweeper's timeout backstop (spec §6.2), which is terminal and
// does not retry.
func (a *App) recoverOrphans(ctx context.Context) {
	var err error
	for attempt := 1; attempt <= orphanStartupTries; attempt++ {
		var rec Reconciliation
		rec, err = a.ReconcileOrphans(ctx)
		if err == nil {
			log.Printf("foremanapp: orphan recovery: listed=%d orphaned=%v live=%v local=%v",
				rec.Listed, rec.Orphaned, rec.Live, rec.Local)
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("foremanapp: orphan recovery attempt %d/%d: %v", attempt, orphanStartupTries, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(orphanStartupBackoff):
		}
	}
	log.Printf("foremanapp: ORPHAN RECOVERY SKIPPED after %d attempts (%v): running jobs owned by %s may stay 'running' until the sweeper times them out",
		orphanStartupTries, err, a.inboxID)
}

// ownerForms are the spellings this foreman's jobs carry in the registry's
// foreman_id column: the inbox identity (dispatch and spawn registration
// store it), its explicit "foreman:" form, and the bare id. Any other owner
// is another foreman's job and is never touched.
func (a *App) ownerForms() []string {
	forms := []string{a.inboxID, "foreman:" + a.id, a.id}
	seen := map[string]bool{}
	var out []string
	for _, f := range forms {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// listOwnedRunning returns the registry's running jobs owned by this
// foreman, deduplicated across owner forms.
func (a *App) listOwnedRunning(ctx context.Context) ([]registryJob, error) {
	owners := a.ownerForms()
	ownedHere := map[string]bool{}
	for _, o := range owners {
		ownedHere[o] = true
	}
	seen := map[string]bool{}
	var out []registryJob
	for _, owner := range owners {
		q := url.Values{
			"state":   {"running"},
			"foreman": {owner},
			"limit":   {strconv.Itoa(orphanListLimit)},
		}
		var rows []registryJob
		if err := a.getRegistry(ctx, "/v1/jobs?"+q.Encode(), &rows); err != nil {
			return nil, err
		}
		if len(rows) >= orphanListLimit {
			log.Printf("foremanapp: ORPHAN RECOVERY TRUNCATED: %d+ running jobs for %s; the rest are reconciled on the next startup", len(rows), owner)
		}
		for _, r := range rows {
			// Defence in depth: the query is owner-filtered, but a row whose
			// owner is not ours is never ours to fail.
			if r.State != "running" || !ownedHere[r.ForemanID] || seen[r.ID] || !plausibleJobID(r.ID) {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// getRegistry GETs path on the factory and decodes the JSON body into out.
func (a *App) getRegistry(ctx context.Context, path string, out any) error {
	client := &http.Client{Timeout: orphanHTTPTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimSlash(a.factoryURL)+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// failOrphan posts the loud failed result for one orphaned job. The
// envelope is posted by the foreman (its factory:main credential), with the
// crash kind so the retry policy arms (spec §6.4). The worker tokens minted
// for the job are revoked afterward (spec §7.4): with no process behind them
// they are dead credentials.
func (a *App) failOrphan(ctx context.Context, jobID string) error {
	res := wire.JobResult{
		JobID:   jobID,
		Status:  wire.StatusFailed,
		Summary: "worker orphaned by foreman restart",
		Error: &wire.JobError{
			Kind: wire.KindCrash,
			Message: fmt.Sprintf("orphaned: foreman %s restarted while job %s was running; "+
				"no live worker process was found at startup, so the job is failed (crash) and its retry re-arms",
				a.id, jobID),
		},
	}
	if err := res.Validate(nil); err != nil {
		return fmt.Errorf("orphan result for %s: %w", jobID, err)
	}
	payload, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("orphan result for %s: %w", jobID, err)
	}
	env := wire.MessageEnvelope{
		Type:    "job.result",
		JobID:   jobID,
		From:    a.inboxID,
		Payload: payload,
	}
	if err := a.poster.PostToParent(ctx, env); err != nil {
		return fmt.Errorf("post orphan result for %s: %w", jobID, err)
	}
	log.Printf("foremanapp: ORPHAN %s: running with no worker process after foreman restart; marked failed (crash), retry armed by the factory", jobID)
	if err := a.auth.InvalidateJob(ctx, jobID); err != nil {
		log.Printf("foremanapp: orphan %s: revoke worker credentials: %v", jobID, err)
	}
	return nil
}

// markSpawned records that this process started a worker for jobID. The
// startup pass never reconciles such a job: its harness owns it.
func (a *App) markSpawned(jobID string) {
	a.spawnedMu.Lock()
	defer a.spawnedMu.Unlock()
	a.spawned[jobID] = true
}

func (a *App) spawnedHere(jobID string) bool {
	a.spawnedMu.Lock()
	defer a.spawnedMu.Unlock()
	return a.spawned[jobID]
}

// jobsRootFor is the directory holding every per-job worker directory
// (spec §8.2), resolved the way the kernel reports a process's cwd: absolute,
// with symlinks evaluated when the directory exists.
func jobsRootFor(dataDir string) string {
	root, err := filepath.Abs(filepath.Join(dataDir, jobsDirName))
	if err != nil {
		return filepath.Join(dataDir, jobsDirName)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return resolved
	}
	return root
}

// processCwds returns the working directory of every process under procRoot
// that can be inspected. A worker child runs with cmd.Dir set to its job
// directory (workerharness execChild), so its cwd names the job, and so does
// the cwd of anything it started that still has that directory as cwd.
//
// Processes that exited between the directory listing and the readlink are
// skipped. Processes owned by another user are skipped too: a same-user
// worker child can never be one of them. Any other error is returned, so the
// caller fails closed instead of guessing that a job is dead.
func processCwds(procRoot string) ([]string, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var cwds []string
	for _, e := range entries {
		if !isPID(e.Name()) {
			continue
		}
		target, err := os.Readlink(filepath.Join(procRoot, e.Name(), "cwd"))
		switch {
		case err == nil:
			cwds = append(cwds, strings.TrimSuffix(target, deletedSuffix))
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
		default:
			return nil, fmt.Errorf("read cwd of pid %s: %w", e.Name(), err)
		}
	}
	return cwds, nil
}

// hasWorkerIn reports whether any cwd is jobDir or lies beneath it.
func hasWorkerIn(jobDir string, cwds []string) bool {
	prefix := jobDir + string(filepath.Separator)
	for _, cwd := range cwds {
		if cwd == jobDir || strings.HasPrefix(cwd, prefix) {
			return true
		}
	}
	return false
}

func isPID(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// plausibleJobID rejects ids that cannot name a job directory under the
// jobs root (spec §8.2). A registry row with a separator or a dot-segment is
// refused outright, so it can never point the probe outside the jobs root.
func plausibleJobID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\`) && id != "." && id != ".."
}
