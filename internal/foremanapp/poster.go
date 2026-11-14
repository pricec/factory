package foremanapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// Poster posts foreman-originated envelopes to the factory's main inbox
// (spec §7.3): the forward action of the routing table (spec §3.3, §3.4)
// and the job.event reports (spec §6.2). It implements routing.Poster.
type Poster struct {
	// FactoryURL is the factory base URL.
	FactoryURL string
	// Bearer is the paired foreman credential (post-only on factory:main).
	Bearer string
	// Backoff is the delay between attempts; zero means 1s.
	Backoff time.Duration
	// HTTPClient issues requests; nil means http.DefaultClient.
	HTTPClient *http.Client
}

// Post attempts (at-least-once): 3 attempts with 1s backoff between them.
// A 409 response is an idempotent duplicate (spec §4.2) and succeeds.
// Other non-2xx responses are definitive and not retried.
const (
	postAttempts = 3
	postBackoff  = time.Second
)

// PostToParent posts env to {FactoryURL}/v1/inboxes/factory:main/messages
// with the paired bearer. The body carries the client-side envelope fields
// (spec §3.1); the factory server assigns id and ts. The payload crosses
// byte-identical (spec §3.4).
func (p *Poster) PostToParent(ctx context.Context, env wire.MessageEnvelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	target := trimSlash(p.FactoryURL) + "/v1/inboxes/" + FactoryMainInbox + "/messages"
	backoff := p.Backoff
	if backoff <= 0 {
		backoff = postBackoff
	}
	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	var lastErr error
	for attempt := 1; attempt <= postAttempts; attempt++ {
		err := p.postOnce(ctx, client, target, body)
		if err == nil {
			return nil
		}
		lastErr = err
		var se *postStatusError
		if errors.As(err, &se) {
			return err // definitive rejection: not retried
		}
		if attempt == postAttempts {
			break
		}
		if !sleepCtx(ctx, backoff) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("post to factory after %d attempts: %w", postAttempts, lastErr)
}

func (p *Poster) postOnce(ctx context.Context, client *http.Client, target string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.Bearer)
	resp, err := client.Do(req)
	if err != nil {
		return err // connection-style: retried
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusConflict:
		return nil // idempotent duplicate (spec §4.2)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	}
	return &postStatusError{code: resp.StatusCode}
}

// postStatusError marks a definitive non-2xx, non-409 rejection.
type postStatusError struct{ code int }

func (e *postStatusError) Error() string {
	return fmt.Sprintf("post to factory: status %d", e.code)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
