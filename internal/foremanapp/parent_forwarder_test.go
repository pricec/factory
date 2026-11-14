package foremanapp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pricec/factory/internal/foremanapp"
	"github.com/pricec/factory/internal/wire"
)

// TestParentForwarderDrainsLocalFactoryMain (spec §7.3): the foreman's
// local factory:main is a staging inbox - the brain's post_to_factory
// output lands there, and the parent forwarder loop must deliver it to
// the factory-side factory:main and ack it locally. Without the loop the
// staging copy is a dead end: brain replies never reached the factory.
func TestParentForwarderDrainsLocalFactoryMain(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	fEnv := newForeman(t, fac, token)

	// Post into the foreman's LOCAL factory:main exactly the way the
	// brain's post_to_factory tool does (mcpforeman writes the envelope
	// into the foreman's own inbox service under the parent inbox id).
	env := wire.MessageEnvelope{
		Type:    "brain.reply",
		From:    "foreman:alpha",
		Payload: json.RawMessage(`{"text":"foreman brain checked in"}`),
	}
	posted, err := fEnv.a.Inbox().Post(fEnv.ctx, foremanapp.FactoryMainInbox, env)
	if err != nil {
		t.Fatalf("post to local factory:main: %v", err)
	}

	// The forwarder delivers it to the factory-side factory:main (the
	// factory assigns its own message id on the REST post, so match on
	// content). The factory's consumer may have already acked it
	// (brain.reply has no factory-side handler), so poll the replayable
	// Recent history, not the unacked snapshot.
	delivered := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		envs, err := fac.a.Inbox().Recent(ctx, "factory:main", 50)
		if err != nil {
			t.Fatalf("factory inbox recent: %v", err)
		}
		for _, e := range envs {
			if e.Type == "brain.reply" && e.From == "foreman:alpha" && string(e.Payload) == `{"text":"foreman brain checked in"}` {
				return true
			}
		}
		return false
	}
	drained := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		envs, err := fEnv.a.Inbox().Get(ctx, foremanapp.FactoryMainInbox, 50, 0)
		if err != nil {
			t.Fatalf("local inbox get: %v", err)
		}
		return len(envs) == 0
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if delivered() && drained() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("parent forwarder did not deliver + ack envelope %s (delivered=%v drained=%v)",
		posted.ID, delivered(), drained())
}
