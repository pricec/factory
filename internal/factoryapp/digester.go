package factoryapp

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/summarize"
)

// digestQueueCap bounds the summariser backlog; a result arriving when
// the queue is full is simply not digested (read_result serves the
// original).
const digestQueueCap = 32

// digester is the async local-model summarisation pre-pass (spec §6.13).
// The consumer enqueues each applied result; ONE worker goroutine calls
// the summariser (one model call at a time) and stores the digest next
// to the untouched results. Nothing here can block or fail job
// completion: enqueue never blocks, and every summariser failure just
// leaves the job without a digest.
type digester struct {
	client *summarize.Client
	store  *jobs.Store
	queue  chan digestItem

	mu       sync.Mutex
	inflight map[string]chan struct{} // job id -> closed when its digest attempt ends
}

type digestItem struct {
	jobID string
	index int
	text  string
	done  chan struct{}
}

func newDigester(cfg summarize.Config, store *jobs.Store) *digester {
	if !cfg.Enabled() {
		return nil
	}
	return &digester{
		client:   summarize.New(cfg),
		store:    store,
		queue:    make(chan digestItem, digestQueueCap),
		inflight: map[string]chan struct{}{},
	}
}

// lastResultText returns the index and summarisable text of the job's
// last result (spec §6.13: JobResult.result; a string is unquoted).
func lastResultText(results json.RawMessage) (int, string) {
	var entries []struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(results, &entries); err != nil || len(entries) == 0 {
		return -1, ""
	}
	i := len(entries) - 1
	return i, summarize.ResultText(entries[i].Result)
}

// enqueue schedules job's last result for digesting when it is over the
// threshold. Never blocks. No-op on a nil digester (disabled).
func (d *digester) enqueue(job jobs.Job) {
	if d == nil {
		return
	}
	idx, text := lastResultText(job.Results)
	if idx < 0 || len(text) < d.client.Config().MinBytes {
		return
	}
	done := make(chan struct{})
	d.mu.Lock()
	if _, busy := d.inflight[job.ID]; busy {
		d.mu.Unlock()
		return
	}
	d.inflight[job.ID] = done
	d.mu.Unlock()
	select {
	case d.queue <- digestItem{jobID: job.ID, index: idx, text: text, done: done}:
	default:
		log.Printf("factoryapp: summarizer: queue full; %s keeps its original only", job.ID)
		d.finish(job.ID, done)
	}
}

func (d *digester) finish(jobID string, done chan struct{}) {
	d.mu.Lock()
	if d.inflight[jobID] == done {
		delete(d.inflight, jobID)
	}
	d.mu.Unlock()
	close(done)
}

// run is the single worker loop; it returns when ctx is done.
func (d *digester) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-d.queue:
			d.process(ctx, it)
		}
	}
}

func (d *digester) process(ctx context.Context, it digestItem) {
	defer d.finish(it.jobID, it.done)
	// A duplicate job.result (terminal-idempotent no-op) re-enqueues the
	// same result: never summarise it twice.
	if dg, ok, err := d.store.ResultDigest(ctx, it.jobID); err == nil && ok && dg.ResultIndex == it.index {
		return
	}
	s, err := d.client.Summarize(ctx, it.text) // bounded by the client's Timeout
	if err != nil {
		log.Printf("factoryapp: summarizer: %s: %v (original kept)", it.jobID, err)
		return
	}
	if err := d.store.SetResultDigest(ctx, it.jobID, jobs.ResultDigest{
		ResultIndex: it.index,
		Text:        s.Text,
		Model:       s.Model,
		InputBytes:  s.InputBytes,
		Truncated:   s.Truncated,
		DurationMS:  s.Duration.Milliseconds(),
	}); err != nil {
		log.Printf("factoryapp: summarizer: store digest for %s: %v", it.jobID, err)
		return
	}
	log.Printf("factoryapp: summarizer: %s digested %d -> %d bytes in %s (%s)",
		it.jobID, s.InputBytes, len(s.Text), s.Duration.Round(time.Millisecond), s.Model)
}

// resultDigest serves read_result (mcpfactory.Config.ResultDigest): it
// waits - at most ReadWait - for an in-flight digest of jobID, then
// returns the stored digest when it condenses the result at lastIndex.
func (d *digester) resultDigest(ctx context.Context, jobID string, lastIndex int) (jobs.ResultDigest, bool) {
	if d == nil {
		return jobs.ResultDigest{}, false
	}
	d.mu.Lock()
	done := d.inflight[jobID]
	d.mu.Unlock()
	if done != nil {
		if wait := d.client.Config().ReadWait; wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-done:
			case <-t.C:
			case <-ctx.Done():
			}
			t.Stop()
		}
	}
	dg, ok, err := d.store.ResultDigest(ctx, jobID)
	if err != nil || !ok || dg.ResultIndex != lastIndex {
		return jobs.ResultDigest{}, false
	}
	return dg, true
}

// resultDigest adapts the digester to mcpfactory.Config.ResultDigest; it
// reports no digest when summarisation is disabled.
func (a *App) resultDigest(ctx context.Context, jobID string, lastIndex int) (jobs.ResultDigest, bool) {
	if a.digest == nil {
		return jobs.ResultDigest{}, false
	}
	return a.digest.resultDigest(ctx, jobID, lastIndex)
}
