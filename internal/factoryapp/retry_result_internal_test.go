package factoryapp

import (
	"context"
	"errors"
	"testing"

	"github.com/pricec/factory/internal/inbox"
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
