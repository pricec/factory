package mcpfactory_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/jobs"
)

// TestRequestMerge (spec §6.11, §9.4): the planner's request_merge queues
// a merge request (requested_by planner); bad input and a duplicate active
// branch are tool errors that create nothing; the description routes
// "merge and deploy" asks here instead of prose jobs.
func TestRequestMerge(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	head := "0123456789abcdef0123456789abcdef01234567"

	res := resultOf(t, e.callTool("request_merge", map[string]any{"branch": "feat/x", "head": head, "note": "Add X"}))
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("request_merge errored: %v", res)
	}
	sc := structured(t, res)
	id, _ := sc["id"].(string)
	if !strings.HasPrefix(id, "mr_") || sc["state"] != jobs.MergeQueued {
		t.Fatalf("structured = %v", sc)
	}
	mr, ok, err := e.store.GetMergeRequest(ctx, id)
	if err != nil || !ok || mr.RequestedBy != "planner" || mr.Note != "Add X" || mr.Repo != jobs.DefaultMergeRepo {
		t.Fatalf("stored = %+v ok=%v err=%v", mr, ok, err)
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"duplicate", map[string]any{"branch": "feat/x", "head": head}, "already exists"},
		{"bad head", map[string]any{"branch": "feat/y", "head": "nope"}, "head"},
		{"master", map[string]any{"branch": "master", "head": head}, "integration branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := resultOf(t, e.callTool("request_merge", tc.args))
			if isErr, _ := res["isError"].(bool); !isErr {
				t.Fatalf("want tool error, got %v", res)
			}
			raw, _ := json.Marshal(res)
			if !strings.Contains(string(raw), tc.want) {
				t.Errorf("error %s does not mention %q", raw, tc.want)
			}
		})
	}
	all, _ := e.store.MergeRequests(ctx, 0)
	if len(all) != 1 {
		t.Errorf("merge requests = %d, want 1 (rejected calls create nothing)", len(all))
	}

	_, out := e.rpc(e.planner, "tools/list", nil)
	raw, _ := json.Marshal(out)
	for _, want := range []string{"request_merge", "merge (and deploy) X", "never a create_job prose job"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("tools/list missing %q", want)
		}
	}
}

// TestRequestMergeRepo: the optional repo field targets another repo (bare
// names get the default owner), and it is exposed in the tool schema.
func TestRequestMergeRepo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	head := "0123456789abcdef0123456789abcdef01234567"
	for i, tc := range []struct{ repo, want string }{
		{"video-chat", "pricec/video-chat"},
		{"pricec/video-chat", "pricec/video-chat"},
		{"", jobs.DefaultMergeRepo},
	} {
		args := map[string]any{"branch": "feat/r-" + string(rune('a'+i)), "head": head}
		if tc.repo != "" {
			args["repo"] = tc.repo
		}
		sc := structured(t, resultOf(t, e.callTool("request_merge", args)))
		id, _ := sc["id"].(string)
		mr, ok, err := e.store.GetMergeRequest(ctx, id)
		if err != nil || !ok || mr.Repo != tc.want {
			t.Fatalf("repo %q: stored = %+v ok=%v err=%v", tc.repo, mr, ok, err)
		}
	}
	res := resultOf(t, e.callTool("request_merge", map[string]any{"branch": "feat/bad", "head": head, "repo": "no pe"}))
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Errorf("bad repo accepted: %v", res)
	}
	_, out := e.rpc(e.planner, "tools/list", nil)
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"repo"`) || !strings.Contains(string(raw), "default branch") {
		t.Errorf("tools/list lacks request_merge repo field: %s", raw)
	}
}
