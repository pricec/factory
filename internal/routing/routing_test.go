package routing

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

type recordPoster struct {
	envs []wire.MessageEnvelope
	err  error
}

func (p *recordPoster) PostToParent(ctx context.Context, env wire.MessageEnvelope) error {
	p.envs = append(p.envs, env)
	return p.err
}

type recordSpawner struct {
	specs []wire.JobSpec
	err   error
}

func (s *recordSpawner) Spawn(ctx context.Context, spec wire.JobSpec) error {
	s.specs = append(s.specs, spec)
	return s.err
}

func validSpec(t *testing.T) wire.JobSpec {
	t.Helper()
	spec := wire.NewJobSpec("summarize the logs", "factory")
	spec.Payload = json.RawMessage(`{"path":"/var/log/app.log"}`)
	if err := spec.Validate(); err != nil {
		t.Fatalf("spec invalid: %v", err)
	}
	return spec
}

func env(t *testing.T, typ string, jobID string, payload json.RawMessage) wire.MessageEnvelope {
	t.Helper()
	env := wire.MessageEnvelope{
		ID:      wire.NewMessageID(),
		Type:    typ,
		JobID:   jobID,
		From:    "foreman:alpha",
		Payload: payload,
		TS:      time.Now().UTC().Format(time.RFC3339),
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("envelope invalid: %v", err)
	}
	return env
}

func TestRuleTable(t *testing.T) {
	spec := validSpec(t)
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		env        wire.MessageEnvelope
		wantAction func(t *testing.T, env wire.MessageEnvelope, brain *RecordBrain, poster *recordPoster, spawner *recordSpawner, err error)
	}{
		{
			name: "job.request spawns worker with the decoded spec",
			env:  env(t, "job.request", spec.ID, specJSON),
			wantAction: func(t *testing.T, _ wire.MessageEnvelope, brain *RecordBrain, poster *recordPoster, spawner *recordSpawner, err error) {
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if got := len(spawner.specs); got != 1 {
					t.Fatalf("spawner calls = %d, want 1", got)
				}
				if !reflect.DeepEqual(spawner.specs[0], spec) {
					t.Errorf("spawned spec = %+v, want %+v", spawner.specs[0], spec)
				}
				if brain.Count() != 0 || len(poster.envs) != 0 {
					t.Errorf("job.request must not touch brain or poster")
				}
			},
		},
		{
			name: "job.request with invalid payload logs and returns nil",
			env:  env(t, "job.request", spec.ID, json.RawMessage(`{"task":123,"timeout_s":"nope"}`)),
			wantAction: func(t *testing.T, _ wire.MessageEnvelope, brain *RecordBrain, poster *recordPoster, spawner *recordSpawner, err error) {
				if err != nil {
					t.Fatalf("Handle: %v (want nil; never crash the loop)", err)
				}
				if len(spawner.specs) != 0 {
					t.Errorf("spawner calls = %d, want 0", len(spawner.specs))
				}
			},
		},
		{
			name: "job.result forwards byte-identical, zero brain calls",
			env: env(t, "job.result", spec.ID, json.RawMessage(
				`{"job_id":"`+spec.ID+`","status":"done","summary":"ok","result":{"x":1},"usage":{"input_tokens":10,"output_tokens":5,"cost_usd":0.01}}`)),
			wantAction: func(t *testing.T, env wire.MessageEnvelope, brain *RecordBrain, poster *recordPoster, spawner *recordSpawner, err error) {
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if got := len(poster.envs); got != 1 {
					t.Fatalf("poster calls = %d, want 1", got)
				}
				if !reflect.DeepEqual(poster.envs[0], env) {
					t.Errorf("forwarded envelope = %+v, want %+v", poster.envs[0], env)
				}
				if brain.Count() != 0 {
					t.Errorf("brain calls = %d, want 0 (ZERO LLM calls on the hot path)", brain.Count())
				}
				if len(spawner.specs) != 0 {
					t.Errorf("spawner calls = %d, want 0", len(spawner.specs))
				}
			},
		},
		{
			name: "job.event drops",
			env:  env(t, "job.event", spec.ID, json.RawMessage(`{"event":"accepted"}`)),
			wantAction: func(t *testing.T, _ wire.MessageEnvelope, brain *RecordBrain, poster *recordPoster, spawner *recordSpawner, err error) {
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if brain.Count() != 0 || len(poster.envs) != 0 || len(spawner.specs) != 0 {
					t.Errorf("job.event must call nothing")
				}
			},
		},
		{
			name: "job.question goes to brain",
			env:  env(t, "job.question", spec.ID, json.RawMessage(`{"question":"continue?"}`)),
			wantAction: func(t *testing.T, _ wire.MessageEnvelope, brain *RecordBrain, poster *recordPoster, spawner *recordSpawner, err error) {
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if brain.Count() != 1 {
					t.Fatalf("brain calls = %d, want 1", brain.Count())
				}
				if len(poster.envs) != 0 || len(spawner.specs) != 0 {
					t.Errorf("job.question must not touch poster or spawner")
				}
			},
		},
		{
			name: "brain.wake goes to brain",
			env:  env(t, "brain.wake", "", json.RawMessage(`{"why":"cron"}`)),
			wantAction: func(t *testing.T, _ wire.MessageEnvelope, brain *RecordBrain, poster *recordPoster, spawner *recordSpawner, err error) {
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if brain.Count() != 1 {
					t.Fatalf("brain calls = %d, want 1", brain.Count())
				}
			},
		},
		{
			name: "unknown type goes to brain",
			env:  env(t, "chat.tumbleweed", "", json.RawMessage(`{"any":"thing"}`)),
			wantAction: func(t *testing.T, _ wire.MessageEnvelope, brain *RecordBrain, poster *recordPoster, spawner *recordSpawner, err error) {
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if brain.Count() != 1 {
					t.Fatalf("brain calls = %d, want 1", brain.Count())
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			brain := NewRecordBrain()
			poster := &recordPoster{}
			spawner := &recordSpawner{}
			r := &Router{Brain: brain, Poster: poster, Spawner: spawner}
			err := r.Handle(context.Background(), tt.env)
			tt.wantAction(t, tt.env, brain, poster, spawner, err)
		})
	}
}

