package claudeauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SetupSession drives `claude setup-token` headlessly (spec §13.3): the
// CLI's OAuth flow is interactive (it prints an authorize URL and waits
// for the code the browser shows after sign-in), so it runs under
// util-linux `script` for a pseudo-terminal. Start returns the URL the
// operator opens; Submit types the code the operator pastes back and
// returns the minted long-lived token. Verified on 2.1.325: the URL is
// printed (as an OSC 8 hyperlink and plain text) and a pasted code is sent
// to the token exchange (a bad code yields `OAuth error: That code wasn't
// accepted...`). The success screen is expected to print the
// `sk-ant-oat01-...` token, which Submit scrapes.
//
// The session holds the PKCE verifier in the child, so the code must go
// back to the SAME process; a session lives at most MaxAge.
type SetupSession struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	home    string
	started time.Time

	mu     sync.Mutex
	out    strings.Builder
	done   chan struct{}
	waitEr error
	closed bool
}

// SetupConfig configures a session.
type SetupConfig struct {
	// Bin is the Claude Code CLI ("" = claude).
	Bin string
	// Script is util-linux script ("" = script).
	Script string
	// URLTimeout bounds the wait for the authorize URL (default 45s).
	URLTimeout time.Duration
}

// MaxAge is how long a setup session waits for the operator's code.
const MaxAge = 6 * time.Hour

var (
	// osc8Re is an OSC 8 hyperlink's target: ESC ] 8 ; params ; URL ST.
	osc8Re = regexp.MustCompile(`\x1b\]8;[^;\x07\x1b]*;(https://[^\x07\x1b]+)`)
	// plainURLRe is the fallback on ANSI-stripped text.
	plainURLRe = regexp.MustCompile(`https://\S*/oauth/authorize\?\S+`)
	ansiRe     = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[=>]`)
	oauthErrRe = regexp.MustCompile(`(?i)oauth error[^\r\n]*`)
)

// ErrSetupUnavailable means the headless flow could not start here (no
// `script`, no CLI, or no URL appeared); the operator can still paste a
// token minted with `claude setup-token` on any machine.
var ErrSetupUnavailable = errors.New("headless claude setup-token unavailable")

// StartSetup launches `claude setup-token` under a pty and returns once
// the authorize URL is known.
func StartSetup(ctx context.Context, cfg SetupConfig) (*SetupSession, string, error) {
	bin, script := cfg.Bin, cfg.Script
	if bin == "" {
		bin = "claude"
	}
	if script == "" {
		script = "script"
	}
	if _, err := exec.LookPath(script); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrSetupUnavailable, err)
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrSetupUnavailable, err)
	}
	// A private, empty HOME: setup-token mints a token and prints it; it
	// must never touch the service user's own stored login.
	home, err := os.MkdirTemp("", "claude-setup-token-")
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrSetupUnavailable, err)
	}
	// A very wide terminal keeps the URL and token on one line each.
	inner := "stty cols 1000 rows 50 2>/dev/null; exec " + shellQuote(bin) + " setup-token"
	cmd := exec.Command(script, "-qfec", inner, "/dev/null")
	cmd.Dir = home
	cmd.Env = append(withEnv(withEnv(os.Environ(), "HOME", home), "TERM", "xterm"), "BROWSER=true")
	cmd.Env = withoutEnv(cmd.Env, TokenEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		os.RemoveAll(home)
		return nil, "", fmt.Errorf("%w: %v", ErrSetupUnavailable, err)
	}
	s := &SetupSession{cmd: cmd, stdin: stdin, home: home, started: time.Now(), done: make(chan struct{})}
	cmd.Stdout = (*sessionWriter)(s)
	cmd.Stderr = (*sessionWriter)(s)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(home)
		return nil, "", fmt.Errorf("%w: %v", ErrSetupUnavailable, err)
	}
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.waitEr = err
		s.mu.Unlock()
		close(s.done)
	}()
	wait := cfg.URLTimeout
	if wait <= 0 {
		wait = 45 * time.Second
	}
	url, err := s.waitFor(ctx, wait, func(out string) (string, bool) { return AuthorizeURL(out) })
	if err != nil {
		s.Close()
		return nil, "", fmt.Errorf("%w: no authorize URL: %v", ErrSetupUnavailable, err)
	}
	return s, url, nil
}

// AuthorizeURL extracts the OAuth authorize URL from raw CLI output.
func AuthorizeURL(raw string) (string, bool) {
	if m := osc8Re.FindStringSubmatch(raw); m != nil && strings.Contains(m[1], "/oauth/authorize?") {
		return m[1], true
	}
	if m := plainURLRe.FindString(stripANSI(raw)); m != "" {
		return m, true
	}
	return "", false
}

// Submit types code into the waiting prompt and waits (up to timeout) for
// the token. A rejected code returns the CLI's OAuth error text; the
// session is spent either way (a retry needs a fresh session and URL).
func (s *SetupSession) Submit(ctx context.Context, code string, timeout time.Duration) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" || strings.ContainsAny(code, "\r\n") {
		return "", errors.New("empty or multi-line code")
	}
	s.mu.Lock()
	mark := s.out.Len()
	s.mu.Unlock()
	if _, err := io.WriteString(s.stdin, code+"\r"); err != nil {
		return "", fmt.Errorf("write code: %w", err)
	}
	tok, err := s.waitFor(ctx, timeout, func(out string) (string, bool) {
		if len(out) < mark {
			return "", false
		}
		tail := stripANSI(out[mark:])
		if t, ok := FindToken(tail); ok {
			return t, true
		}
		if m := oauthErrRe.FindString(tail); m != "" {
			return "\x00" + m, true
		}
		return "", false
	})
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(tok, "\x00") {
		return "", errors.New(Redact(strings.TrimSpace(tok[1:]), code))
	}
	return tok, nil
}

// Expired reports whether the session is past MaxAge or its child exited.
func (s *SetupSession) Expired(now time.Time) bool {
	select {
	case <-s.done:
		return true
	default:
	}
	return now.Sub(s.started) > MaxAge
}

// Close kills the child (its whole process group) and removes the temp
// HOME. Idempotent.
func (s *SetupSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
	os.RemoveAll(s.home)
}

// waitFor polls the accumulated output until match succeeds, the child
// exits, ctx ends or timeout passes.
func (s *SetupSession) waitFor(ctx context.Context, timeout time.Duration, match func(string) (string, bool)) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		s.mu.Lock()
		out := s.out.String()
		s.mu.Unlock()
		if v, ok := match(out); ok {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("timed out after %s", timeout)
		case <-s.done:
			s.mu.Lock()
			out, werr := s.out.String(), s.waitEr
			s.mu.Unlock()
			if v, ok := match(out); ok {
				return v, nil
			}
			return "", fmt.Errorf("claude setup-token exited (%v)", werr)
		case <-tick.C:
		}
	}
}

type sessionWriter SetupSession

func (w *sessionWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out.Len() < 1<<20 {
		w.out.Write(p)
	}
	return len(p), nil
}

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func withoutEnv(base []string, key string) []string {
	prefix := key + "="
	out := base[:0:0]
	for _, e := range base {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}
