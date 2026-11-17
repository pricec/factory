package factoryapp

import (
	"context"
	"testing"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

// TestCancelPostsJobCancelToForeman: cancelling or superseding a running or
// dispatched job posts job.cancel to the owning foreman's inbox; a pending
// job (no worker anywhere) posts nothing; a late result changes nothing.
func TestCancelPostsJobCancelToForeman(t *testing.T) {
	tests := []struct {
		name       string
		to         string
		running    bool
		pending    bool
		wantCancel bool
	}{
		{"running cancelled", jobs.StateCancelled, true, false, true},
		{"running superseded", jobs.StateSuperseded, true, false, true},
		{"dispatched superseded", jobs.StateSuperseded, false, false, true},
		{"pending cancelled", jobs.StateCancelled, false, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newInternalApp(t)
			ctx := context.Background()
			id := "job_cancel_x"
			switch {
			case tc.running:
				runningJob(t, a, id)
			case tc.pending:
				spec := wire.JobSpec{ID: id, Task: "work", Priority: 5}
				spec.Normalize()
				if err := a.jobs.Create(ctx, spec); err != nil {
					t.Fatal(err)
				}
			default:
				spec := wire.JobSpec{ID: id, Task: "work", Priority: 5}
				spec.Normalize()
				if err := a.jobs.Create(ctx, spec); err != nil {
					t.Fatal(err)
				}
				if err := a.jobs.Dispatch(ctx, id, "fm_1"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.jobs.Cancel(ctx, id, tc.to, "test"); err != nil {
				t.Fatal(err)
			}
			var got []wire.MessageEnvelope
			recent, err := a.inbox.Recent(ctx, "foreman:fm_1", 20)
			if err != nil {
				t.Fatal(err)
			}
			for _, env := range recent {
				if env.Type == wire.TypeJobCancel && env.JobID == id {
					got = append(got, env)
				}
			}
			if tc.wantCancel != (len(got) == 1) {
				t.Fatalf("job.cancel envelopes = %d, want present=%v", len(got), tc.wantCancel)
			}
			// A late result for the terminal job is ignored: no state change.
			sendResult(t, a, wire.JobResult{
				JobID: id, Status: wire.StatusFailed, Summary: "x",
				Error: &wire.JobError{Kind: wire.KindCrash, Message: "killed"},
			})
			job, _, _ := a.jobs.Get(ctx, id)
			if job.State != tc.to {
				t.Errorf("state after late failed result = %s, want %s", job.State, tc.to)
			}
		})
	}
}
