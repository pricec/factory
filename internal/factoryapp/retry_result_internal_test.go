package factoryapp

import (
	"context"
	"errors"
	"testing"

	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// A retry re-dispatches the SAME job id. Dispatching it must free the
// previous attempt's job.result slot in factory:main, or the retry attempt's
// result is rejected as a duplicate (409) and the job stays running until
// reconcile reaps it ("foreman X does not claim it").
func TestNotifyDispatchedReleasesPriorAttemptResult(t *testing.T) {
	a := newInternalApp(t)
	ctx := context.Background()
	id := "job_retry_x"
	spec := wire.JobSpec{ID: id, Task: "work", Priority: 5}
	spec.Normalize()
	if err := a.jobs.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := a.jobs.Dispatch(ctx, id, "fm_1"); err != nil {
		t.Fatal(err)
	}

	// Attempt 0's failed result is stored and consumed (acked).
	res := wire.MessageEnvelope{Type: "job.result", JobID: id, From: "foreman:fm_1", Payload: []byte(`{"status":"failed"}`)}
	first, err := a.inbox.Post(ctx, FactoryMainInbox, res)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.inbox.Ack(ctx, FactoryMainInbox, []string{first.ID}); err != nil {
		t.Fatal(err)
	}
	var dup *inbox.DuplicateError
	if _, err := a.inbox.Post(ctx, FactoryMainInbox, res); !errors.As(err, &dup) {
		t.Fatalf("before re-dispatch: got %v, want duplicate", err)
	}

	job, ok, err := a.jobs.Get(ctx, id)
	if err != nil || !ok {
		t.Fatalf("get: %v %v", ok, err)
	}
	if err := a.notifyDispatched(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := a.inbox.Post(ctx, FactoryMainInbox, res); err != nil {
		t.Fatalf("retry attempt result rejected after re-dispatch: %v", err)
	}
}

// A worker that finished posts its job.result and exits, so the foreman's
// capabilities push no longer lists it as active. If the factory has not
// consumed that result yet (busy consumer, restart/deploy), reconcile must
// not reap the job as "does not claim it": the consumer will settle it.
func TestReconcileSparesJobWithUnconsumedResult(t *testing.T) {
	a := newInternalApp(t)
	ctx := context.Background()
	id := "job_inflight_x"
	spec := wire.JobSpec{ID: id, Task: "work", Priority: 5}
	spec.Normalize()
	if err := a.jobs.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := a.jobs.Dispatch(ctx, id, "fm_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.jobs.DB().ExecContext(ctx,
		`UPDATE jobs SET state = 'running', updated_at = '2000-01-01T00:00:00Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	res := wire.MessageEnvelope{Type: "job.result", JobID: id, From: "foreman:fm_1", Payload: []byte(`{"status":"done"}`)}
	posted, err := a.inbox.Post(ctx, FactoryMainInbox, res)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.reconcileForeman(ctx, "fm_1", nil); err != nil {
		t.Fatal(err)
	}
	job, _, _ := a.jobs.Get(ctx, id)
	if job.State != jobs.StateRunning {
		t.Fatalf("state with unconsumed result = %s, want running", job.State)
	}

	// Once consumed (acked) and the job is somehow still running, it is lost.
	if err := a.inbox.Ack(ctx, FactoryMainInbox, []string{posted.ID}); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileForeman(ctx, "fm_1", nil); err != nil {
		t.Fatal(err)
	}
	job, _, _ = a.jobs.Get(ctx, id)
	if job.State == jobs.StateRunning {
		t.Fatalf("state after result consumed = running, want reaped")
	}
}