func TestForwardFidelity(t *testing.T) {
	// Unicode + nested objects in the payload; the poster must receive an
	// envelope equal on ALL fields (ID, TS, From included) and a payload that
	// marshals to identical bytes.
	raw := json.RawMessage(`{"summary":"coöl ✓ result","nested":{"α":[1,2,{"b":true}],"s":"ü"},"usage":{"input_tokens":10,"output_tokens":5,"cost_usd":0.01}}`)
	env := env(t, "job.result", "job_01HZZZZZZZZZZZZZZZZZZZZZZZ", raw)

	brain := NewRecordBrain()
	poster := &recordPoster{}
	r := &Router{Brain: brain, Poster: poster, Spawner: &recordSpawner{}}
	if err := r.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if brain.Count() != 0 {
		t.Fatalf("brain calls = %d, want 0", brain.Count())
	}
	if len(poster.envs) != 1 {
		t.Fatalf("poster calls = %d, want 1", len(poster.envs))
	}
	got := poster.envs[0]
	if !reflect.DeepEqual(got, env) {
		t.Errorf("forwarded envelope = %+v, want %+v", got, env)
	}
	if string(got.Payload) != string(env.Payload) {
		t.Errorf("payload bytes = %s, want %s", got.Payload, env.Payload)
	}
	// Byte-identical end to end: marshal both envelopes and compare.
	gb, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wb, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if string(gb) != string(wb) {
		t.Errorf("marshaled bytes differ:\n got %s\nwant %s", gb, wb)
	}
}

