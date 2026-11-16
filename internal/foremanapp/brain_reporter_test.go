package foremanapp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

// stubBrain fails every session with err (nil err = success).
type stubBrain struct{ err error }

func (b *stubBrain) Handle(context.Context, wire.MessageEnvelope) error { return b.err }

// recordPoster records every PostToParent envelope (routing.Poster stub).
type recordPoster struct {
	mu   sync.Mutex
	envs []wire.MessageEnvelope
}

func (p *recordPoster) PostToParent(_ context.Context, env wire.MessageEnvelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.envs = append(p.envs, env)
	return nil
}

func (p *recordPoster) recorded() []wire.MessageEnvelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]wire.MessageEnvelope(nil), p.envs...)
}

// TestBrainReporterPostsFailure: a failed non-question brain session
// posts a brain.failed envelope upstream (spec §3.2, 2026-11-15
// amendment) - the router alone logs and acks, which silently dead-ends
// the delegation; job.question failures stay exempt (ask_parent's
// default answer is the pinned fallback) and successes stay silent.
func TestBrainReporterPostsFailure(t *testing.T) {
	cases := []struct {
		name       string
		typ        string
		brainErr   error
		wantPosted bool
	}{
		{"failed chat nudge reports upstream", "chat.message", errors.New("brain session exceeded 5m0s"), true},
		{"failed brain.wake reports upstream", "brain.wake", errors.New("boom"), true},
		{"failed job.question is exempt", "job.question", errors.New("boom"), false},
		{"successful session stays silent", "chat.message", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poster := &recordPoster{}
			reporter := &brainReporter{inner: &stubBrain{err: tc.brainErr}, poster: poster, from: "foreman:alpha"}
			env := wire.MessageEnvelope{ID: "msg_nudge", Type: tc.typ, JobID: "job_x", From: "factory:planner"}

			got := reporter.Handle(context.Background(), env)
			if !errors.Is(got, tc.brainErr) {
				t.Errorf("Handle error = %v, want the brain's own %v unchanged", got, tc.brainErr)
			}

			posted := poster.recorded()
			if !tc.wantPosted {
				if len(posted) != 0 {
					t.Fatalf("poster received %+v, want nothing", posted)
				}
				return
			}
			if len(posted) != 1 {
				t.Fatalf("poster received %d envelopes, want 1", len(posted))
			}
			out := posted[0]
			if out.Type != BrainFailedType || out.From != "foreman:alpha" || out.JobID != "job_x" {
				t.Errorf("brain.failed envelope = type %q from %q job %q", out.Type, out.From, out.JobID)
			}
			var p struct {
				EnvelopeID   string `json:"envelope_id"`
				EnvelopeType string `json:"envelope_type"`
				Error        string `json:"error"`
			}
			if err := json.Unmarshal(out.Payload, &p); err != nil {
				t.Fatalf("payload %s: %v", out.Payload, err)
			}
			if p.EnvelopeID != "msg_nudge" || p.EnvelopeType != tc.typ {
				t.Errorf("payload identifies %+v, want the failed %s msg_nudge", p, tc.typ)
			}
			if p.Error == "" {
				t.Error("payload carries no error text")
			}
		})
	}
}
