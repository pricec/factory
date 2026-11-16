package mcpforeman_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestPostToFactoryDestinations: post_to_factory's to is optional; omitted
// or factory:main is the only routable destination and lands in the parent
// inbox. Every other address is refused with a loud unroutable_address
// error and nothing is staged (spec §9.3) - the phantom "factory:planner"
// address in particular must never park silently.
func TestPostToFactoryDestinations(t *testing.T) {
	cases := []struct {
		name    string
		to      string // "" = omit the field
		wantOK  bool
		wantErr []string // substrings the tool error must carry
	}{
		{name: "omitted routes to factory:main", wantOK: true},
		{name: "explicit factory:main routes", to: parentInbox, wantOK: true},
		{
			name: "phantom factory:planner refused", to: "factory:planner",
			wantErr: []string{"unroutable_address", "factory:planner", "auth identity", "foreman.report"},
		},
		{
			name: "operator inbox refused with route hint", to: "planner:main",
			wantErr: []string{"unroutable_address", "planner:main", "operator inbox", "foreman.report"},
		},
		{
			name: "unknown inbox refused", to: "nowhere:x",
			wantErr: []string{"unroutable_address", "nowhere:x", "factory:main"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			args := map[string]any{
				"type": "job.event", "job_id": jobID,
				"payload": map[string]any{"event": "accepted"},
			}
			if tc.to != "" {
				args["to"] = tc.to
			}
			out := e.callTool("post_to_factory", args)
			if !tc.wantOK {
				msg := toolError(t, out)
				for _, want := range tc.wantErr {
					if !strings.Contains(msg, want) {
						t.Errorf("tool error = %q, want mention of %q", msg, want)
					}
				}
				envs, err := e.svc.Get(context.Background(), parentInbox, 10, 0)
				if err != nil {
					t.Fatalf("inbox.Get: %v", err)
				}
				if len(envs) != 0 {
					t.Fatalf("refused post staged %d envelope(s), want none", len(envs))
				}
				return
			}
			if sc := structured(t, resultOf(t, out)); sc["ok"] != true {
				t.Fatalf("post_to_factory = %v, want ok:true", sc)
			}
			envs, err := e.svc.Get(context.Background(), parentInbox, 10, 0)
			if err != nil || len(envs) != 1 {
				t.Fatalf("parent inbox = %d envelope(s), err %v; want 1", len(envs), err)
			}
		})
	}
}

// TestPostToFactoryForemanReport: a foreman.report is the operator report
// type (spec §9.3, §9.5). It stages with its type and payload verbatim and
// requires a payload - a report with nothing in it is refused, not sent.
func TestPostToFactoryForemanReport(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if msg := toolError(t, e.callTool("post_to_factory", map[string]any{
		"type": "foreman.report",
	})); !strings.Contains(msg, "payload") {
		t.Fatalf("payload-less foreman.report error = %q, want mention of payload", msg)
	}

	payload := map[string]any{"text": "toast task finished", "job_id": jobID}
	sc := structured(t, resultOf(t, e.callTool("post_to_factory", map[string]any{
		"type":    "foreman.report",
		"payload": payload,
	})))
	if sc["ok"] != true {
		t.Fatalf("foreman.report = %v, want ok:true", sc)
	}
	envs, err := e.svc.Get(ctx, parentInbox, 10, 0)
	if err != nil || len(envs) != 1 {
		t.Fatalf("parent inbox = %d envelope(s), err %v; want 1", len(envs), err)
	}
	got := envs[0]
	if got.Type != "foreman.report" || got.From != foremanInbox {
		t.Fatalf("envelope = %+v", got)
	}
	var p map[string]any
	if err := json.Unmarshal(got.Payload, &p); err != nil || p["text"] != "toast task finished" {
		t.Fatalf("payload = %s: %v", got.Payload, err)
	}
}
