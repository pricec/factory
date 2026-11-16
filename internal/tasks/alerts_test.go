package tasks_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/pricec/factory/internal/tasks"
)

func TestCreateGetAlert(t *testing.T) {
	ctx := context.Background()
	_, st, clk := newStore(t)
	exp := clk.Now().Add(4 * time.Hour)
	a, err := st.CreateAlert(ctx, tasks.NewAlert{Priority: 2, Title: "t", Body: "b", Source: "factory", ExpiresAt: &exp, TaskID: "task_1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.State != tasks.AlertOpen || a.Priority != 2 || a.Title != "t" || a.Body != "b" || a.Source != "factory" ||
		!a.CreatedAt.Equal(clk.Now()) || a.ExpiresAt == nil || !a.ExpiresAt.Equal(exp) || a.TaskID != "task_1" || a.AckedAt != nil {
		t.Fatalf("alert = %+v", a)
	}
	got, err := st.GetAlert(ctx, a.ID)
	if err != nil || !reflect.DeepEqual(got, a) {
		t.Fatalf("get = %+v %v", got, err)
	}
	never, err := st.CreateAlert(ctx, tasks.NewAlert{Priority: 0, Title: "p0"})
	if err != nil || never.ExpiresAt != nil || never.TaskID != "" {
		t.Fatalf("never = %+v %v", never, err)
	}
	if _, err := st.GetAlert(ctx, "alert_nope"); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := st.CreateAlert(ctx, tasks.NewAlert{Title: " "}); !errors.Is(err, tasks.ErrInvalid) {
		t.Errorf("empty title: %v", err)
	}
}

func TestAlertJSONShape(t *testing.T) {
	ctx := context.Background()
	_, st, clk := newStore(t)
	a, _ := st.CreateAlert(ctx, tasks.NewAlert{Priority: 1, Title: "t", Source: "s"})
	b, _ := json.Marshal(a)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"id", "priority", "title", "body", "source", "created_at", "expires_at", "state", "task_id"}
	if len(m) != len(wantKeys) {
		t.Errorf("keys = %v", m)
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in %s", k, b)
		}
	}
	if m["expires_at"] != nil || m["task_id"] != nil {
		t.Errorf("unset expires_at/task_id must be null: %s", b)
	}
	exp := clk.Now().Add(time.Hour)
	a2, _ := st.CreateAlert(ctx, tasks.NewAlert{Priority: 3, Title: "x", ExpiresAt: &exp, TaskID: "task_9"})
	b2, _ := json.Marshal(a2)
	var back tasks.Alert
	if err := json.Unmarshal(b2, &back); err != nil {
		t.Fatal(err)
	}
	back.AckedAt = a2.AckedAt
	if back.ID != a2.ID || back.TaskID != "task_9" || !back.ExpiresAt.Equal(exp) || !back.CreatedAt.Equal(a2.CreatedAt) {
		t.Errorf("round trip %+v vs %+v", back, a2)
	}
}

