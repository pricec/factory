package claudeauth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tok = "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz_ABCDEFGH-0123456789"

func TestIsAuthFailure(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"Not logged in · Please run /login", true},
		{"claude run: exit status 1; stdout: Not logged in · Please run /login", true},
		{"Failed to authenticate. API Error: 401 OAuth access token is invalid.", true},
		{"OAuth token has expired. Please obtain a new token or refresh your existing token.", true},
		{"Invalid API key · Please run /login", true},
		{`{"type":"error","error":{"type":"authentication_error","message":"x"}}`, true},
		{"gh: You are not logged into any GitHub hosts. Run gh auth login", false},
		{"Claude AI usage limit reached|1763000000", false},
		{"job exceeded timeout_s=60 ceiling", false},
		{"", false},
	} {
		if got := IsAuthFailure(tc.text); got != tc.want {
			t.Errorf("IsAuthFailure(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestFindTokenAndRedact(t *testing.T) {
	if got, ok := FindToken("here it is:\n  " + tok + "\nthanks"); !ok || got != tok {
		t.Fatalf("FindToken = %q %v", got, ok)
	}
	if _, ok := FindToken("sk-ant-api03-notanoauthtoken0123456789abcdef"); ok {
		t.Fatal("an API key is not an OAuth token")
	}
	if _, ok := FindToken("no token here"); ok {
		t.Fatal("false positive")
	}
	red := Redact("a "+tok+" b sk-ant-api03-zzzzzzzzzzzzzzzz c my-code", "my-code")
	if strings.Contains(red, "sk-ant-") || strings.Contains(red, "my-code") || strings.Count(red, "[REDACTED]") != 3 {
		t.Fatalf("Redact = %q", red)
	}
}

func TestRunsClaude(t *testing.T) {
	for _, tc := range []struct {
		model, tier string
		tiers       []string
		want        bool
	}{
		{"claude-opus-5-5", "", []string{}, true},
		{"anthropic/claude-sonnet-5-5", "flash", nil, true},
		{"openrouter/z-ai/glm-5.3", "hardest", []string{"hardest"}, false},
		{"", "hardest", []string{"hardest"}, true},
		{"", "", []string{"flash"}, true}, // no tier = flash
		{"", "default", []string{"hardest"}, false},
		{"", "default", nil, true}, // unknown tiers: conservative
	} {
		if got := RunsClaude(tc.model, tc.tier, tc.tiers); got != tc.want {
			t.Errorf("RunsClaude(%q,%q,%v) = %v, want %v", tc.model, tc.tier, tc.tiers, got, tc.want)
		}
	}
}

func TestProbeClassifies(t *testing.T) {
	for _, tc := range []struct {
		name           string
		stdout, stderr string
		err            error
		want           string
	}{
		{"ok", `{"type":"result","is_error":false,"result":"OK"}`, "", nil, StateOK},
		{"not logged in", `{"type":"result","is_error":true,"result":"Not logged in · Please run /login"}`, "", nil, StateFailed},
		{"401", `{"is_error":true,"api_error_status":401,"result":"Failed to authenticate."}`, "", nil, StateFailed},
		{"auth via stderr", "", "Not logged in · Please run /login", errors.New("exit status 1"), StateFailed},
		{"usage limit", `{"is_error":true,"result":"Claude AI usage limit reached"}`, "", nil, StateUnknown},
		{"network", "", "ECONNREFUSED", errors.New("exit status 1"), StateUnknown},
		{"not installed", "", "", exec.ErrNotFound, StateUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotEnv, gotArgs []string
			p := Prober{Run: func(_ context.Context, _ string, args, env []string, _ string) (string, string, error) {
				gotEnv, gotArgs = env, args
				return tc.stdout + " " + tok, tc.stderr, tc.err
			}}
			if tc.stdout != "" {
				p.Run = func(_ context.Context, _ string, args, env []string, _ string) (string, string, error) {
					gotEnv, gotArgs = env, args
					return tc.stdout, tc.stderr + " " + tok, tc.err
				}
			}
			out := p.Probe(context.Background(), tok, true)
			if out.State != tc.want {
				t.Fatalf("state = %s (%s), want %s", out.State, out.Detail, tc.want)
			}
			if strings.Contains(out.Detail, tok) {
				t.Fatalf("token leaked into detail: %q", out.Detail)
			}
			var hasTok, isolated bool
			for _, e := range gotEnv {
				hasTok = hasTok || e == TokenEnv+"="+tok
				isolated = isolated || strings.HasPrefix(e, "HOME="+os.TempDir())
			}
			if !hasTok || !isolated {
				t.Fatalf("env: token=%v isolated home=%v", hasTok, isolated)
			}
			if strings.Join(gotArgs, " ") != strings.Join(ProbeArgs(), " ") {
				t.Fatalf("args = %v", gotArgs)
			}
		})
	}
}

func TestAuthorizeURL(t *testing.T) {
	// Shape observed from `claude setup-token` 2.1.325 under a pty.
	raw := "Browser didn't open? Use the url below to sign in (c to copy)\r\n" +
		"\x1b]8;id=1ircrrv;https://claude.com/cai/oauth/authorize?code=true&client_id=x&state=y\x1b\\https://claude.com/cai/oauth/au\x1b]8;;\x1b\\\r\n"
	got, ok := AuthorizeURL(raw)
	if !ok || got != "https://claude.com/cai/oauth/authorize?code=true&client_id=x&state=y" {
		t.Fatalf("AuthorizeURL = %q %v", got, ok)
	}
	got, ok = AuthorizeURL("\x1b[1mhttps://claude.com/cai/oauth/authorize?code=true&s=1\x1b[0m\r\nPaste code here if prompted >")
	if !ok || got != "https://claude.com/cai/oauth/authorize?code=true&s=1" {
		t.Fatalf("plain AuthorizeURL = %q %v", got, ok)
	}
}

// fakeCLI writes a stand-in `claude` that mimics setup-token: OSC 8 URL,
// a prompt, then a token for "good-code" or the CLI's OAuth error.
func fakeCLI(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("util-linux script not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	body := `#!/bin/sh
[ "$1" = setup-token ] || exit 2
[ -z "$CLAUDE_CODE_OAUTH_TOKEN" ] || { echo "token leaked into env"; exit 3; }
printf 'Browser did not open? Use the url below\r\n\033]8;id=1;https://claude.com/cai/oauth/authorize?code=true&state=s1\033\\link\033]8;;\033\\\r\n'
printf 'Paste code here if prompted > '
read code
code=$(printf '%s' "$code" | tr -d '\r')
if [ "$code" = good-code ]; then
  printf '\r\n\033[32mLong-lived authentication token created successfully!\033[0m\r\nYour OAuth token (valid for 1 year):\r\n\r\n` + tok + `\r\n'
else
  printf "\r\nOAuth error: That code wasn't accepted.\r\nPress Enter to retry.\r\n"
fi
sleep 30
`
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestSetupSessionUnderPTY(t *testing.T) {
	bin := fakeCLI(t)
	t.Setenv(TokenEnv, "must-not-leak")
	ctx := context.Background()
	s, url, err := StartSetup(ctx, SetupConfig{Bin: bin, URLTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("StartSetup: %v", err)
	}
	defer s.Close()
	if url != "https://claude.com/cai/oauth/authorize?code=true&state=s1" {
		t.Fatalf("url = %q", url)
	}
	got, err := s.Submit(ctx, "good-code", 10*time.Second)
	if err != nil || got != tok {
		t.Fatalf("Submit = %q, %v", got, err)
	}

	s2, _, err := StartSetup(ctx, SetupConfig{Bin: bin, URLTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("StartSetup 2: %v", err)
	}
	defer s2.Close()
	if _, err := s2.Submit(ctx, "bad-code", 10*time.Second); err == nil || !strings.Contains(err.Error(), "OAuth error") {
		t.Fatalf("bad code err = %v", err)
	}
	s2.Close()
	if !s2.Expired(time.Now()) {
		t.Fatal("closed session must report expired")
	}
}

func TestStartSetupUnavailable(t *testing.T) {
	_, _, err := StartSetup(context.Background(), SetupConfig{Bin: "/nonexistent/claude"})
	if !errors.Is(err, ErrSetupUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
