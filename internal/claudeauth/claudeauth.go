// Package claudeauth is the Claude Code CLI authentication health kit
// (spec §13.3): it recognises the CLI's not-logged-in / expired-auth
// failures, runs the cheap auth probe a foreman (or the factory) uses to
// confirm a credential works, recognises and redacts long-lived OAuth
// tokens (`claude setup-token`, `sk-ant-oat..-`), and defines the small
// health report foremen push with their capabilities. It is shared by the
// factory (detection from job results, operator re-login flow) and the
// foreman (probe loop, token delivery to claude children).
package claudeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	// SecretName is the credentials-store secret (spec §13) holding the
	// long-lived OAuth token minted by `claude setup-token`.
	SecretName = "claude-oauth-token"
	// TokenEnv is the env var the Claude Code CLI reads the token from.
	TokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

	// Health states (spec §13.3).
	StateOK      = "ok"      // the last probe (or run) authenticated
	StateFailed  = "failed"  // the CLI is not logged in / auth rejected
	StateUnknown = "unknown" // never probed, or the probe failed for a non-auth reason

	// ProbeModel is the cheapest model alias; the probe costs ~$0.0001.
	ProbeModel = "haiku"
	// ProbeTimeout bounds one probe run.
	ProbeTimeout = 90 * time.Second

	redacted = "[REDACTED]"
)

// authFailureRe matches the CLI's own auth-failure texts (observed on
// 2.1.325: `Not logged in · Please run /login` with no credentials, and
// `Failed to authenticate. API Error: 401 OAuth access token is invalid.`
// for a rejected token), plus the expired / revoked variants. The phrases
// are specific to the Claude CLI on purpose: a job whose own work fails on
// some other tool's login (gh, docker) must not trip the claude gate.
var authFailureRe = regexp.MustCompile(`(?i)not logged in\s*·\s*please run /login|please run /login|failed to authenticate\. api error: 40[13]|oauth (access )?token (is invalid|has expired|has been revoked)|invalid api key\s*·|"type"\s*:\s*"authentication_error"|api_error_type"\s*:\s*"authentication_error"`)

// IsAuthFailure reports whether text (a job error, a run's stdout/stderr)
// reads as a Claude CLI authentication failure.
func IsAuthFailure(text string) bool { return authFailureRe.MatchString(text) }

// tokenRe matches a Claude long-lived OAuth token (`claude setup-token`
// prints `sk-ant-oat01-...`). Anthropic API keys (`sk-ant-api..-`) are a
// different credential and deliberately not matched.
var tokenRe = regexp.MustCompile(`sk-ant-oat\d{2}-[A-Za-z0-9_-]{20,}`)

// anyKeyRe is wider than tokenRe: every Anthropic credential shape, for
// redaction only.
var anyKeyRe = regexp.MustCompile(`sk-ant-[A-Za-z0-9]{2,8}-[A-Za-z0-9_-]{8,}`)

// FindToken returns the first OAuth token in text, if any.
func FindToken(text string) (string, bool) {
	t := tokenRe.FindString(text)
	return t, t != ""
}

