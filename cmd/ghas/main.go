// Command ghas runs a command as a gh account that can push to a repo.
//
// The host's gh may be logged in to several GitHub accounts, and the
// active one may not be a collaborator on the repo at hand. ghas picks
// the account (see internal/ghaccount: the repo owner's own account
// first, then the active one, then the rest; the first with push access
// wins) and runs the command with GH_TOKEN set to that account's token.
// It never switches gh's active account, so concurrent jobs are not
// affected. git over https picks the token up through gh's credential
// helper, so `ghas -repo R git push ...` works too.
//
//	ghas -repo OWNER/REPO gh pr create -R OWNER/REPO ...
//	ghas -repo OWNER/REPO git push origin HEAD
//	ghas -repo OWNER/REPO -login        # print the chosen login only
//
// Exit status: the command's own status; 2 for usage errors, 3 when no
// account can push to the repo (the message lists the accounts tried).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/pricec/factory/internal/ghaccount"
)

type ghExec struct{}

func (ghExec) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(scrubEnv(os.Environ()), env...)
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

// scrubEnv drops token variables so gh reports its stored accounts.
func scrubEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(ghaccount.TokenEnv, k)
	})
}

// runCmd runs argv with env added (token variables replaced) and stdio
// passed through, returning its exit status.
type runCmd func(ctx context.Context, env []string, argv []string, stdin io.Reader, stdout, stderr io.Writer) int

func execCmd(ctx context.Context, env []string, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(scrubEnv(os.Environ()), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		fmt.Fprintf(stderr, "ghas: %v\n", err)
		return 127
	}
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], ghExec{}, execCmd, os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, gh ghaccount.GH, exe runCmd, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ghas", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "OWNER/REPO the command acts on (required)")
	host := fs.String("host", ghaccount.DefaultHost, "gh host")
	login := fs.Bool("login", false, "print the chosen account's login and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if *repo == "" || (!*login && len(argv) == 0) {
		fmt.Fprintln(stderr, "usage: ghas -repo OWNER/REPO [-host H] (-login | COMMAND [ARGS...])")
		return 2
	}
	sel, err := ghaccount.Select(ctx, gh, *host, *repo)
	if err != nil {
		fmt.Fprintf(stderr, "ghas: %v\n", err)
		if errors.Is(err, ghaccount.ErrNoAccount) {
			return 3
		}
		return 2
	}
	if *login {
		fmt.Fprintln(stdout, sel.Account.Login)
		return 0
	}
	fmt.Fprintf(stderr, "ghas: acting as %s for %s\n", sel.Account.Login, *repo)
	return exe(ctx, sel.Env(), argv, stdin, stdout, stderr)
}
