package ghaccount

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeGH answers gh auth status / auth token / api from tables.
type fakeGH struct {
	status string            // auth status JSON
	tokens map[string]string // login -> token ("" = gh auth token fails)
	perms  map[string]string // token -> permissions JSON ("" = api fails)
	calls  []string
}

func (f *fakeGH) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(env, " ")+"|"+strings.Join(args, " "))
	switch {
	case len(args) >= 2 && args[0] == "auth" && args[1] == "status":
		return []byte(f.status), nil
	case len(args) >= 2 && args[0] == "auth" && args[1] == "token":
		user := args[len(args)-1]
		if t := f.tokens[user]; t != "" {
			return []byte(t + "\n"), nil
		}
		return nil, errors.New("no token for " + user)
	case args[0] == "api":
		var tok string
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "GH_TOKEN="); ok {
				tok = v
			}
		}
		if p := f.perms[tok]; p != "" {
			return []byte(p), nil
		}
		return nil, errors.New("HTTP 404: Not Found")
	}
	return nil, fmt.Errorf("unexpected gh %v", args)
}

func status(accts ...string) string {
	var parts []string
	for _, a := range accts {
		login, active := strings.CutSuffix(a, "*") // trailing * marks the active account
		parts = append(parts, fmt.Sprintf(`{"state":"success","active":%v,"host":"github.com","login":%q}`, active, login))
	}
	return `{"hosts":{"github.com":[` + strings.Join(parts, ",") + `]}}`
}

const (
	push   = `{"admin":false,"maintain":false,"push":true,"pull":true}`
	pull   = `{"admin":false,"maintain":false,"push":false,"pull":true}`
	admin  = `{"admin":true,"maintain":true,"push":true,"pull":true}`
	expAct = "chris-qunify*"
)

func TestSelect(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		tokens    map[string]string
		perms     map[string]string
		repo      string
		want      string // login, or "" for error
		wantErr   []string
		wantNoTry string // login that must not be queried
	}{
		{
			name:   "active account lacks access; owner account picked",
			status: status(expAct, "pricec"),
			tokens: map[string]string{"chris-qunify": "t-cq", "pricec": "t-pc"},
			perms:  map[string]string{"t-cq": pull, "t-pc": admin},
			repo:   "pricec/factory", want: "pricec",
			wantNoTry: "chris-qunify",
		},
		{
			name:   "owner account preferred even when active also qualifies",
			status: status(expAct, "pricec"),
			tokens: map[string]string{"chris-qunify": "t-cq", "pricec": "t-pc"},
			perms:  map[string]string{"t-cq": push, "t-pc": push},
			repo:   "PriceC/factory", want: "pricec",
		},
		{
			name:   "org repo: active account tried first",
			status: status("pricec", expAct),
			tokens: map[string]string{"chris-qunify": "t-cq", "pricec": "t-pc"},
			perms:  map[string]string{"t-cq": push, "t-pc": push},
			repo:   "qunify/app", want: "chris-qunify",
		},
		{
			name:   "org repo: falls through to a non-active collaborator",
			status: status(expAct, "pricec"),
			tokens: map[string]string{"chris-qunify": "t-cq", "pricec": "t-pc"},
			perms:  map[string]string{"t-pc": push},
			repo:   "someorg/x", want: "pricec",
		},
		{
			name:   "none qualifies: error lists every account and reason",
			status: status(expAct, "pricec", "ghost"),
			tokens: map[string]string{"chris-qunify": "t-cq", "pricec": "t-pc"},
			perms:  map[string]string{"t-pc": pull},
			repo:   "pricec/factory",
			wantErr: []string{
				"no logged-in gh account can push to pricec/factory",
				"pricec (no push permission)", "chris-qunify (repo not visible", "ghost (no token",
			},
		},
		{
			name:    "not logged in",
			status:  `{"hosts":{}}`,
			repo:    "pricec/factory",
			wantErr: []string{"not logged in"},
		},
		{
			name:    "bad repo",
			status:  status("pricec"),
			repo:    "factory",
			wantErr: []string{"want OWNER/REPO"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := &fakeGH{status: tc.status, tokens: tc.tokens, perms: tc.perms}
			sel, err := Select(context.Background(), gh, "", tc.repo)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("got %+v, want error", sel)
				}
				for _, w := range tc.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q lacks %q", err, w)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sel.Account.Login != tc.want {
				t.Fatalf("login = %q, want %q", sel.Account.Login, tc.want)
			}
			if got, want := sel.Env(), "GH_TOKEN="+tc.tokens[tc.want]; len(got) != 1 || got[0] != want {
				t.Fatalf("env = %v, want [%s]", got, want)
			}
			for _, c := range gh.calls {
				if strings.Contains(c, "switch") {
					t.Fatalf("must never switch the active account: %s", c)
				}
				if tc.wantNoTry != "" && strings.HasSuffix(c, "--user "+tc.wantNoTry) {
					t.Fatalf("queried %s after the owner account qualified", tc.wantNoTry)
				}
			}
		})
	}
}

func TestErrNoAccountWrapped(t *testing.T) {
	gh := &fakeGH{status: status("pricec"), tokens: map[string]string{"pricec": "t"}, perms: map[string]string{"t": pull}}
	_, err := Select(context.Background(), gh, "github.com", "a/b")
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
}

func TestAuthStatusFailingButJSON(t *testing.T) {
	// gh auth status exits 1 when any account's token is bad, yet prints JSON.
	gh := failingStatus{`{"hosts":{"github.com":[{"state":"error","login":"old"},{"state":"success","login":"pricec","active":true}]}}`}
	accts, err := Accounts(context.Background(), gh, "github.com")
	if err != nil || len(accts) != 2 {
		t.Fatalf("accts=%v err=%v", accts, err)
	}
	ok, why, _ := check(context.Background(), gh, accts[0], "a/b")
	if ok || why != "auth state error" {
		t.Fatalf("check bad account: ok=%v why=%q", ok, why)
	}
}

type failingStatus struct{ out string }

func (f failingStatus) Run(context.Context, []string, ...string) ([]byte, error) {
	return []byte(f.out), errors.New("exit status 1")
}

func TestEnvEnterprise(t *testing.T) {
	got := Selection{Account: Account{Host: "ghe.example.com"}, Token: "x"}.Env()
	if strings.Join(got, " ") != "GH_ENTERPRISE_TOKEN=x GH_HOST=ghe.example.com" {
		t.Fatalf("env = %v", got)
	}
}
