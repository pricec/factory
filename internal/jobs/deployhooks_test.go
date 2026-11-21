package jobs

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The hook list in code must equal the reviewed table in docs/merging.md.
func TestDeployHooksMatchDoc(t *testing.T) {
	b, err := os.ReadFile("../../docs/merging.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `(pricec/[^`]+)` \\| (integration \\(default\\)|`(foreman:[^`]+)`) \\| `([^`]+)`")
	var got []DeployHook
	for _, l := range strings.Split(string(b), "\n") {
		if m := row.FindStringSubmatch(l); m != nil {
			got = append(got, DeployHook{Repo: m[1], Foreman: m[3], Script: m[4]})
		}
	}
	if len(got) != len(DeployHooks) {
		t.Fatalf("doc table has %d rows, DeployHooks %d: %+v", len(got), len(DeployHooks), got)
	}
	for i := range got {
		if got[i] != DeployHooks[i] {
			t.Errorf("row %d: doc %+v, code %+v", i, got[i], DeployHooks[i])
		}
	}
}

func TestRemoteDeployHook(t *testing.T) {
	const integ = "foreman:server"
	if h, ok := RemoteDeployHook("media-curator", integ); !ok || h.Foreman != "foreman:megatron" {
		t.Fatalf("media-curator: %+v %v", h, ok)
	}
	for _, r := range []string{"pricec/movies-chat", "pricec/video-chat", "pricec/factory", "pricec/unknown"} {
		if _, ok := RemoteDeployHook(r, integ); ok {
			t.Errorf("%s must not be remote", r)
		}
	}
	// A hook on the integration foreman itself is not remote.
	if _, ok := RemoteDeployHook("media-curator", "foreman:megatron"); ok {
		t.Error("hook foreman == integration foreman must not be remote")
	}
}

func TestParseDeployOutcome(t *testing.T) {
	full := strings.Repeat("a", 40)
	for _, tc := range []struct {
		exit     int
		line     string
		deployed bool
		deferred bool
		sha      string
	}{
		{0, "DEPLOYED sha=" + full, true, false, full},
		{0, "DEPLOYED a1b2c3d prev=9f8e7d6", true, false, "a1b2c3d"},
		{3, "DEPLOYED a1b2c3d prev=9f8e7d6", true, true, "a1b2c3d"},
		{3, "DEPLOYED sha=" + full, true, true, full},
		{1, "DEPLOYED a1b2c3d prev=9f8e7d6", false, false, ""},
		{0, "all good", false, false, ""},
		{0, "", false, false, ""},
		{0, "DEPLOYED sha=", false, false, ""},
	} {
		g := ParseDeployOutcome(tc.exit, tc.line)
		if g.Deployed != tc.deployed || g.RestartDeferred != tc.deferred || g.SHA != tc.sha {
			t.Errorf("exit %d %q: got %+v", tc.exit, tc.line, g)
		}
	}
}

func TestRemoteDeployJobSpec(t *testing.T) {
	h := DeployHooks[2]
	mr := MergeRequest{ID: "mr_1", Repo: h.Repo}
	sha := strings.Repeat("b", 40)
	s := RemoteDeployJobSpec(mr, "job_x", sha, h)
	if s.Foreman != "foreman:megatron" || s.CreatedBy != MergeJobCreatedBy || len(s.Secrets) != 1 || s.Secrets[0] != DeployGitSecret {
		t.Fatalf("spec: %+v", s)
	}
	if !IsRemoteDeployJob(s) || IsRemoteDeployJob(MergeJobSpec(mr, "j", "foreman:server")) {
		t.Error("IsRemoteDeployJob misclassifies")
	}
	for _, want := range []string{sha, "x-access-token", "never write it", "DEPLOYED sha=", "prev=", "rc 3", "restart_deferred", `stage: "deploy"`} {
		if !strings.Contains(s.Task, want) {
			t.Errorf("task missing %q", want)
		}
	}
	if err := s.Validate(); err != nil {
		t.Errorf("spec invalid: %v", err)
	}
}
