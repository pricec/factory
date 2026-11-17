package foremanapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

// postEnvelopeStatus POSTs a typed envelope with a bearer and returns the
// HTTP status.
func postEnvelopeStatus(url, token, typ, jobID string, payload any) (int, error) {
	p, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	body, err := json.Marshal(wire.MessageEnvelope{Type: typ, JobID: jobID, From: "worker:" + jobID, Payload: p})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// TestWorkerUsagePostSurvivesReportResult reproduces the 2026-11-17
// usage-401 bug: the agent calls report_result mid-run (job.result lands
// in the foreman inbox and is forwarded), then the claude child exits and
// the harness posts job.usage with the per-job bearer. The foreman used to
// revoke that bearer as soon as it forwarded the result, so the usage post
// got 401 and the row was lost. The usage post must succeed; once the
// harness has exited the token must be dead (no second result).
func TestWorkerUsagePostSurvivesReportResult(t *testing.T) {
	fac := newFactory(t)
	token, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	fe := newForeman(t, fac, token)

	spec := wire.NewJobSpec("report then exit", "factory:main")
	resultURL := fe.ts.URL + "/v1/inboxes/foreman:alpha/messages"
	res, _ := json.Marshal(wire.JobResult{JobID: spec.ID, Status: wire.StatusDone, Summary: "reported"})
	env, _ := json.Marshal(wire.MessageEnvelope{Type: "job.result", JobID: spec.ID, From: "worker:" + spec.ID, Payload: res})

	dir := t.TempDir()
	// Fake claude: report_result (POST + flag file), linger so the foreman
	// consumer forwards the result and revokes (on master), then exit
	// printing the CLI's JSON result with usage.
	claudeDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
cat > %[1]s/env.json <<'EOF'
%[2]s
EOF
curl -s -o /dev/null -X POST -H "Authorization: Bearer $WORKER_TOKEN" -H 'Content-Type: application/json' --data @%[1]s/env.json %[3]s
touch .result_posted
sleep 2
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1","total_cost_usd":0.5,"usage":{"input_tokens":10,"output_tokens":20},"modelUsage":{"claude-sonnet-5-5":{"inputTokens":10}}}'
`, dir, env, resultURL)
	if err := os.WriteFile(filepath.Join(claudeDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	usageStatus := -1
	var bearerSeen string
	var usageErr error
	orig := fe.a.BuildHarnessConfig
	fe.a.BuildHarnessConfig = func(s wire.JobSpec, bearer string) workerharness.Config {
		c := orig(s, bearer)
		mu.Lock()
		bearerSeen = bearer
		mu.Unlock()
		c.Model = "claude-sonnet-5-5"
		c.ClaudeBin = filepath.Join(claudeDir, "claude")
		c.ResultURL = resultURL
		c.MCPURL = fe.ts.URL + "/v1/mcp/worker"
		c.Timeout = 20 * time.Second
		c.PostUsage = func(_ context.Context, url, tok string, ev wire.UsageEvent) error {
			st, err := postEnvelopeStatus(url, tok, wire.TypeJobUsage, ev.JobID, ev)
			mu.Lock()
			usageStatus, usageErr = st, err
			mu.Unlock()
			return err
		}
		return c
	}

	if err := fe.a.SpawnWorker(context.Background(), spec); err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}
	waitFor(t, "usage post attempted", 15*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return usageStatus != -1 || usageErr != nil
	})
	mu.Lock()
	st, uerr := usageStatus, usageErr
	mu.Unlock()
	if uerr != nil || st < 200 || st >= 300 {
		t.Fatalf("usage post after report_result: status=%d err=%v, want 2xx (token revoked too early)", st, uerr)
	}

	// After the harness exits the credential is revoked: a second result
	// post is rejected.
	var second int
	waitFor(t, "token revoked after harness exit", 10*time.Second, func() bool {
		mu.Lock()
		tok := bearerSeen
		mu.Unlock()
		second, _ = postEnvelopeStatus(resultURL, tok, "job.result", spec.ID, wire.JobResult{JobID: spec.ID, Status: wire.StatusDone, Summary: "again"})
		return second == http.StatusUnauthorized
	})
}
