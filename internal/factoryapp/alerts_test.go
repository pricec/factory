package factoryapp_test

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/mcpfactory"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

type alertList struct {
	Alerts []tasks.Alert `json:"alerts"`
}

func (e *testEnv) listAlerts() []tasks.Alert {
	e.t.Helper()
	var l alertList
	if st := e.do("GET", "/v1/alerts", "", nil, &l); st != http.StatusOK {
		e.t.Fatalf("GET /v1/alerts = %d", st)
	}
	if l.Alerts == nil {
		e.t.Fatalf("alerts must be [] not null")
	}
	return l.Alerts
}

func (e *testEnv) raise(prio int, title string, opts ...func(*mcpfactory.RaiseAlertInput)) tasks.Alert {
	e.t.Helper()
	in := mcpfactory.RaiseAlertInput{Priority: prio, Title: title}
	for _, o := range opts {
		o(&in)
	}
	got, err := e.a.RaisePlannerAlert(e.ctx, in)
	if err != nil {
		e.t.Fatalf("raise %q: %v", title, err)
	}
	al, err := e.a.Tasks().GetAlert(e.ctx, got.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return al
}

// TestAlertTTLDefaultsAndOverrides: P0/P1 never expire, P2 24h, P3+ 4h;
// ttl_s and no_expiry override any priority, P0 included.
func TestAlertTTLDefaultsAndOverrides(t *testing.T) {
	e := newQuietEnv(t)
	now := e.clock.Now()
	cases := []struct {
		name string
		prio int
		opt  func(*mcpfactory.RaiseAlertInput)
		want time.Duration // 0 = never
	}{
		{"P0 never", 0, nil, 0},
		{"P1 never", 1, nil, 0},
		{"P2 24h", 2, nil, 24 * time.Hour},
		{"P3 4h", 3, nil, 4 * time.Hour},
		{"P9 4h", 9, nil, 4 * time.Hour},
		{"P0 ttl override", 0, func(i *mcpfactory.RaiseAlertInput) { i.TTLS = 90 }, 90 * time.Second},
		{"P3 no_expiry", 3, func(i *mcpfactory.RaiseAlertInput) { i.NoExpiry = true }, 0},
		{"P2 ttl shorter", 2, func(i *mcpfactory.RaiseAlertInput) { i.TTLS = 60 }, time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts []func(*mcpfactory.RaiseAlertInput)
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}
			al := e.raise(tc.prio, tc.name, opts...)
			if al.Source != "planner" || al.State != tasks.AlertOpen {
				t.Fatalf("alert = %+v", al)
			}
			if tc.want == 0 {
				if al.ExpiresAt != nil {
					t.Fatalf("expires_at = %v, want never", al.ExpiresAt)
				}
				return
			}
			if al.ExpiresAt == nil || !al.ExpiresAt.Equal(now.Add(tc.want)) {
				t.Fatalf("expires_at = %v, want %v", al.ExpiresAt, now.Add(tc.want))
			}
		})
	}
}

func TestRaiseAlertValidation(t *testing.T) {
	e := newQuietEnv(t)
	cases := map[string]mcpfactory.RaiseAlertInput{
		"negative priority": {Priority: -1, Title: "x"},
		"empty title":       {Priority: 1, Title: "  "},
		"long title":        {Priority: 1, Title: strings.Repeat("t", 201)},
		"long body":         {Priority: 1, Title: "x", Body: strings.Repeat("b", 9<<10)},
		"negative ttl":      {Priority: 1, Title: "x", TTLS: -5},
		"ttl and no_expiry": {Priority: 1, Title: "x", TTLS: 5, NoExpiry: true},
		"unknown task":      {Priority: 1, Title: "x", TaskID: "task_nope"},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := e.a.RaisePlannerAlert(e.ctx, in); err == nil {
				t.Fatal("want error")
			}
		})
	}
	if n := len(e.listAlerts()); n != 0 {
		t.Fatalf("%d alerts stored after rejected raises", n)
	}
	// A known task id links.
	task, err := e.a.Tasks().CreateTask(e.ctx, "t", 2)
	if err != nil {
		t.Fatal(err)
	}
	if al := e.raise(1, "linked", func(i *mcpfactory.RaiseAlertInput) { i.TaskID = task.ID }); al.TaskID != task.ID {
		t.Fatalf("task_id = %q", al.TaskID)
	}
}

