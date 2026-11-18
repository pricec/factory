package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

type stubGH struct{ out string }

func (s stubGH) Run(context.Context, ...string) ([]byte, error) { return []byte(s.out), nil }

func TestRun(t *testing.T) {
	var out, errb bytes.Buffer
	green := `[{"databaseId":1,"workflowName":"ci","status":"completed","conclusion":"success","attempt":1}]`
	if c := run(context.Background(), []string{"-repo", "o/r", "-sha", "abc"}, stubGH{green}, &out, &errb); c != 0 ||
		!strings.Contains(out.String(), `"verdict": "green"`) {
		t.Fatalf("%d %s %s", c, out.String(), errb.String())
	}
	if c := run(context.Background(), []string{"-repo", "o/r"}, stubGH{}, &out, &errb); c != 2 {
		t.Fatalf("missing sha: %d", c)
	}
}

type recGH struct{ args [][]string }

func (r *recGH) Run(_ context.Context, a ...string) ([]byte, error) {
	r.args = append(r.args, a)
	return []byte(`[{"databaseId":1,"workflowName":"build","status":"completed","conclusion":"success","attempt":1}]`), nil
}

// A non-factory repo is queried by name and echoed with its branch; nothing
// about pricec/factory is assumed.
func TestRunTargetsGivenRepo(t *testing.T) {
	var out, errb bytes.Buffer
	gh := &recGH{}
	args := []string{"-repo", "pricec/speech-service", "-branch", "speech-service-standalone", "-sha", "ace1b7d"}
	if c := run(context.Background(), args, gh, &out, &errb); c != 0 {
		t.Fatalf("%d %s %s", c, out.String(), errb.String())
	}
	for _, want := range []string{`"repo": "pricec/speech-service"`, `"branch": "speech-service-standalone"`, `"head": "ace1b7d"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s in %s", want, out.String())
		}
	}
	for _, call := range gh.args {
		if strings.Contains(strings.Join(call, " "), "pricec/factory") {
			t.Errorf("hardcoded factory repo: %v", call)
		}
	}
	if len(gh.args) == 0 || strings.Join(gh.args[0], " ") != "run list -R pricec/speech-service --commit ace1b7d --limit 100 --json databaseId,workflowName,status,conclusion,attempt" {
		t.Errorf("calls: %v", gh.args)
	}
}
