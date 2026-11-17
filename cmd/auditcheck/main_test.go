package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	for _, c := range []struct {
		name, diff string
		args       []string
		code       int
		out        string
	}{
		{"allowed", "3\t1\tdocs/a.md\x00", nil, 0, "ok: 1 files, 4 changed lines"},
		{"plain numstat", "3\t1\tdocs/a.md\n", nil, 0, "ok"},
		{"over the cap", "151\t0\tdocs/a.md\x00", nil, 1, "FAIL"},
		{"cap cannot be raised", "151\t0\tdocs/a.md\x00", []string{"-max-lines", "1000"}, 1, "FAIL"},
		{"lower cap", "20\t0\tdocs/a.md\x00", []string{"-max-lines", "10"}, 1, "FAIL"},
		{"CI config", "1\t0\t.github/workflows/ci.yml\x00", nil, 1, "FAIL"},
		{"the checker itself", "1\t0\tcmd/auditcheck/main.go\x00", nil, 1, "FAIL"},
		{"rename into auditor", "0\t0\t\x00docs/x.go\x00internal/auditor/x.go\x00", nil, 1, "FAIL"},
		{"binary", "-\t-\tlogo.png\x00", nil, 1, "FAIL"},
		{"empty diff", "", nil, 1, "FAIL"},
		{"bad flag", "", []string{"-nope"}, 2, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run(c.args, strings.NewReader(c.diff), &out, &errb)
			if code != c.code || !strings.Contains(out.String(), c.out) {
				t.Fatalf("code=%d out=%q err=%q; want code %d containing %q", code, out.String(), errb.String(), c.code, c.out)
			}
		})
	}
}