// Redact removes every Anthropic credential shape from s, plus the exact
// extra values given (e.g. the token in hand), so a message can be logged
// or posted to an operator thread.
func Redact(s string, extra ...string) string {
	for _, v := range extra {
		if v != "" {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	return anyKeyRe.ReplaceAllString(s, redacted)
}

// Report is the `claude_auth` object a foreman adds to its 60s
// capabilities push (spec §13.3).
type Report struct {
	// State is ok | failed | unknown.
	State string `json:"state"`
	// Detail is the redacted, capped reason of the last probe.
	Detail string `json:"detail,omitempty"`
	// CheckedAt is the last probe time (RFC3339), "" = never.
	CheckedAt string `json:"checked_at,omitempty"`
	// TokenRev is the factory token revision the foreman holds ("" =
	// none: the CLI's own login on that host is used).
	TokenRev string `json:"token_rev,omitempty"`
	// ClaudeTiers lists the job tiers whose worker model routes to the
	// Claude CLI on this foreman (so the factory knows which jobs a
	// failed login blocks).
	ClaudeTiers []string `json:"claude_tiers"`
}

// Directive is the `claude_auth` object of the capabilities-push response
// (spec §13.3): the factory tells the foreman which token revision is
// current and whether to probe now.
type Directive struct {
	// TokenRev is the current factory token revision ("" = no token
	// stored: the foreman drops any token it holds).
	TokenRev string `json:"token_rev"`
	// Probe asks the foreman to run its auth probe at once (a job just
	// failed on auth there, or the operator asked for a re-check).
	Probe bool `json:"probe,omitempty"`
}

// RunsClaude reports whether a job with this explicit model and tier runs
// on the Claude CLI on a foreman whose claude tiers are tiers. nil tiers
// (an older foreman that does not report) count as "every tier".
func RunsClaude(model, tier string, tiers []string) bool {
	if model != "" {
		return IsModel(model)
	}
	if tiers == nil {
		return true
	}
	if tier == "" {
		tier = "flash"
	}
	for _, t := range tiers {
		if t == tier {
			return true
		}
	}
	return false
}

// IsModel mirrors the worker routing (spec §8.4): bare claude-* ids or the
// `claude` alias, optionally behind `anthropic/`.
func IsModel(model string) bool {
	m := strings.TrimPrefix(model, "anthropic/")
	return m == "claude" || strings.HasPrefix(m, "claude-")
}

// ProbeArgs is the probe's argument vector: one print-mode turn on the
// cheapest model, no tools, no MCP servers, no persisted session, JSON
// output so the outcome is machine-readable.
func ProbeArgs() []string {
	return []string{
		"-p", "--output-format", "json", "--no-session-persistence",
		"--model", ProbeModel, "--strict-mcp-config", "--tools", "",
		"--system-prompt", "Reply with the single word OK.", "ping",
	}
}

// Outcome is one probe's verdict.
type Outcome struct {
	State  string // ok | failed | unknown
	Detail string // redacted, at most 200 chars
}

// Prober runs the auth probe. The zero value runs `claude` from PATH.
type Prober struct {
	// Bin is the Claude Code CLI ("" = claude on PATH).
	Bin string
	// Run is a test seam: it returns the child's stdout, stderr and wait
	// error. nil runs the real binary.
	Run func(ctx context.Context, bin string, args, env []string, dir string) (stdout, stderr string, err error)
}

// Probe runs one auth check. token, when non-empty, rides the child env
// as CLAUDE_CODE_OAUTH_TOKEN; isolatedHome additionally points HOME at a
// fresh empty dir so stored credentials cannot mask a bad token (used to
// verify an operator-submitted token before it is stored). The token never
// appears in the returned detail.
func (p Prober) Probe(ctx context.Context, token string, isolatedHome bool) Outcome {
	dir, err := os.MkdirTemp("", "claude-auth-probe-")
	if err != nil {
		return Outcome{State: StateUnknown, Detail: "probe work dir: " + err.Error()}
	}
	defer os.RemoveAll(dir)
	env := os.Environ()
	if token != "" {
		env = withEnv(env, TokenEnv, token)
	}
	if isolatedHome {
		env = withEnv(env, "HOME", dir)
	}
	bin := p.Bin
	if bin == "" {
		bin = "claude"
	}
	run := p.Run
	if run == nil {
		run = runCLI
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	stdout, stderr, runErr := run(ctx, bin, ProbeArgs(), env, dir)
	return classify(stdout, stderr, runErr, token)
}

// classify turns one probe run into an Outcome.
func classify(stdout, stderr string, runErr error, token string) Outcome {
	var out struct {
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
		Status  *int   `json:"api_error_status"`
		ErrType string `json:"api_error_type"`
	}
	parsed := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &out) == nil
	text := strings.TrimSpace(out.Result)
	if !parsed {
		text = strings.TrimSpace(stdout)
	}
	if t := strings.TrimSpace(stderr); t != "" {
		text = strings.TrimSpace(text + " " + t)
	}
	detail := capRunes(Redact(text, token), 200)
	authErr := IsAuthFailure(text) || out.ErrType == "authentication_error" ||
		(out.Status != nil && (*out.Status == 401 || *out.Status == 403))
	switch {
	case authErr:
		return Outcome{State: StateFailed, Detail: detail}
	case runErr == nil && parsed && !out.IsError:
		return Outcome{State: StateOK, Detail: "probe OK"}
	case runErr != nil && (errors.Is(runErr, exec.ErrNotFound) || errors.Is(runErr, os.ErrNotExist)):
		return Outcome{State: StateUnknown, Detail: "claude CLI not found: " + runErr.Error()}
	default:
		if detail == "" && runErr != nil {
			detail = capRunes(Redact(runErr.Error(), token), 200)
		}
		return Outcome{State: StateUnknown, Detail: "probe inconclusive: " + detail}
	}
}

func runCLI(ctx context.Context, bin string, args, env []string, dir string) (string, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		err = fmt.Errorf("probe timed out after %s", ProbeTimeout)
	}
	return stdout.String(), stderr.String(), err
}

func withEnv(base []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return append(out, prefix+value)
}

func capRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