// TestListAlertsOrderAndLazyExpiry: open only, priority ASC then newest
// first; expired alerts vanish on read without any ticker.
func TestListAlertsOrderAndLazyExpiry(t *testing.T) {
	e := newQuietEnv(t)
	e.raise(2, "p2-old")
	e.clock.Advance(time.Minute)
	e.raise(2, "p2-new")
	e.raise(0, "p0")
	e.raise(3, "p3 short", func(i *mcpfactory.RaiseAlertInput) { i.TTLS = 30 })

	var titles []string
	for _, al := range e.listAlerts() {
		titles = append(titles, al.Title)
	}
	if got := strings.Join(titles, ","); got != "p0,p2-new,p2-old,p3 short" {
		t.Fatalf("order = %s", got)
	}
	e.clock.Advance(31 * time.Second) // p3 short expires lazily
	titles = nil
	for _, al := range e.listAlerts() {
		titles = append(titles, al.Title)
	}
	if got := strings.Join(titles, ","); got != "p0,p2-new,p2-old" {
		t.Fatalf("after lazy expiry = %s", got)
	}
	e.clock.Advance(25 * time.Hour)
	if got := e.listAlerts(); len(got) != 1 || got[0].Title != "p0" {
		t.Fatalf("after 25h = %+v, want only the never-expiring P0", got)
	}
}

func TestExpireAlertsPass(t *testing.T) {
	e := newQuietEnv(t)
	e.raise(3, "a")
	e.raise(0, "b")
	if n, err := e.a.ExpireAlertsPass(e.ctx); err != nil || n != 0 {
		t.Fatalf("early pass = %d, %v", n, err)
	}
	e.clock.Advance(4 * time.Hour)
	if n, err := e.a.ExpireAlertsPass(e.ctx); err != nil || n != 1 {
		t.Fatalf("pass = %d, %v, want 1", n, err)
	}
	if n, _ := e.a.ExpireAlertsPass(e.ctx); n != 0 {
		t.Fatalf("second pass = %d, want 0", n)
	}
}

func TestAckAlert(t *testing.T) {
	e := newQuietEnv(t)
	al := e.raise(1, "ack me")

	var resp struct {
		Alert tasks.Alert `json:"alert"`
	}
	if st := e.do("POST", "/v1/alerts/"+al.ID+"/ack", "", nil, &resp); st != http.StatusOK {
		t.Fatalf("ack = %d", st)
	}
	if resp.Alert.ID != al.ID || resp.Alert.State != tasks.AlertAcked {
		t.Fatalf("alert = %+v", resp.Alert)
	}
	if n := len(e.listAlerts()); n != 0 {
		t.Fatalf("acked alert still listed: %d", n)
	}
	var errBody map[string]any
	if st := e.do("POST", "/v1/alerts/"+al.ID+"/ack", "", nil, &errBody); st != http.StatusConflict || errBody["error"] != "already_terminal" {
		t.Fatalf("re-ack = %d %v", st, errBody)
	}
	if st := e.do("POST", "/v1/alerts/alert_nope/ack", "", nil, &errBody); st != http.StatusNotFound {
		t.Fatalf("unknown ack = %d", st)
	}
	// Expired alert: ack is terminal-conflict (lazy expiry on the ack path).
	exp := e.raise(3, "short", func(i *mcpfactory.RaiseAlertInput) { i.TTLS = 10 })
	e.clock.Advance(time.Minute)
	if st := e.do("POST", "/v1/alerts/"+exp.ID+"/ack", "", nil, &errBody); st != http.StatusConflict || errBody["error"] != "already_terminal" {
		t.Fatalf("ack expired = %d %v", st, errBody)
	}
}

