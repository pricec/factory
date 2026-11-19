package jobs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func TestNormalizeRepoAndBaseBranch(t *testing.T) {
	for _, tc := range []struct{ in, repo, base string }{
		{"", "", "master"},
		{"pricec/factory", "pricec/factory", "master"},
		{"factory", "pricec/factory", "master"},
		{"video-chat", "pricec/video-chat", "main"},
		{"pricec/video-chat", "pricec/video-chat", "main"},
		{" https://github.com/pricec/Video-Chat.git ", "pricec/video-chat", "main"},
	} {
		if got := NormalizeRepo(tc.in); got != tc.repo {
			t.Errorf("NormalizeRepo(%q) = %q, want %q", tc.in, got, tc.repo)
		}
		if got := MergeBaseBranch(tc.in); got != tc.base {
			t.Errorf("MergeBaseBranch(%q) = %q, want %q", tc.in, got, tc.base)
		}
	}
}

func TestCheckConflictScopedPerRepo(t *testing.T) {
	fp := func(id, repo string, touches ...string) Footprint {
		return Footprint{JobID: id, Repo: repo, CreatedAt: "2026-11-18T09:00:00Z", State: StateRunning, Priority: 5, Touches: touches}
	}
	for _, tc := range []struct {
		name     string
		repo     string
		touches  []string
		active   Footprint
		wantHold bool
	}{
		{"same repo overlap", "pricec/factory", []string{"backend/captions.go"}, fp("job_A", "pricec/factory", "backend/captions*.go"), true},
		{"different repos never conflict", "pricec/factory", []string{"backend/captions.go"}, fp("job_A", "pricec/video-chat", "backend/captions*.go"), false},
		{"different repos, star", "pricec/factory", []string{"*"}, fp("job_A", "pricec/video-chat", "*"), false},
		{"both unknown overlap", "", []string{"backend/x.go"}, fp("job_A", "", "backend/*.go"), true},
		{"both unknown disjoint", "", []string{"a/x.go"}, fp("job_A", "", "b/*.go"), false},
		{"unknown vs known glob overlap", "", []string{"backend/x.go"}, fp("job_A", "pricec/video-chat", "backend/*.go"), false},
		{"known vs unknown glob overlap", "pricec/factory", []string{"backend/x.go"}, fp("job_A", "", "backend/*.go"), false},
		{"unknown vs known star", "", []string{"a.go"}, fp("job_A", "pricec/video-chat", "*"), true},
		{"known vs unknown star", "pricec/factory", []string{"*"}, fp("job_A", "", "a.go"), true},
	} {
		hard, _ := CheckConflict(Candidate{JobID: "job_C", Repo: tc.repo, Touches: tc.touches, CreatedAt: "2026-11-18T10:00:00Z", Priority: 5}, []Footprint{tc.active})
		if (hard != nil) != tc.wantHold {
			t.Errorf("%s: hard=%v, want hold=%v", tc.name, hard, tc.wantHold)
		}
	}
}

func TestSpecRepo(t *testing.T) {
	s := wire.NewJobSpec("t", "x")
	if got := SpecRepo(s); got != "" {
		t.Errorf("unknown = %q", got)
	}
	s.Payload = json.RawMessage(`{"repo":"video-chat"}`)
	if got := SpecRepo(s); got != "pricec/video-chat" {
		t.Errorf("payload repo = %q", got)
	}
	s.Repo = "pricec/factory"
	if got := SpecRepo(s); got != "pricec/factory" {
		t.Errorf("spec repo = %q", got)
	}
}

// TestActiveFootprintsRepoAndStaleMerge: the spec repo reaches the
// footprint, and a done job whose merge request is "merging" under a dead
// merge job no longer holds its touches.
func TestActiveFootprintsRepoAndStaleMerge(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	mk := func(id, state, repo string, touches ...string) {
		t.Helper()
		spec := wire.NewJobSpec("t", "test")
		spec.ID = id
		spec.Repo = repo
		spec.Touches = touches
		if err := s.Create(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE jobs SET state = ? WHERE id = ?`, state, id); err != nil {
			t.Fatal(err)
		}
	}
	mk("job_vc", StateRunning, "video-chat", "backend/captions*.go")
	mk("job_dead_merge", StateDone, "pricec/factory", "x/**")
	mk("job_live_merge", StateDone, "pricec/factory", "y/**")
	mk("job_mergedead", StateDead, "", "m1")
	mk("job_mergerun", StateRunning, "", "m2")
	for src, mj := range map[string]string{"job_dead_merge": "job_mergedead", "job_live_merge": "job_mergerun"} {
		m, err := s.CreateMergeRequest(ctx, NewMergeRequest{Branch: "br-" + src, Head: testHead, JobID: src})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE merge_requests SET job_id = ? WHERE id = ?`, mj, m.ID); err != nil {
			t.Fatal(err)
		}
		if mj == "job_mergerun" {
			continue
		}
		// only one request may be merging at once: park the live one after
		if _, err := s.db.Exec(`UPDATE merge_requests SET state = ? WHERE id = ?`, MergeMerging, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ActiveFootprints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]Footprint{}
	for _, f := range got {
		ids[f.JobID] = f
	}
	if ids["job_vc"].Repo != "pricec/video-chat" {
		t.Errorf("repo = %q", ids["job_vc"].Repo)
	}
	if _, ok := ids["job_dead_merge"]; ok {
		t.Error("stale hold: merge job is dead but job still holds its touches")
	}
	if _, ok := ids["job_live_merge"]; !ok {
		t.Error("queued merge with a live job must still hold")
	}
}

func TestMergeTaskAndPayloadBase(t *testing.T) {
	mr := MergeRequest{ID: "mr_1", Repo: "pricec/video-chat", Branch: "feat/x", Head: testHead}
	spec := MergeJobSpec(mr, "job_1", "foreman:server")
	var p map[string]string
	if err := json.Unmarshal(spec.Payload, &p); err != nil || p["base"] != "main" || p["repo"] != "pricec/video-chat" {
		t.Errorf("payload = %v (%v)", p, err)
	}
	mr.Repo = DefaultMergeRepo
	spec = MergeJobSpec(mr, "job_1", "foreman:server")
	_ = json.Unmarshal(spec.Payload, &p)
	if p["base"] != "master" {
		t.Errorf("factory base = %q", p["base"])
	}
}
