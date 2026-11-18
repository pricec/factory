// Claude CLI auth health on the foreman (spec §13.3): the foreman holds
// the factory's long-lived OAuth token (in memory only) and hands it to
// every claude child (workers and the brain) as CLAUDE_CODE_OAUTH_TOKEN;
// it probes auth at start, every claudeProbeEvery, whenever the factory
// asks (a job just failed on auth here) and whenever the token changes,
// and reports the verdict in its capabilities push so the factory can
// hold claude jobs off a logged-out foreman and release them once the
// probe passes.
package foremanapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pricec/factory/internal/claudeauth"
	"github.com/pricec/factory/internal/wire"
)

const (
	// claudeProbeEvery is the proactive probe cadence: a logged-out or
	// expired credential is caught within this window even when no job
	// runs (one haiku turn, ~$0.0001).
	claudeProbeEvery = 30 * time.Minute
	// claudeProbeMinGap debounces factory-requested probes.
	claudeProbeMinGap = 30 * time.Second
	maxTokenResponse  = 64 << 10
)

// claudeAuth is the foreman's auth-health state and token holder.
type claudeAuth struct {
	mu        sync.Mutex
	token     string // never logged
	rev       string
	state     string
	detail    string
	checkedAt time.Time
	probing   bool
	forced    bool // the next kicked probe ignores the debounce (token changed)

	prober claudeauth.Prober
	kick   chan struct{}
}

func newClaudeAuth() *claudeAuth {
	return &claudeAuth{state: claudeauth.StateUnknown, kick: make(chan struct{}, 1)}
}

// Env returns the env to merge into a claude child (nil when the factory
// holds no token: the host's own CLI login is used).
func (c *claudeAuth) Env() map[string]string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		return nil
	}
	return map[string]string{claudeauth.TokenEnv: c.token}
}

// report snapshots the caps-push report.
func (c *claudeAuth) report(tiers []string) *claudeauth.Report {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r := claudeauth.Report{State: c.state, Detail: c.detail, TokenRev: c.rev, ClaudeTiers: tiers}
	if !c.checkedAt.IsZero() {
		r.CheckedAt = c.checkedAt.UTC().Format(time.RFC3339)
	}
	return &r
}

// requestProbe schedules a probe on the loop (non-blocking); force skips
// the debounce (a new token must always be verified).
func (c *claudeAuth) requestProbe(force bool) {
	if force {
		c.mu.Lock()
		c.forced = true
		c.mu.Unlock()
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// claudeTiers lists the tiers whose current worker model is a claude id.
func (a *App) claudeTiers() []string {
	out := []string{}
	for _, t := range wire.Tiers {
		if claudeauth.IsModel(a.workerModelForTier(t)) {
			out = append(out, t)
		}
	}
	return out
}

// usesClaude reports whether anything on this foreman runs the claude CLI
// (a worker tier or the brain); a pure-opencode foreman never probes.
func (a *App) usesClaude() bool {
	if len(a.claudeTiers()) > 0 {
		return true
	}
	return a.modelCfg != nil && claudeauth.IsModel(a.modelCfg.Brain())
}

// observeClaudeDirective applies the caps-push response's claude_auth
// block: a new token revision is fetched (or a cleared one dropped) and
// probed; a probe request is honoured.
func (a *App) observeClaudeDirective(ctx context.Context, d *claudeauth.Directive) {
	c := a.claudeAuth
	if d == nil || c == nil {
		return
	}
	c.mu.Lock()
	held := c.rev
	c.mu.Unlock()
	if d.TokenRev != held {
		if d.TokenRev == "" {
			c.mu.Lock()
			c.token, c.rev = "", ""
			c.mu.Unlock()
			log.Printf("foremanapp: claude auth: factory token cleared; using this host's own CLI login")
		} else {
			tok, rev, err := a.fetchClaudeToken(ctx)
			if err != nil {
				log.Printf("foremanapp: claude auth: fetch token rev %s: %v", d.TokenRev, err)
				return
			}
			c.mu.Lock()
			c.token, c.rev = tok, rev
			c.mu.Unlock()
			log.Printf("foremanapp: claude auth: adopted factory token rev %s", rev)
		}
		c.requestProbe(true)
		return
	}
	if d.Probe {
		c.requestProbe(false)
	}
}

// fetchClaudeToken GETs the factory token (paired bearer). Errors carry
// the HTTP status only, never a body.
func (a *App) fetchClaudeToken(ctx context.Context) (token, rev string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimSlash(a.factoryURL)+"/v1/foreman/claude-token", nil)
	if err != nil {
		return "", "", errors.New("build request")
	}
	req.Header.Set("Authorization", "Bearer "+a.paired.Bearer)
	client := a.poster.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", errors.New("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return "", "", fmt.Errorf("factory returned status %d", resp.StatusCode)
	}
	var body struct {
		Value string `json:"value"`
		Rev   string `json:"rev"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponse)).Decode(&body); err != nil || body.Value == "" {
		return "", "", errors.New("malformed token response")
	}
	return body.Value, body.Rev, nil
}

// probeClaudeAuth runs one probe with the current token and records it.
// It reports whether the state changed.
func (a *App) probeClaudeAuth(ctx context.Context) bool {
	c := a.claudeAuth
	c.mu.Lock()
	if c.probing {
		c.mu.Unlock()
		return false
	}
	c.probing = true
	tok, prev := c.token, c.state
	prober := c.prober
	c.mu.Unlock()

	out := prober.Probe(ctx, tok, false)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.probing = false
	c.detail = out.Detail
	c.checkedAt = a.now()
	// An inconclusive probe (network, usage limit) never overturns a
	// known verdict: only ok/failed are authoritative.
	if out.State == claudeauth.StateUnknown && prev != claudeauth.StateUnknown {
		log.Printf("foremanapp: claude auth: probe inconclusive (%s); keeping %s", out.Detail, prev)
		return false
	}
	c.state = out.State
	if out.State != prev {
		log.Printf("foremanapp: claude auth: %s -> %s (%s)", prev, out.State, out.Detail)
	}
	return out.State != prev
}

// claudeAuthLoop probes at start, every claudeProbeEvery, and on demand;
// a changed verdict is pushed to the factory at once (not at the next 60s
// caps tick).
func (a *App) claudeAuthLoop(ctx context.Context) {
	if !a.usesClaude() {
		log.Printf("foremanapp: claude auth: no claude model configured; probe loop idle")
	}
	t := time.NewTicker(claudeProbeEvery)
	defer t.Stop()
	var last time.Time
	run := func(force bool) {
		if !a.usesClaude() {
			return
		}
		a.claudeAuth.mu.Lock()
		force = force || a.claudeAuth.forced
		a.claudeAuth.forced = false
		a.claudeAuth.mu.Unlock()
		if !force && !last.IsZero() && a.now().Sub(last) < claudeProbeMinGap {
			return
		}
		last = a.now()
		if a.probeClaudeAuth(ctx) {
			if err := a.pushCapabilities(ctx); err != nil {
				log.Printf("foremanapp: capabilities push after auth change: %v", err)
			}
		}
	}
	run(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run(true)
		case <-a.claudeAuth.kick:
			run(false)
		}
	}
}
