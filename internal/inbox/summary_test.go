package inbox

import (
	"context"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func postEnvelope(t *testing.T, s *Service, inboxID, msgType string) wire.MessageEnvelope {
	t.Helper()
	env, err := s.Post(context.Background(), inboxID, wire.MessageEnvelope{
		Type: msgType,
		From: "test:sender",
	})
	if err != nil {
		t.Fatalf("post %s to %s: %v", msgType, inboxID, err)
	}
	return env
}

func TestSummaries(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	// Empty store: no rows, no error.
	sums, err := s.Summaries(ctx)
	if err != nil {
		t.Fatalf("summaries: %v", err)
	}
	if len(sums) != 0 {
		t.Fatalf("empty store: got %d summaries, want 0", len(sums))
	}

	if _, err := s.Post(ctx, "factory:main", wire.MessageEnvelope{Type: "job.event", From: "foreman:alpha"}); err != nil {
		t.Fatal(err)
	}
	postEnvelope(t, s, "foreman:alpha", "job.request")
	postEnvelope(t, s, "foreman:alpha", "job.request")
	if err := s.Ack(ctx, "foreman:alpha", mustIDs(t, s, "foreman:alpha", 1)); err != nil {
		t.Fatal(err)
	}

	got, err := s.Summaries(ctx)
	if err != nil {
		t.Fatalf("summaries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d summaries, want 2: %+v", len(got), got)
	}
	want := map[string]InboxSummary{
		"factory:main":  {InboxID: "factory:main", Total: 1, Unacked: 1, LastTS: got[0].LastTS},
		"foreman:alpha": {InboxID: "foreman:alpha", Total: 2, Unacked: 1, LastTS: got[1].LastTS},
	}
	for i, id := range []string{"factory:main", "foreman:alpha"} {
		if got[i].InboxID != id {
			t.Errorf("row %d: got id %q, want %q", i, got[i].InboxID, id)
		}
		if got[i] != want[id] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[id])
		}
		if got[i].LastTS == "" {
			t.Errorf("row %d: empty last_ts", i)
		}
	}
}

func TestRecent(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	// Unknown inbox: empty, not an error.
	envs, err := s.Recent(ctx, "foreman:nobody", 10)
	if err != nil {
		t.Fatalf("recent unknown inbox: %v", err)
	}
	if len(envs) != 0 {
		t.Fatalf("unknown inbox: got %d envelopes, want 0", len(envs))
	}

	const inbox = "foreman:alpha"
	var posted []wire.MessageEnvelope
	for i := 0; i < 5; i++ {
		posted = append(posted, postEnvelope(t, s, inbox, "job.request"))
	}
	if err := s.Ack(ctx, inbox, []string{posted[0].ID, posted[4].ID}); err != nil {
		t.Fatal(err)
	}

	// Full history, chronological, acked included.
	envs, err = s.Recent(ctx, inbox, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(envs) != 5 {
		t.Fatalf("got %d envelopes, want 5", len(envs))
	}
	for i := range envs {
		if envs[i].ID != posted[i].ID {
			t.Errorf("row %d: got id %s, want %s", i, envs[i].ID, posted[i].ID)
		}
	}

	// limit keeps the *newest* envelopes.
	envs, err = s.Recent(ctx, inbox, 2)
	if err != nil {
		t.Fatalf("recent limit: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("limit 2: got %d, want 2", len(envs))
	}
	if envs[0].ID != posted[3].ID || envs[1].ID != posted[4].ID {
		t.Errorf("limit 2: got [%s, %s], want newest two [%s, %s]",
			envs[0].ID, envs[1].ID, posted[3].ID, posted[4].ID)
	}
}

func TestRecentWaitIndependence(t *testing.T) {
	// Recent must never consult the waiter machinery: zero limit defaults,
	// and an acked-only inbox still returns history.
	s := openTemp(t)
	ctx := context.Background()
	env := postEnvelope(t, s, "foreman:beta", "job.event")
	if err := s.Ack(ctx, "foreman:beta", []string{env.ID}); err != nil {
		t.Fatal(err)
	}
	envs, err := s.Recent(ctx, "foreman:beta", 0)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("got %d, want 1", len(envs))
	}
}

// mustIDs fetches the first n unacked envelope ids for inboxID.
func mustIDs(t *testing.T, s *Service, inboxID string, n int) []string {
	t.Helper()
	envs, err := s.Get(context.Background(), inboxID, n, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != n {
		t.Fatalf("get %s: got %d, want %d", inboxID, len(envs), n)
	}
	ids := make([]string, n)
	for i, e := range envs {
		ids[i] = e.ID
	}
	return ids
}
