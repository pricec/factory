package inbox

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

func testEnvelope(t *testing.T, msgType, jobID, payload string) wire.MessageEnvelope {
	t.Helper()
	return wire.MessageEnvelope{
		Type:    msgType,
		JobID:   jobID,
		From:    "test:sender",
		Payload: []byte(payload),
	}
}

func TestPostAndFIFOOrder(t *testing.T) {
	svc := openTemp(t)
	defer svc.Close()

	want := []string{"alpha", "bravo", "charlie"}
	for i, from := range want {
		env := testEnvelope(t, "job.event", "", `{"n":`+string(rune('0'+i))+`}`)
		env.From = from
		full, err := svc.Post(context.Background(), "inbox:main", env)
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		if full.ID == "" || full.TS == "" {
			t.Fatalf("post %d: server did not assign id/ts", i)
		}
	}

	envs, err := svc.Get(context.Background(), "inbox:main", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 3 {
		t.Fatalf("got %d envelopes, want 3", len(envs))
	}
	for i, env := range envs {
		if env.From != want[i] {
			t.Errorf("envs[%d].From = %q, want %q (FIFO violated)", i, env.From, want[i])
		}
	}
}

func TestAckIdempotentAndExcludes(t *testing.T) {
	svc := openTemp(t)
	defer svc.Close()

	ctx := context.Background()
	ids := make([]string, 2)
	for i := 0; i < 2; i++ {
		full, err := svc.Post(ctx, "inbox:main", testEnvelope(t, "job.event", "", `{"n":`+string(rune('0'+i))+`}`))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = full.ID
	}

	if err := svc.Ack(ctx, "inbox:main", []string{ids[0]}); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// Double-ack is not an error (spec §4.1).
	if err := svc.Ack(ctx, "inbox:main", []string{ids[0]}); err != nil {
		t.Fatalf("double-ack: %v", err)
	}
	// Unknown id is not an error either.
	if err := svc.Ack(ctx, "inbox:main", []string{"msg_nonexistent"}); err != nil {
		t.Fatalf("ack unknown: %v", err)
	}

	envs, err := svc.Get(ctx, "inbox:main", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 || envs[0].ID != ids[1] {
		t.Fatalf("after acking %s, got %v; want only %s", ids[0], envs, ids[1])
	}
}

func TestRestartReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	ctx := context.Background()

	svc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := svc.Post(ctx, "inbox:main", testEnvelope(t, "job.event", "", `{"n":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Post(ctx, "inbox:main", testEnvelope(t, "job.event", "", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Ack(ctx, "inbox:main", []string{kept.ID}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	svc2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()

	// Unacked messages are redelivered; acked ones are not.
	envs, err := svc2.Get(ctx, "inbox:main", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 || envs[0].ID == kept.ID {
		t.Fatalf("restart replay: got %v, want only the unacked envelope", envs)
	}

	// The acked one is still retrievable via the replay path.
	stored, ok, err := svc2.GetMessage(ctx, kept.ID)
	if err != nil || !ok {
		t.Fatalf("GetMessage after restart: ok=%v err=%v", ok, err)
	}
	if stored.ID != kept.ID || string(stored.Payload) != `{"n":0}` {
		t.Fatalf("replayed envelope mismatch: %+v", stored)
	}
}

func TestDuplicateJobResultDedupe(t *testing.T) {
	svc := openTemp(t)
	defer svc.Close()

	ctx := context.Background()
	first, err := svc.Post(ctx, "inbox:main", testEnvelope(t, "job.result", "job_abc", `{"status":"done"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Post(ctx, "inbox:main", testEnvelope(t, "job.result", "job_abc", `{"status":"done"}`))
	var dup *DuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("second post: got %v, want *DuplicateError", err)
	}
	if dup.OriginalID != first.ID {
		t.Errorf("dup.OriginalID = %q, want %q", dup.OriginalID, first.ID)
	}

	// Non-job.result types with the same job_id are not deduped.
	for i := 0; i < 2; i++ {
		if _, err := svc.Post(ctx, "inbox:main", testEnvelope(t, "job.event", "job_abc", "{}")); err != nil {
			t.Fatalf("non-result post %d: %v", i, err)
		}
	}
	envs, err := svc.Get(ctx, "inbox:main", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 3 { // 1 result + 2 events
		t.Fatalf("got %d envelopes, want 3", len(envs))
	}
}

func TestLongPollWakeOnPost(t *testing.T) {
	svc := openTemp(t)
	defer svc.Close()

	type res struct {
		envs []wire.MessageEnvelope
		err  error
	}
	done := make(chan res, 1)
	go func() {
		envs, err := svc.Get(context.Background(), "inbox:main", 50, 5*time.Second)
		done <- res{envs, err}
	}()

	time.Sleep(100 * time.Millisecond) // let the waiter register
	if _, err := svc.Post(context.Background(), "inbox:main", testEnvelope(t, "job.event", "", "{}")); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if len(r.envs) != 1 {
			t.Fatalf("woke with %d envelopes, want 1", len(r.envs))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long-poll GET did not wake on POST within 2s")
	}
}

func TestGetEmptyReturnsImmediatelyWithoutWait(t *testing.T) {
	svc := openTemp(t)
	defer svc.Close()

	start := time.Now()
	envs, err := svc.Get(context.Background(), "inbox:main", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 0 {
		t.Fatalf("got %d envelopes, want 0", len(envs))
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("empty no-wait GET took %v", elapsed)
	}
}

func TestLongPollTimeoutReturnsEmpty(t *testing.T) {
	svc := openTemp(t)
	defer svc.Close()

	start := time.Now()
	envs, err := svc.Get(context.Background(), "inbox:main", 50, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 0 {
		t.Fatalf("got %d envelopes, want 0", len(envs))
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("wait honored: returned after %v", elapsed)
	}
}

func TestCloseWakesWaiters(t *testing.T) {
	svc := openTemp(t)

	done := make(chan error, 1)
	go func() {
		_, err := svc.Get(context.Background(), "inbox:main", 50, 60*time.Second)
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("waiter returned nil error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken by Close within 2s")
	}
}

func TestGetMessageMissing(t *testing.T) {
	svc := openTemp(t)
	defer svc.Close()

	_, ok, err := svc.GetMessage(context.Background(), "msg_missing")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("ok=true for missing message")
	}
}

func openTemp(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatalf("open inbox: %v", err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc
}
