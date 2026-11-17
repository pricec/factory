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