func TestListAlertsOrderAndFilter(t *testing.T) {
	ctx := context.Background()
	_, st, clk := newStore(t)
	mk := func(p int) tasks.Alert {
		clk.Add(time.Second)
		a, err := st.CreateAlert(ctx, tasks.NewAlert{Priority: p, Title: "x"})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	p2old, p0, p2new, p1 := mk(2), mk(0), mk(2), mk(1)
	if _, err := st.TransitionAlert(ctx, p1.ID, nil, tasks.AlertAcked); err != nil {
		t.Fatal(err)
	}
	ids := func(as []tasks.Alert) []string {
		var out []string
		for _, a := range as {
			out = append(out, a.ID)
		}
		return out
	}
	all, _ := st.ListAlerts(ctx, tasks.AlertFilter{})
	if got, want := ids(all), []string{p0.ID, p1.ID, p2new.ID, p2old.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("all = %v, want %v", got, want)
	}
	open, _ := st.ListAlerts(ctx, tasks.AlertFilter{States: []string{tasks.AlertOpen}})
	if got, want := ids(open), []string{p0.ID, p2new.ID, p2old.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("open = %v, want %v", got, want)
	}
	lim, _ := st.ListAlerts(ctx, tasks.AlertFilter{States: []string{tasks.AlertOpen, tasks.AlertAcked}, Limit: 2})
	if got, want := ids(lim), []string{p0.ID, p1.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("limit = %v, want %v", got, want)
	}
	if _, err := st.ListAlerts(ctx, tasks.AlertFilter{States: []string{"bogus"}}); !errors.Is(err, tasks.ErrInvalid) {
		t.Errorf("bad state: %v", err)
	}
}

func TestTransitionAlert(t *testing.T) {
	ctx := context.Background()
	_, st, clk := newStore(t)
	a, _ := st.CreateAlert(ctx, tasks.NewAlert{Priority: 1, Title: "x"})
	open := []string{tasks.AlertOpen}

	clk.Add(time.Minute)
	acked, err := st.TransitionAlert(ctx, a.ID, open, tasks.AlertAcked)
	if err != nil || acked.State != tasks.AlertAcked || acked.AckedAt == nil || !acked.AckedAt.Equal(clk.Now()) {
		t.Fatalf("ack: %+v %v", acked, err)
	}
	// Second ack from "open" conflicts and returns the current alert.
	cur, err := st.TransitionAlert(ctx, a.ID, open, tasks.AlertAcked)
	if !errors.Is(err, tasks.ErrStateConflict) || cur.State != tasks.AlertAcked || cur.ID != a.ID {
		t.Fatalf("re-ack: %+v %v", cur, err)
	}
	// Unconditional set works from any state and leaves acked_at alone.
	rep, err := st.TransitionAlert(ctx, a.ID, nil, tasks.AlertReplied)
	if err != nil || rep.State != tasks.AlertReplied || rep.AckedAt == nil {
		t.Fatalf("reply: %+v %v", rep, err)
	}
	if _, err := st.TransitionAlert(ctx, "alert_nope", open, tasks.AlertAcked); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := st.TransitionAlert(ctx, a.ID, nil, "bogus"); !errors.Is(err, tasks.ErrInvalid) {
		t.Errorf("bad to: %v", err)
	}
	if _, err := st.TransitionAlert(ctx, a.ID, []string{"bogus"}, tasks.AlertOpen); !errors.Is(err, tasks.ErrInvalid) {
		t.Errorf("bad from: %v", err)
	}
}

func TestSetAlertTask(t *testing.T) {
	ctx := context.Background()
	_, st, _ := newStore(t)
	a, _ := st.CreateAlert(ctx, tasks.NewAlert{Priority: 1, Title: "x"})
	got, err := st.SetAlertTask(ctx, a.ID, "task_7")
	if err != nil || got.TaskID != "task_7" || got.State != tasks.AlertOpen {
		t.Fatalf("set: %+v %v", got, err)
	}
	got, err = st.SetAlertTask(ctx, a.ID, "")
	if err != nil || got.TaskID != "" {
		t.Fatalf("clear: %+v %v", got, err)
	}
	if _, err := st.SetAlertTask(ctx, "alert_nope", "task_7"); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}

func TestExpireAlerts(t *testing.T) {
	ctx := context.Background()
	_, st, clk := newStore(t)
	t0 := clk.Now()
	at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }
	soon, _ := st.CreateAlert(ctx, tasks.NewAlert{Priority: 3, Title: "soon", ExpiresAt: at(time.Hour)})
	later, _ := st.CreateAlert(ctx, tasks.NewAlert{Priority: 2, Title: "later", ExpiresAt: at(24 * time.Hour)})
	never, _ := st.CreateAlert(ctx, tasks.NewAlert{Priority: 0, Title: "never"})
	ackedSoon, _ := st.CreateAlert(ctx, tasks.NewAlert{Priority: 3, Title: "acked", ExpiresAt: at(time.Hour)})
	st.TransitionAlert(ctx, ackedSoon.ID, nil, tasks.AlertAcked)

	if got, err := st.ExpireAlerts(ctx, t0.Add(59*time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("before expiry: %+v %v", got, err)
	}
	got, err := st.ExpireAlerts(ctx, t0.Add(time.Hour)) // boundary: <= now expires
	if err != nil || len(got) != 1 || got[0].ID != soon.ID || got[0].State != tasks.AlertExpired {
		t.Fatalf("at expiry: %+v %v", got, err)
	}
	if again, _ := st.ExpireAlerts(ctx, t0.Add(time.Hour)); len(again) != 0 {
		t.Errorf("re-expire returned %+v", again)
	}
	got, _ = st.ExpireAlerts(ctx, t0.Add(1000*time.Hour))
	if len(got) != 1 || got[0].ID != later.ID {
		t.Errorf("far future: %+v", got)
	}
	for id, want := range map[string]string{
		soon.ID: tasks.AlertExpired, later.ID: tasks.AlertExpired,
		never.ID: tasks.AlertOpen, ackedSoon.ID: tasks.AlertAcked,
	} {
		if a, _ := st.GetAlert(ctx, id); a.State != want {
			t.Errorf("%s state %s, want %s", id, a.State, want)
		}
	}
}