func TestQuestionTimeout(t *testing.T) {
	spec := validSpec(t)
	q := env(t, "job.question", spec.ID, json.RawMessage(`{"question":"continue?"}`))

	// A brain that ignores the context: it must hit the SLA and the loop must
	// continue (Handle returns nil).
	brainErr := errors.New("simulated LLM failure")
	brain := NewRecordBrain()
	brain.Err = brainErr

	r := &Router{Brain: brain, Poster: &recordPoster{}, Spawner: &recordSpawner{}, QuestionTimeout: 50 * time.Millisecond}
	if err := r.Handle(context.Background(), q); err != nil {
		t.Fatalf("Handle: %v (brain error must not surface)", err)
	}
	if brain.Count() != 1 {
		t.Fatalf("brain calls = %d, want 1", brain.Count())
	}

	// A brain that hangs: the SLA context must cut it off, Handle returns nil.
	hang := &hangingBrain{}
	r2 := &Router{Brain: hang, Poster: &recordPoster{}, Spawner: &recordSpawner{}, QuestionTimeout: 50 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- r2.Handle(context.Background(), q) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Handle blocked past the question SLA")
	}
	if !hang.awoke() {
		t.Fatal("brain was never invoked")
	}

	// Zero QuestionTimeout means the 60s default.
	r3 := &Router{Brain: NewRecordBrain(), Poster: &recordPoster{}, Spawner: &recordSpawner{}}
	if r3.questionSla() != QuestionTimeout || r3.questionSla() != 60*time.Second {
		t.Errorf("default SLA = %v, want 60s", r3.questionSla())
	}
}

type hangingBrain struct {
	entered chan struct{}
}

func (b *hangingBrain) Handle(ctx context.Context, env wire.MessageEnvelope) error {
	if b.entered == nil {
		b.entered = make(chan struct{})
	}
	close(b.entered)
	select {
	case <-ctx.Done(): // SLA expired; default answer is ask_parent's job
	case <-time.After(10 * time.Second):
	}
	return ctx.Err()
}

func (b *hangingBrain) awoke() bool {
	if b.entered == nil {
		return false
	}
	select {
	case <-b.entered:
		return true
	default:
		return false
	}
}

func TestRouteTable(t *testing.T) {
	tests := []struct {
		msgType string
		want    Action
	}{
		{"job.request", ActionSpawnWorker},
		{"job.result", ActionForward},
		{"job.event", ActionDrop},
		{"job.question", ActionBrain},
		{"brain.wake", ActionBrain},
		{"anything.else", ActionBrain},
		{"", ActionBrain},
	}
	for _, tt := range tests {
		if got := route(tt.msgType); got != tt.want {
			t.Errorf("route(%q) = %v, want %v", tt.msgType, got, tt.want)
		}
	}
}

func TestDelegatesToBrain(t *testing.T) {
	// DelegatesToBrain must agree with the rule table exactly: it is the
	// consume loop's partition signal (spec §3.3, 2026-11-15 amendment).
	tests := []struct {
		msgType string
		want    bool
	}{
		{"job.request", false},
		{"job.result", false},
		{"job.event", false},
		{"job.question", true},
		{"brain.wake", true},
		{"chat.message", true},
		{"anything.else", true},
		{"", true},
	}
	for _, tt := range tests {
		if got := DelegatesToBrain(tt.msgType); got != tt.want {
			t.Errorf("DelegatesToBrain(%q) = %v, want %v", tt.msgType, got, tt.want)
		}
		if got := route(tt.msgType) == ActionBrain; got != tt.want {
			t.Errorf("route(%q) disagrees with DelegatesToBrain", tt.msgType)
		}
	}
}

func TestRecordBrainConcurrency(t *testing.T) {
	brain := NewRecordBrain()
	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			brain.Handle(context.Background(), wire.MessageEnvelope{Type: "brain.wake"})
		}(i)
	}
	wg.Wait()
	if got := brain.Count(); got != n {
		t.Errorf("recorded calls = %d, want %d", got, n)
	}
}
