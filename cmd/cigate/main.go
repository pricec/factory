// Command cigate is the merge procedure's CI gate (docs/merging.md step 5).
// It re-runs red checks once before declaring a head dead and prints the
// verdict as JSON; see internal/cigate.
//
// Exit status: 0 green, 1 red/no checks/timeout, 2 usage or gh error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/pricec/factory/internal/cigate"
)

type ghExec struct{}

func (ghExec) Run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return out, fmt.Errorf("%w: %s", err, ee.Stderr)
	}
	return out, err
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], ghExec{}, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, gh cigate.GH, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cigate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "OWNER/REPO")
	sha := fs.String("sha", "", "full head SHA")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *repo == "" || *sha == "" {
		fmt.Fprintln(stderr, "cigate: -repo and -sha are required")
		return 2
	}
	g := &cigate.Gate{GH: gh, Repo: *repo}
	res, err := g.Check(ctx, *sha)
	if err != nil {
		fmt.Fprintf(stderr, "cigate: %v\n", err)
		return 2
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
	if res.Verdict != cigate.Green {
		return 1
	}
	return 0
}
