// Command auditcheck is the auditor's diff guardrail (spec §6.12) as a CI
// check: it reads `git diff --numstat -z -M <master>...<head>` on stdin
// and applies auditor.CheckDiff (non-empty, text only, at most 150 changed
// lines, no denylisted path on either side of a rename). It runs no git
// itself. CI builds it from MASTER's checkout, never from the branch
// under test, so a fix branch cannot weaken its own check
// (.github/workflows/audit-guardrail.yml, a pull_request_target workflow,
// so GitHub also runs master's copy of the workflow itself).
//
// Exit status: 0 allowed, 1 refused by the guardrail, 2 usage/input error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pricec/factory/internal/auditor"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auditcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	maxLines := fs.Int("max-lines", auditor.HardMaxChangedLines,
		fmt.Sprintf("changed-line cap (clamped to %d)", auditor.HardMaxChangedLines))
	if err := fs.Parse(args); err != nil {
		return 2
	}
	limit := *maxLines
	if limit <= 0 || limit > auditor.HardMaxChangedLines {
		limit = auditor.HardMaxChangedLines
	}
	files, err := auditor.ReadDiff(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "auditcheck: read diff: %v\n", err)
		return 2
	}
	lines, _ := auditor.ChangedLines(files)
	if err := auditor.CheckDiff(files, limit); err != nil {
		if errors.Is(err, auditor.ErrGuardrail) {
			fmt.Fprintf(stdout, "auditor guardrail: FAIL: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "auditcheck: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "auditor guardrail: ok: %d files, %d changed lines (cap %d), no forbidden areas, no binaries\n",
		len(files), lines, limit)
	return 0
}
