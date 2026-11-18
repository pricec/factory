package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeGH struct{ perms map[string]bool }

func (f fakeGH) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	switch args[0] + " " + args[1] {
	case "auth status":
		return []byte(`{"hosts":{"github.com":[{"state":"success","active":true,"login":"other"},{"state":"success","login":"owner"}]}}`), nil
	case "auth token":
		return []byte("tok-" + args[len(args)-1]), nil
	}
	if args[0] == "api" && len(env) == 1 {
		if f.perms[strings.TrimPrefix(env[0], "GH_TOKEN=")] {
			return []byte(`{"push":true}`), nil
		}
		return []byte(`{"push":false}`), nil
	}
	return nil, errors.New("unexpected")
}

func TestRun(t *testing.T) {
	gh := fakeGH{perms: map[string]bool{"tok-owner": true}}
	var gotEnv, gotArgv []string
	exe := func(_ context.Context, env, argv []string, _ io.Reader, _, _ io.Writer) int {
		gotEnv, gotArgv = env, argv
		return 7
	}
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"-repo", "owner/r", "gh", "pr", "create"}, gh, exe, nil, &out, &errb)
	if code != 7 {
		t.Fatalf("exit = %d, want the command's 7 (stderr %s)", code, errb.String())
	}
	if strings.Join(gotEnv, " ") != "GH_TOKEN=tok-owner" || strings.Join(gotArgv, " ") != "gh pr create" {
		t.Fatalf("env=%v argv=%v", gotEnv, gotArgv)
	}
	if strings.Contains(out.String()+errb.String(), "tok-owner") {
		t.Fatal("token leaked to output")
	}

	out.Reset()
	if code := run(context.Background(), []string{"-repo", "owner/r", "-login"}, gh, exe, nil, &out, &errb); code != 0 || out.String() != "owner\n" {
		t.Fatalf("-login: code=%d out=%q", code, out.String())
	}

	errb.Reset()
	code = run(context.Background(), []string{"-repo", "x/y", "true"}, fakeGH{}, exe, nil, &out, &errb)
	if code != 3 || !strings.Contains(errb.String(), "tried: other (no push permission), owner (no push permission)") {
		t.Fatalf("no account: code=%d stderr=%q", code, errb.String())
	}

	if code := run(context.Background(), []string{"gh", "pr"}, gh, exe, nil, &out, &errb); code != 2 {
		t.Fatalf("missing -repo: code=%d", code)
	}
}

func TestScrubEnv(t *testing.T) {
	got := scrubEnv([]string{"PATH=/bin", "GH_TOKEN=a", "GITHUB_TOKEN=b", "GH_TOKENX=c"})
	if strings.Join(got, " ") != "PATH=/bin GH_TOKENX=c" {
		t.Fatalf("scrubEnv = %v", got)
	}
}
