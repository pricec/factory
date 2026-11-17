package foremanapp

import (
	"context"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

func newCancelApp() *App {
	return &App{cancels: map[string]chan struct{}{}, cancelled: map[string]bool{}}
}

// TestJobCancelEnvelopeStopsWorker: a job.cancel envelope in a batch closes
// the live worker's cancel channel and is acked; it is idempotent.
func TestJobCancelEnvelopeStopsWorker(t *testing.T) {
	a := newCancelApp()
	ch := a.registerCancel("job_1")
	var acked []string
	ack := func(_ context.Context, ids []string) error { acked = append(acked, ids...); return nil }
	env := wire.MessageEnvelope{ID: "m1", Type: wire.TypeJobCancel, JobID: "job_1"}
	for range 2 {
		if err := a.handleBatch(context.Background(), []wire.MessageEnvelope{env}, ack); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("cancel channel not closed")
	}
	if len(acked) != 2 {
		t.Errorf("acked = %v, want both deliveries acked", acked)
	}
	// Cancel for a job with no worker is remembered and a later spawn for it
	// starts already-cancelled (offline-foreman path).
	a.cancelWorker("job_2")
	if !a.jobCancelled("job_2") {
		t.Error("job_2 not remembered as cancelled")
	}
	select {
	case <-a.registerCancel("job_2"):
	default:
		t.Error("channel for an already-cancelled job must be closed")
	}
}

// TestJobRequestForCancelledJobDropped: a job.request arriving after the
// job's cancel is acked and never reaches the router.
func TestJobRequestForCancelledJobDropped(t *testing.T) {
	a := newCancelApp()
	a.cancelWorker("job_3")
	var acked []string
	ack := func(_ context.Context, ids []string) error { acked = append(acked, ids...); return nil }
	env := wire.MessageEnvelope{ID: "r1", Type: "job.request", JobID: "job_3"}
	// a.router is nil: reaching the router would panic.
	if err := a.handleBatch(context.Background(), []wire.MessageEnvelope{env}, ack); err != nil {
		t.Fatal(err)
	}
	if len(acked) != 1 || acked[0] != "r1" {
		t.Errorf("acked = %v, want [r1]", acked)
	}
}