// TestReplyAlert: reply opens a task seeded with the alert, posts the reply
// as a chat.message on it and wakes the planner; acked and expired alerts
// still get a task but keep their state.
func TestReplyAlert(t *testing.T) {
	e := newEnv(t)
	rec := &promptRecorder{}
	e.a.SetPlannerExec(rec.exec(e))

	al := e.raise(1, "Job stuck", func(i *mcpfactory.RaiseAlertInput) { i.Body = "needs a foreman" })
	var resp struct {
		Alert  tasks.Alert `json:"alert"`
		TaskID string      `json:"task_id"`
	}
	if st := e.do("POST", "/v1/alerts/"+al.ID+"/reply", "", map[string]string{"text": "use the mac"}, &resp); st != http.StatusCreated {
		t.Fatalf("reply = %d", st)
	}
	if resp.Alert.State != tasks.AlertReplied || resp.TaskID == "" || resp.Alert.TaskID != resp.TaskID {
		t.Fatalf("resp = %+v", resp)
	}
	task, err := e.a.Tasks().GetTask(e.ctx, resp.TaskID)
	if err != nil || task.Status != tasks.StatusOpen || task.Title != "Job stuck" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	thread, err := e.a.Inbox().ThreadMessages(e.ctx, resp.TaskID, 10)
	if err != nil || len(thread) != 2 {
		t.Fatalf("thread = %d msgs, %v", len(thread), err)
	}
	if thread[0].Type != "brain.reply" || !strings.Contains(string(thread[0].Payload), "needs a foreman") {
		t.Fatalf("seed = %s %s", thread[0].Type, thread[0].Payload)
	}
	if thread[1].Type != "chat.message" || !strings.Contains(string(thread[1].Payload), "use the mac") || thread[1].ThreadID != resp.TaskID {
		t.Fatalf("reply = %+v", thread[1])
	}
	waitFor(t, "planner wake", 5*time.Second, func() bool { return rec.count() >= 1 })
	if !strings.Contains(rec.last(), "use the mac") {
		t.Fatalf("wake prompt lacks the reply: %q", rec.last())
	}

	// Acked alert: still gets a task, state stays acked.
	acked := e.raise(2, "acked one")
	e.do("POST", "/v1/alerts/"+acked.ID+"/ack", "", nil, nil)
	var r2 struct {
		Alert  tasks.Alert `json:"alert"`
		TaskID string      `json:"task_id"`
	}
	if st := e.do("POST", "/v1/alerts/"+acked.ID+"/reply", "", map[string]string{"text": "late"}, &r2); st != http.StatusCreated || r2.TaskID == "" {
		t.Fatalf("reply to acked = %d %+v", st, r2)
	}
	if r2.Alert.State != tasks.AlertAcked || r2.Alert.TaskID != r2.TaskID {
		t.Fatalf("acked alert after reply = %+v", r2.Alert)
	}
	// Expired alert likewise.
	short := e.raise(3, "short", func(i *mcpfactory.RaiseAlertInput) { i.TTLS = 5 })
	e.clock.Advance(time.Minute)
	if st := e.do("POST", "/v1/alerts/"+short.ID+"/reply", "", map[string]string{"text": "x"}, &r2); st != http.StatusCreated || r2.Alert.State != tasks.AlertExpired || r2.TaskID == "" {
		t.Fatalf("reply to expired = %d %+v", st, r2)
	}
}

func TestReplyAlertErrors(t *testing.T) {
	e := newQuietEnv(t)
	al := e.raise(1, "x")
	var out map[string]any
	for name, tc := range map[string]struct {
		id, body string
		want     int
	}{
		"unknown":    {"alert_nope", `{"text":"hi"}`, http.StatusNotFound},
		"bad json":   {al.ID, `{`, http.StatusBadRequest},
		"empty text": {al.ID, `{"text":"  "}`, http.StatusBadRequest},
	} {
		if st := e.do("POST", "/v1/alerts/"+tc.id+"/reply", "", tc.body, &out); st != tc.want {
			t.Errorf("%s: status = %d, want %d", name, st, tc.want)
		}
	}
	if got := e.listAlerts(); len(got) != 1 || got[0].State != tasks.AlertOpen {
		t.Fatalf("failed replies must leave the alert open: %+v", got)
	}
	if ts, _ := e.a.Tasks().ListTasks(e.ctx, tasks.TaskFilter{}); len(ts) != 0 {
		t.Fatalf("failed replies created %d tasks", len(ts))
	}
}

