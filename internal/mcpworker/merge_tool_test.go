package mcpworker_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/mcpworker"
)

// TestRequestMerge: the worker tool relays the call with the CALLER's job
// id (from the worker token, never from arguments) and returns the queued
// request (spec §9.2, §6.11 job-owned merges).
func TestRequestMerge(t *testing.T) {
	e := newEnv(t)
	out := e.call("request_merge", map[string]any{
		"branch": "feat/x", "head": "0123456789abcdef", "note": "Add x", "after": []string{"feat/base"},
	})
	got := structured(t, resultOf(t, out))
	if got["id"] != "mr_TEST" || got["state"] != "queued" {
		t.Fatalf("result = %v", got)
	}
	if w, _ := got["waiting_on"].([]any); len(w) != 1 || w[0] != "feat/base" {
		t.Fatalf("waiting_on = %v", got["waiting_on"])
	}
	want := mcpworker.MergeRequestArgs{Branch: "feat/x", Head: "0123456789abcdef", Note: "Add x", After: []string{"feat/base"}}
	if len(e.merges.calls) != 1 || !reflect.DeepEqual(e.merges.calls[0], want) {
		t.Fatalf("relayed = %+v, want %+v", e.merges.calls, want)
	}
	if e.merges.jobs[0] != jobID {
		t.Fatalf("relayed job = %q, want the token's job %q", e.merges.jobs[0], jobID)
	}
}

// TestRequestMergeErrors: missing fields and factory refusals are loud
// tool errors that carry the reason.
func TestRequestMergeErrors(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		err  error
		want string
	}{
		{"no head", map[string]any{"branch": "feat/x", "head": ""}, nil, "branch and head are required"},
		{
			"factory refuses",
			map[string]any{"branch": "feat/x", "head": "0123456"},
			errors.New("duplicate_request: mr_1 is merging (feat/x)"), "mr_1 is merging",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.merges.err = tc.err
			if msg := toolError(t, e.call("request_merge", tc.args)); !strings.Contains(msg, tc.want) {
				t.Fatalf("tool error = %q, want it to contain %q", msg, tc.want)
			}
		})
	}
}
