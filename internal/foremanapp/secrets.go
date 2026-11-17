package foremanapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pricec/factory/internal/wire"
)

// Handout fetch (spec §13.2): bounded retry, definitive on 4xx.
const (
	secretFetchAttempts = 3
	secretFetchBackoff  = time.Second
	maxSecretsResponse  = 8 << 20
)

// fetchJobSecrets fetches the values for spec.Secrets from the factory's
// job handout endpoint with the paired bearer and returns env var -> value.
// Errors carry job id, secret NAMES and HTTP status only - never a value
// and never a response body. A job promised credentials never runs without
// them: the caller fails the job on any error.
func (a *App) fetchJobSecrets(ctx context.Context, spec wire.JobSpec) (map[string]string, error) {
	backoff := a.secretBackoff
	if backoff <= 0 {
		backoff = secretFetchBackoff
	}
	target := trimSlash(a.factoryURL) + "/v1/jobs/" + url.PathEscape(spec.ID) + "/secrets"
	client := a.poster.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	var lastErr error
	for attempt := 1; attempt <= secretFetchAttempts; attempt++ {
		env, retry, err := a.fetchSecretsOnce(ctx, client, target, spec)
		if err == nil {
			return env, nil
		}
		lastErr = err
		if !retry || attempt == secretFetchAttempts || !sleepCtx(ctx, backoff) {
			break
		}
	}
	return nil, fmt.Errorf("could not fetch secrets [%s] from the factory: %w", strings.Join(spec.Secrets, ", "), lastErr)
}

func (a *App) fetchSecretsOnce(ctx context.Context, client *http.Client, target string, spec wire.JobSpec) (env map[string]string, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, errors.New("build request")
	}
	req.Header.Set("Authorization", "Bearer "+a.paired.Bearer)
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL
		}
		return nil, true, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode >= 500, fmt.Errorf("factory returned status %d", resp.StatusCode)
	}
	var body struct {
		Secrets []struct {
			Name   string `json:"name"`
			EnvVar string `json:"env_var"`
			Value  string `json:"value"`
		} `json:"secrets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSecretsResponse)).Decode(&body); err != nil {
		return nil, false, errors.New("malformed handout response")
	}
	env = make(map[string]string, len(body.Secrets))
	got := map[string]bool{}
	for _, s := range body.Secrets {
		if s.EnvVar == "" || s.Value == "" {
			return nil, false, fmt.Errorf("handout for %s is incomplete", s.Name)
		}
		env[s.EnvVar] = s.Value
		got[s.Name] = true
	}
	var missing []string
	for _, n := range spec.Secrets {
		if !got[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, false, fmt.Errorf("handout omitted %s", strings.Join(missing, ", "))
	}
	return env, false, nil
}
