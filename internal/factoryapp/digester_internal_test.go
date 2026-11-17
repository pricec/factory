package factoryapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/summarize"
	"github.com/pricec/factory/internal/wire"
)

// seedDone creates a job and applies a done result carrying result.
func seedDone(t *testing.T, store *jobs.Store, result string) jobs.Job {
	t.Helper()
	ctx := context.Background()
	spec := wire.NewJobSpec("long job", "factory")
	if err := store.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := store.Dispatch(ctx, spec.ID, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEvent(ctx, spec.ID, "started"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if err := store.ApplyResult(ctx, wire.JobResult{JobID: spec.ID, Status: wire.StatusDone, Summary: "ok", Result: raw}); err != nil {
		t.Fatal(err)
	}
	job, _, err := store.Get(ctx, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// TestDigester covers the async pre-pass end to end against a fake
// Ollama (spec §6.13): a digest is stored beside the untouched result,
// read_result's lookup waits for one in flight, and every failure leaves
// the job without a digest (the original is served).
func TestDigester(t *testing.T) {
	long := strings.Repeat("step 7 wrote /tmp/out.txt; PR #41 opened.\n", 200)
	tests := []struct {
		name     string
		input    string
		reply    string
		delay    time.Duration
		timeout  time.Duration
		disabled bool
		want     bool // digest stored
	}{
		{name: "digested", input: long, reply: "- step 7 wrote /tmp/out.txt\n- PR #41 opened", want: true},
		{name: "below threshold", input: "tiny", reply: "- tiny"},
		{name: "reasoning leak rejected", input: long, reply: "We need to summarise this."},
		{name: "empty rejected", input: long, reply: ""},
		{name: "ollama too slow", input: long, reply: "- ok", delay: 400 * time.Millisecond, timeout: 50 * time.Millisecond},
		{name: "disabled", input: long, reply: "- ok", disabled: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(tc.delay)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"message": map[string]any{"content": tc.reply}, "done_reason": "stop",
				})
			}))
			defer srv.Close()
			store, err := jobs.Open(filepath.Join(t.TempDir(), "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cfg := summarize.Config{URL: srv.URL, MinBytes: 1024, Timeout: tc.timeout, ReadWait: 5 * time.Second}
			if tc.disabled {
				cfg.URL = ""
			}
			d := newDigester(cfg, store)
			if tc.disabled != (d == nil) {
				t.Fatalf("digester = %v for disabled=%v", d, tc.disabled)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if d != nil {
				go d.run(ctx)
			}
			job := seedDone(t, store, tc.input)
			start := time.Now()
			d.enqueue(job) // nil-safe
			// The lookup read_result uses waits for the in-flight attempt.
			a := &App{digest: d}
			dg, ok := a.resultDigest(ctx, job.ID, 0)
			if ok != tc.want {
				t.Fatalf("digest ok = %v, want %v (%+v)", ok, tc.want, dg)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatalf("lookup waited %s", time.Since(start))
			}
			if tc.want {
				if dg.Text != tc.reply || dg.InputBytes != len(tc.input) || dg.Model != summarize.DefaultModel {
					t.Fatalf("digest = %+v", dg)
				}
				// A stale index (a retry appended a newer result) is ignored.
				if _, ok := a.resultDigest(ctx, job.ID, 1); ok {
					t.Fatal("stale digest served")
				}
			}
			after, _, _ := store.Get(ctx, job.ID)
			if string(after.Results) != string(job.Results) {
				t.Fatal("results modified")
			}
		})
	}
}

// TestDigesterEnqueueNeverBlocks: a full queue drops the digest instead
// of blocking the consumer.
func TestDigesterEnqueueNeverBlocks(t *testing.T) {
	store, err := jobs.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	d := newDigester(summarize.Config{URL: "http://127.0.0.1:1", MinBytes: 1}, store) // worker never started
	done := make(chan struct{})
	go func() {
		for i := 0; i < digestQueueCap+5; i++ {
			d.enqueue(jobs.Job{ID: "job_" + string(rune('a'+i)), Results: json.RawMessage(`[{"result":"some text"}]`)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked")
	}
	if len(d.queue) != digestQueueCap {
		t.Fatalf("queue len = %d", len(d.queue))
	}
}