// TestPlannerMCPRaiseAlert: the planner MCP raise_alert tool creates an
// alert with source "planner".
func TestPlannerMCPRaiseAlert(t *testing.T) {
	e := newEnv(t)
	call := func(args map[string]any) map[string]any {
		_, out := e.rpc("tools/call", map[string]any{"name": "raise_alert", "arguments": args})
		return out
	}
	out := call(map[string]any{"priority": 2, "title": "from planner", "body": "b"})
	res, _ := out["result"].(map[string]any)
	sc, _ := res["structuredContent"].(map[string]any)
	if sc["ok"] != true || sc["id"] == "" || sc["expires_at"] == nil {
		t.Fatalf("result = %v", out)
	}
	if got := e.listAlerts(); len(got) != 1 || got[0].Source != "planner" || got[0].Title != "from planner" {
		t.Fatalf("alerts = %+v", got)
	}
	bad := call(map[string]any{"priority": 2, "title": ""})
	if r, _ := bad["result"].(map[string]any); r["isError"] != true {
		t.Fatalf("empty title not rejected: %v", bad)
	}
}

// TestForemanAlertRaise: a foreman's alert.raise envelope on factory:main
// becomes an alert with source foreman:<id>; malformed ones are surfaced as
// a P2 alert instead of vanishing.
func TestForemanAlertRaise(t *testing.T) {
	e := newEnv(t)
	bearer := e.pair("alpha", nil)
	post := func(payload string) {
		t.Helper()
		if st := e.postToFactoryMain(bearer, wire.MessageEnvelope{
			Type: "alert.raise", From: "foreman:alpha", Payload: []byte(payload),
		}); st != http.StatusCreated {
			t.Fatalf("post = %d", st)
		}
	}
	post(`{"priority":0,"title":"disk full","body":"/ at 99%","no_expiry":true}`)
	waitFor(t, "alert", 5*time.Second, func() bool { return len(e.listAlerts()) == 1 })
	al := e.listAlerts()[0]
	if al.Source != "foreman:alpha" || al.Priority != 0 || al.Title != "disk full" || al.Body != "/ at 99%" || al.ExpiresAt != nil {
		t.Fatalf("alert = %+v", al)
	}
	// Bad payload and invalid alert both surface as factory P2 alerts.
	post(`"not an object"`)
	post(`{"priority":1,"title":""}`)
	waitFor(t, "rejections surfaced", 5*time.Second, func() bool { return len(e.listAlerts()) == 3 })
	var rejected int
	for _, a := range e.listAlerts() {
		if a.Source == "factory:consumer" && a.Priority == 2 && strings.Contains(a.Title, "alert.raise from foreman:alpha") {
			rejected++
		}
	}
	if rejected != 2 {
		t.Fatalf("rejected = %d, alerts = %+v", rejected, e.listAlerts())
	}
	// The envelope did not leak into the chat inbox.
	if len(operatorEnvelopes(t, e, "alert.raise")) != 0 {
		t.Fatal("alert.raise leaked into planner:main")
	}
}

// TestAlertChangedSSE: creating and acking an alert publishes synthetic
// alert.changed events on the planner:main stream (contract I5).
func TestAlertChangedSSE(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/v1/inboxes/planner:main/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	time.Sleep(200 * time.Millisecond)
	al := e.raise(1, "sse alert")
	e.do("POST", "/v1/alerts/"+al.ID+"/ack", "", nil, nil)

	var seen []string
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("stream closed; seen %v", seen)
			}
			if strings.Contains(l, "alert.changed") && strings.Contains(l, al.ID) {
				seen = append(seen, l)
			}
		case <-deadline:
			t.Fatalf("saw %d alert.changed events, want 2", len(seen))
		}
	}
	if !strings.Contains(seen[0], `\"state\":\"open\"`) && !strings.Contains(seen[0], `"state":"open"`) {
		t.Errorf("first event not open: %s", seen[0])
	}
	if !strings.Contains(seen[1], "acked") {
		t.Errorf("second event not acked: %s", seen[1])
	}
}
