package mcpforeman_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestRaiseAlertPostsEnvelope: raise_alert posts an alert.raise envelope to
// the parent inbox, from the foreman's inbox, with the args as payload.
func TestRaiseAlertPostsEnvelope(t *testing.T) {
	e := newEnv(t)
	sc := structured(t, resultOf(t, e.callTool("raise_alert", map[string]any{
		"priority": 0, "title": "disk full", "body": "/ 99%", "no_expiry": true, "task_id": "task_x",
	})))
	if sc["ok"] != true || sc["id"] == "" {
		t.Fatalf("raise_alert = %v", sc)
	}
	envs, err := e.svc.Get(context.Background(), parentInbox, 10, 0)
	if err != nil || len(envs) != 1 {
		t.Fatalf("parent inbox = %d envelopes, %v", len(envs), err)
	}
	got := envs[0]
	if got.Type != "alert.raise" || got.From != foremanInbox {
		t.Fatalf("envelope = %+v", got)
	}
	var p map[string]any
	if err := json.Unmarshal(got.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["priority"] != float64(0) || p["title"] != "disk full" || p["body"] != "/ 99%" ||
		p["no_expiry"] != true || p["task_id"] != "task_x" {
		t.Fatalf("payload = %v", p)
	}
	if _, has := p["ttl_s"]; has {
		t.Errorf("unset ttl_s leaked into payload: %v", p)
	}
}

func TestRaiseAlertRejectsBadArgs(t *testing.T) {
	e := newEnv(t)
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"negative priority": {map[string]any{"priority": -1, "title": "x"}, "priority"},
		"no title":          {map[string]any{"priority": 1, "title": " "}, "title"},
		"negative ttl":      {map[string]any{"priority": 1, "title": "x", "ttl_s": -1}, "ttl_s"},
		"ttl + no_expiry":   {map[string]any{"priority": 1, "title": "x", "ttl_s": 5, "no_expiry": true}, "ttl_s"},
	} {
		t.Run(name, func(t *testing.T) {
			if msg := toolError(t, e.callTool("raise_alert", tc.args)); !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want mention of %q", msg, tc.want)
			}
		})
	}
	envs, _ := e.svc.Get(context.Background(), parentInbox, 10, 0)
	if len(envs) != 0 {
		t.Fatalf("rejected calls posted %d envelopes", len(envs))
	}
}
