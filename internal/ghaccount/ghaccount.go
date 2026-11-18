// Package ghaccount picks, for one GitHub repository, a gh CLI account
// that can actually write to it. The factory host's gh may be logged in
// to several accounts, and the globally active one is not necessarily a
// collaborator on the repo a job works on (e.g. `gh pr create` fails with
// "must be a collaborator"). Flipping the active account (`gh auth
// switch`) would race with concurrent jobs, so instead the chosen
// account's token is handed to individual commands through GH_TOKEN
// (which both gh and the `gh auth git-credential` git helper honour).
package ghaccount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// DefaultHost is the gh host used when none is given.
const DefaultHost = "github.com"

// TokenEnv lists the environment variables through which gh reads a token
// that overrides its stored accounts. The runner strips them before
// listing accounts, so the stored accounts are always what is iterated.
var TokenEnv = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// GH runs `gh <args>` with the token variables in TokenEnv removed from
// the environment and env (KEY=VALUE pairs) added, returning stdout.
// Tests mock it.
type GH interface {
	Run(ctx context.Context, env []string, args ...string) ([]byte, error)
}

// Account is one logged-in gh account.
type Account struct {
	Host   string `json:"host"`
	Login  string `json:"login"`
	Active bool   `json:"active"`
	State  string `json:"state"`
}

// Accounts lists the accounts gh is logged in to on host.
func Accounts(ctx context.Context, gh GH, host string) ([]Account, error) {
	// gh auth status exits non-zero when any account has a bad token, but
	// still prints the JSON; parse whatever came back before giving up.
	out, runErr := gh.Run(ctx, nil, "auth", "status", "--hostname", host, "--json", "hosts")
	var st struct {
		Hosts map[string][]Account `json:"hosts"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("gh auth status: %w", runErr)
		}
		return nil, fmt.Errorf("gh auth status: bad json: %w", err)
	}
	accts := st.Hosts[host]
	if len(accts) == 0 {
		return nil, fmt.Errorf("gh is not logged in to any account on %s", host)
	}
	return accts, nil
}

// Order returns accts in the order they are tried for a repo owned by
// owner: the owner's own account first, then the active account, then
// the rest in gh's order. The sort is stable.
func Order(accts []Account, owner string) []Account {
	out := append([]Account(nil), accts...)
	rank := func(a Account) int {
		switch {
		case strings.EqualFold(a.Login, owner):
			return 0
		case a.Active:
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// Selection is the account chosen for a repo, with its token.
type Selection struct {
	Account Account
	Token   string
}

// Env returns the variables that make gh (and git through gh's credential
// helper) act as the selected account.
func (s Selection) Env() []string {
	if s.Account.Host == "" || s.Account.Host == DefaultHost {
		return []string{"GH_TOKEN=" + s.Token}
	}
	return []string{"GH_ENTERPRISE_TOKEN=" + s.Token, "GH_HOST=" + s.Account.Host}
}

// ErrNoAccount is wrapped by Select when no account qualifies.
var ErrNoAccount = errors.New("no logged-in gh account can push")

// SplitRepo splits OWNER/REPO.
func SplitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repo %q: want OWNER/REPO", repo)
	}
	return owner, name, nil
}

// Select picks the first account (in Order) whose token has push, maintain
// or admin permission on repo (OWNER/REPO) on host. It never changes gh's
// active account. When none qualifies the error wraps ErrNoAccount and
// names every account tried and why it was rejected.
func Select(ctx context.Context, gh GH, host, repo string) (Selection, error) {
	if host == "" {
		host = DefaultHost
	}
	owner, _, err := SplitRepo(repo)
	if err != nil {
		return Selection{}, err
	}
	accts, err := Accounts(ctx, gh, host)
	if err != nil {
		return Selection{}, err
	}
	var tried []string
	for _, a := range Order(accts, owner) {
		if a.Host == "" {
			a.Host = host
		}
		ok, why, tok := check(ctx, gh, a, repo)
		if ok {
			return Selection{Account: a, Token: tok}, nil
		}
		tried = append(tried, a.Login+" ("+why+")")
	}
	return Selection{}, fmt.Errorf("%w to %s on %s; tried: %s", ErrNoAccount, repo, host, strings.Join(tried, ", "))
}

// check reports whether account a can push to repo, with a short reason
// when it cannot, and the account's token when it can.
func check(ctx context.Context, gh GH, a Account, repo string) (bool, string, string) {
	if a.State != "" && a.State != "success" {
		return false, "auth state " + a.State, ""
	}
	out, err := gh.Run(ctx, nil, "auth", "token", "--hostname", a.Host, "--user", a.Login)
	if err != nil {
		return false, "no token: " + oneLine(err.Error()), ""
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return false, "no token", ""
	}
	env := Selection{Account: a, Token: tok}.Env()
	out, err = gh.Run(ctx, env, "api", "--hostname", a.Host, "repos/"+repo, "--jq", ".permissions")
	if err != nil {
		return false, "repo not visible: " + oneLine(err.Error()), ""
	}
	var p struct{ Admin, Maintain, Push bool }
	if err := json.Unmarshal(out, &p); err != nil {
		return false, "bad permissions json", ""
	}
	if !p.Push && !p.Maintain && !p.Admin {
		return false, "no push permission", ""
	}
	return true, "", tok
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
