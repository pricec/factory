package workerharness

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

// Post delivers a fallback JobResult to resultURL with the given bearer
// token (§8.5).
type PostFunc func(ctx context.Context, resultURL, token string, res wire.JobResult) error

const (
	// maxPostAttempts bounds fallback result delivery attempts
	// (at-least-once, §8.5).
	maxPostAttempts = 3
	// defaultPostRetryBackoff is the delay between post attempts (§8.5).
	defaultPostRetryBackoff = time.Second
	// resultEnvelopeType is the wire type of a worker's report (§3.2).
	resultEnvelopeType = "job.result"
)

// senderFrom returns the fallback envelope's sender identity (§3.1).
func senderFrom(jobID string) string {
	return "worker:" + jobID
}

// statusError marks a non-2xx, non-409 response: a definitive rejection
// that is not retried.
type statusError struct {
	code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("post result: status %d", e.code)
}

// httpPost posts res as a job.result envelope with Bearer auth (§3.1, §8.5).
// A 409 response is an idempotent duplicate (§4.2) and succeeds.
func httpPost(ctx context.Context, resultURL, token string, res wire.JobResult) error {
	payload, err := json.Marshal(res)
	if err != nil {
		return err
	}
	env := wire.MessageEnvelope{
		Type:    resultEnvelopeType,
		JobID:   res.JobID,
		From:    senderFrom(res.JobID),
		Payload: payload,
	}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resultURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode == http.StatusConflict:
		return nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return &statusError{code: resp.StatusCode}
	}
	return nil
}

// postResult delivers res with at-least-once retries (§8.5): up to
// maxPostAttempts attempts with cfg.PostRetryBackoff between them, on
// connection-style errors. Definitive rejections (statusError) return
// immediately so the caller can log them.
func postResult(ctx context.Context, cfg Config, res wire.JobResult) error {
	var err error
	for attempt := 1; attempt <= maxPostAttempts; attempt++ {
		if err = cfg.Post(ctx, cfg.ResultURL, cfg.Token, res); err == nil {
			return nil
		}
		var se *statusError
		if errors.As(err, &se) {
			return err
		}
		if attempt == maxPostAttempts {
			break
		}
		if werr := sleepCtx(ctx, cfg.PostRetryBackoff); werr != nil {
			return werr
		}
	}
	return fmt.Errorf("post result after %d attempts: %w", maxPostAttempts, err)
}

// sleepCtx sleeps for d unless ctx finishes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
