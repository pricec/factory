package usagewin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// DefaultURL is the (undocumented) subscription usage endpoint.
const DefaultURL = "https://api.anthropic.com/api/oauth/usage"

// Poller polls the usage endpoint. The OAuth access token is read fresh and
// read-only from the Claude credentials file on every poll; it is never
// logged, stored, or refreshed (the CLI owns refresh).
type Poller struct {
	Tracker *Tracker
	// Client is injectable (nil -> http.DefaultClient with a timeout).
	Client interface {
		Do(*http.Request) (*http.Response, error)
	}
	URL       string // "" -> DefaultURL
	CredsPath string // "" -> ~/.claude/.credentials.json
	// Base is the GREEN interval (default 120s); Fast the interval at
	// YELLOW or worse (default 60s). Jitter is +-10%.
	Base, Fast time.Duration
	Now        func() time.Time // injectable clock
	Rand       func() float64   // injectable jitter source in [0,1)
	Sleep      func(ctx context.Context, d time.Duration) error
	Logf       func(format string, args ...any)

	failures int
}

// maxBackoff caps the failure backoff.
const maxBackoff = 10 * time.Minute

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Poller) logf(f string, a ...any) {
	if p.Logf != nil {
		p.Logf(f, a...)
		return
	}
	log.Printf(f, a...)
}

func (p *Poller) credsPath() string {
	if p.CredsPath != "" {
		return p.CredsPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", ".credentials.json")
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("credentials file unreadable")
	}
	var c struct {
		O struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(b, &c) != nil || c.O.AccessToken == "" {
		return "", errors.New("no access token in credentials file")
	}
	return c.O.AccessToken, nil
}

type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// parseUsage schema-checks the response: five_hour and seven_day each need a
// numeric utilization (percent) and a parseable resets_at. Everything else
// is ignored.
func parseUsage(body []byte) (map[Window]Reading, error) {
	var raw struct {
		Five  *usageWindow `json:"five_hour"`
		Seven *usageWindow `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("response is not the expected JSON")
	}
	out := map[Window]Reading{}
	for w, uw := range map[Window]*usageWindow{Win5h: raw.Five, Win7d: raw.Seven} {
		if uw == nil || uw.Utilization == nil || uw.ResetsAt == nil {
			return nil, fmt.Errorf("schema: %s missing utilization or resets_at", w)
		}
		t, err := time.Parse(time.RFC3339, *uw.ResetsAt)
		if err != nil {
			return nil, fmt.Errorf("schema: %s resets_at unparseable", w)
		}
		out[w] = Reading{Utilization: *uw.Utilization, ResetsAt: t.UTC()}
	}
	return out, nil
}

// retryAfterError carries a server-requested delay.
type httpError struct {
	status     int
	retryAfter time.Duration
}

func (e *httpError) Error() string { return fmt.Sprintf("usage endpoint returned HTTP %d", e.status) }

// PollOnce does one poll. On success the tracker gets endpoint readings; on
// any failure the last good readings are untouched (they age into stale).
func (p *Poller) PollOnce(ctx context.Context) error {
	tok, err := readToken(p.credsPath())
	if err != nil {
		return err
	}
	url := p.URL
	if url == "" {
		url = DefaultURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errors.New("bad request")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		// Do not wrap err: url.Error text is safe, but keep it generic.
		return errors.New("usage request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		he := &httpError{status: resp.StatusCode}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			he.retryAfter = time.Duration(s) * time.Second
		}
		return he
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return errors.New("usage response unreadable")
	}
	rds, err := parseUsage(body)
	if err != nil {
		return err
	}
	now := p.now()
	for w, rd := range rds {
		rd.ObservedAt = now
		p.Tracker.Set(w, SourceEndpoint, rd)
	}
	return nil
}

// Next computes the delay before the next poll given the current state:
// Base when GREEN, Fast at YELLOW or worse, exponential backoff (capped, or
// the server's Retry-After) after failures, +-10% jitter.
func (p *Poller) Next(st State, pollErr error) time.Duration {
	base, fast := p.Base, p.Fast
	if base <= 0 {
		base = 120 * time.Second
	}
	if fast <= 0 {
		fast = 60 * time.Second
	}
	d := base
	if st.rank() >= Yellow.rank() {
		d = fast
	}
	if pollErr != nil {
		p.failures++
		for i := 0; i < p.failures && d < maxBackoff; i++ {
			d *= 2
		}
		var he *httpError
		if errors.As(pollErr, &he) && he.retryAfter > d {
			d = he.retryAfter
		}
		d = min(d, maxBackoff)
	} else {
		p.failures = 0
	}
	r := rand.Float64
	if p.Rand != nil {
		r = p.Rand
	}
	return time.Duration(float64(d) * (0.9 + 0.2*r()))
}

// Run polls until ctx is done.
func (p *Poller) Run(ctx context.Context) {
	sleep := p.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	for ctx.Err() == nil {
		err := p.PollOnce(ctx)
		if err != nil && ctx.Err() == nil {
			p.logf("usagewin: poll failed (keeping last reading, marked stale after %s): %v", StaleAfter, err)
		}
		st := p.Tracker.Snapshot(ctx).State
		if sleep(ctx, p.Next(st, err)) != nil {
			return
		}
	}
}
