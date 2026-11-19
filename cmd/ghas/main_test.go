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
	code := run(context.Background(), []string{"-account", "any", "-repo", "owner/r", "gh", "pr", "create"}, gh, exe, nil, &out, &errb)
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
	if code := run(context.Background(), []string{"-account", "any", "-repo", "owner/r", "-login"}, gh, exe, nil, &out, &errb); code != 0 || out.String() != "owner\n" {
		t.Fatalf("-login: code=%d out=%q", code, out.String())
	}

	errb.Reset()
	code = run(context.Background(), []string{"-account", "any", "-repo", "x/y", "true"}, fakeGH{}, exe, nil, &out, &errb)
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

// pricecGH is logged in to chris-qunify (active) and pricec; both can push.
type pricecGH struct{ queried []string }

func (f *pricecGH) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	switch args[0] + " " + args[1] {
	case "auth status":
		return []byte(`{"hosts":{"github.com":[{"state":"success","active":true,"login":"chris-qunify"},{"state":"success","login":"pricec"}]}}`), nil
	case "auth token":
		f.queried = append(f.queried, args[len(args)-1])
		return []byte("tok-" + args[len(args)-1]), nil
	}
	return []byte(`{"push":true}`), nil
}

// Merge jobs act as pricec only, even for a repo owned by someone else and
// with chris-qunify active; chris-qunify is never queried.
func TestRunDefaultsToPricec(t *testing.T) {
	for _, repo := range []string{"pricec/factory", "qunify/app"} {
		gh := &pricecGH{}
		var gotEnv []string
		exe := func(_ context.Context, env, _ []string, _ io.Reader, _, _ io.Writer) int { gotEnv = env; return 0 }
		var out, errb bytes.Buffer
		if code := run(context.Background(), []string{"-repo", repo, "gh", "pr", "merge"}, gh, exe, nil, &out, &errb); code != 0 {
			t.Fatalf("%s: code=%d stderr=%s", repo, code, errb.String())
		}
		if strings.Join(gotEnv, " ") != "GH_TOKEN=tok-pricec" {
			t.Fatalf("%s: env=%v", repo, gotEnv)
		}
		if strings.Join(gh.queried, ",") != "pricec" {
			t.Fatalf("%s: queried %v, want only pricec", repo, gh.queried)
		}
	}
}

func TestRunPricecMissingFails(t *testing.T) {
	var out, errb bytes.Buffer
	// fakeGH is logged in as other/owner only.
	code := run(context.Background(), []string{"-repo", "owner/r", "-login"}, fakeGH{perms: map[string]bool{"tok-owner": true}}, nil, nil, &out, &errb)
	if code != 3 || !strings.Contains(errb.String(), "pricec is not logged in") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}
