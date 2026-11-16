package inbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func postThread(t *testing.T, svc *Service, inboxID, typ, thread string, n int) wire.MessageEnvelope {
	t.Helper()
	env, err := svc.Post(context.Background(), inboxID, wire.MessageEnvelope{
		Type: typ, From: "test", ThreadID: thread, Payload: []byte(fmt.Sprintf(`{"n":%d}`, n)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestThreadIDPersistedAndReturned(t *testing.T) {
	ctx := context.Background()
	svc := openTemp(t)
	var hooked wire.MessageEnvelope
	svc.SetPublishHook(func(_ string, env wire.MessageEnvelope) { hooked = env })

	with := postThread(t, svc, "planner:main", "chat.message", "task_A", 1)
	without := postThread(t, svc, "planner:main", "chat.message", "", 2)
	if with.ThreadID != "task_A" || hooked.ThreadID != "" || without.ThreadID != "" {
		t.Fatalf("post echo: %+v / %+v (hook %+v)", with, without, hooked)
	}

	got, ok, err := svc.GetMessage(ctx, with.ID)
	if err != nil || !ok || got.ThreadID != "task_A" {
		t.Errorf("GetMessage = %+v %v %v", got, ok, err)
	}
	got, ok, _ = svc.GetMessage(ctx, without.ID)
	if !ok || got.ThreadID != "" {
		t.Errorf("GetMessage(no thread) = %+v", got)
	}
	recent, _ := svc.Recent(ctx, "planner:main", 10)
	if len(recent) != 2 || recent[0].ThreadID != "task_A" || recent[1].ThreadID != "" {
		t.Errorf("Recent = %+v", recent)
	}
	unacked, _ := svc.Get(ctx, "planner:main", 10, 0)
	if len(unacked) != 2 || unacked[0].ThreadID != "task_A" {
		t.Errorf("Get = %+v", unacked)
	}
	// Empty ThreadID is stored as NULL, not "".
	var nulls int
	svc.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE thread_id IS NULL`).Scan(&nulls)
	if nulls != 1 {
		t.Errorf("NULL thread rows = %d, want 1", nulls)
	}
}

func TestThreadMessages(t *testing.T) {
	ctx := context.Background()
	svc := openTemp(t)
	var want []string
	for i := 0; i < 6; i++ {
		e := postThread(t, svc, "planner:main", "chat.message", "task_A", i)
		want = append(want, e.ID)
		postThread(t, svc, "planner:main", "chat.message", "task_B", i)
	}
	// Threads span inboxes and include acked rows.
	other := postThread(t, svc, "factory:main", "foreman.report", "task_A", 99)
	want = append(want, other.ID)
	if err := svc.Ack(ctx, "planner:main", want[:2]); err != nil {
		t.Fatal(err)
	}

	ids := func(es []wire.MessageEnvelope) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.ID)
		}
		return out
	}
	cases := []struct {
		name   string
		thread string
		limit  int
		want   []string
	}{
		{"all (0)", "task_A", 0, want},
		{"all (negative)", "task_A", -5, want},
		{"last 3, oldest first", "task_A", 3, want[len(want)-3:]},
		{"limit above size", "task_A", 100, want},
		{"unknown", "task_Z", 10, nil},
		{"empty id", "", 10, nil},
	}
	for _, c := range cases {
		got, err := svc.ThreadMessages(ctx, c.thread, c.limit)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if g := ids(got); !reflect.DeepEqual(g, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, g, c.want)
		}
	}
}

func TestSetThread(t *testing.T) {
	ctx := context.Background()
	svc := openTemp(t)
	e := postThread(t, svc, "planner:main", "foreman.report", "", 1)
	if err := svc.Ack(ctx, "planner:main", []string{e.ID}); err != nil {
		t.Fatal(err)
	}
	fired := false
	svc.SetPublishHook(func(string, wire.MessageEnvelope) { fired = true })

	if err := svc.SetThread(ctx, e.ID, "task_A"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := svc.GetMessage(ctx, e.ID); got.ThreadID != "task_A" {
		t.Errorf("after set: %+v", got)
	}
	if th, _ := svc.ThreadMessages(ctx, "task_A", 0); len(th) != 1 || th[0].ID != e.ID {
		t.Errorf("thread = %+v", th)
	}
	// Ack state untouched (still acked: not redelivered).
	if un, _ := svc.Get(ctx, "planner:main", 10, 0); len(un) != 0 {
		t.Errorf("SetThread un-acked the row: %+v", un)
	}
	if err := svc.SetThread(ctx, e.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := svc.GetMessage(ctx, e.ID); got.ThreadID != "" {
		t.Errorf("after clear: %+v", got)
	}
	if err := svc.SetThread(ctx, "msg_nope", "task_A"); !errors.Is(err, ErrNoMessage) {
		t.Errorf("unknown msg: %v", err)
	}
	if fired {
		t.Error("SetThread fired the publish hook")
	}
}

func TestMigrateIdempotentReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	for i := 0; i < 3; i++ {
		svc, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		postThread(t, svc, "planner:main", "chat.message", "task_A", i)
		if th, _ := svc.ThreadMessages(context.Background(), "task_A", 0); len(th) != i+1 {
			t.Fatalf("open %d: thread len %d", i, len(th))
		}
		svc.Close()
	}
}

func TestHTTPPostPersistsThreadID(t *testing.T) {
	svc, h := newTestServer(t, nil)
	// thread_id rides the existing REST envelope POST; older clients that
	// omit it are unaffected (NULL, omitted from JSON).
	rec := doJSON(t, h, "POST", "/v1/inboxes/planner:main/messages", map[string]any{
		"type": "chat.message", "from": "x", "thread_id": "task_H",
	})
	if rec.Code != 201 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var env wire.MessageEnvelope
	decode(t, rec, &env)
	if env.ThreadID != "task_H" {
		t.Fatalf("response %+v", env)
	}
	rec = doJSON(t, h, "POST", "/v1/inboxes/planner:main/messages", map[string]any{"type": "chat.message", "from": "x"})
	if strings.Contains(rec.Body.String(), "thread_id") {
		t.Errorf("thread_id present for unthreaded post: %s", rec.Body.String())
	}
	if th, _ := svc.ThreadMessages(context.Background(), "task_H", 0); len(th) != 1 || th[0].ID != env.ID {
		t.Errorf("thread = %+v", th)
	}
}
