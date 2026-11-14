package routing

import (
	"context"
	"sync"

	"github.com/pricec/factory/internal/wire"
)

// RecordBrain is the stub Brain (spec §3.3): it records every Handle
// call and returns the configured error. #18's tests reuse it; the real
// LLM brain replaces it at the integration seam.
type RecordBrain struct {
	mu    sync.Mutex
	calls []wire.MessageEnvelope
	Err   error
}

// NewRecordBrain returns a RecordBrain that succeeds every call.
func NewRecordBrain() *RecordBrain { return &RecordBrain{} }

// Handle records env and returns b.Err.
func (b *RecordBrain) Handle(ctx context.Context, env wire.MessageEnvelope) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, env)
	return b.Err
}

// Calls returns a copy of the recorded envelopes.
func (b *RecordBrain) Calls() []wire.MessageEnvelope {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]wire.MessageEnvelope, len(b.calls))
	copy(out, b.calls)
	return out
}

// Count returns the number of recorded calls.
func (b *RecordBrain) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}
