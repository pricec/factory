package foremanapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/pricec/factory/internal/registry"
)

// pairedFileName is the stored pairing record under DataDir (spec §7.1).
const pairedFileName = "paired.json"

// pairingRecord is the persisted pairing result (spec §7.1 response).
type pairingRecord struct {
	Bearer    string `json:"bearer"`
	InboxID   string `json:"inbox_id"`
	ParentURL string `json:"parent_url"`
}

// loadOrPair returns the stored pairing, or burns a.cfg.PairingToken against
// the factory and stores the result. No record + no token -> "not paired".
func loadOrPair(ctx context.Context, a *App) (pairingRecord, error) {
	path := filepath.Join(a.dataDir, pairedFileName)
	if data, err := os.ReadFile(path); err == nil {
		var rec pairingRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return rec, fmt.Errorf("foremanapp: %s: %w", pairedFileName, err)
		}
		if rec.Bearer == "" {
			return rec, errors.New("foremanapp: paired.json: bearer missing")
		}
		return rec, nil
	} else if !os.IsNotExist(err) {
		return pairingRecord{}, fmt.Errorf("foremanapp: read %s: %w", pairedFileName, err)
	}

	if a.cfg.PairingToken == "" {
		return pairingRecord{}, fmt.Errorf("foremanapp: not paired: no %s and no pairing token; obtain one (curl -X POST %s/v1/pairing-tokens -H %q) and pass -pairing-token",
			pairedFileName, strings.TrimSuffix(a.factoryURL, "/"), `Authorization: Bearer <planner token>`)
	}
	rec, err := pairWithFactory(ctx, a)
	if err != nil {
		return pairingRecord{}, err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return rec, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return rec, fmt.Errorf("foremanapp: write %s: %w", pairedFileName, err)
	}
	return rec, nil
}

// pairWithFactory POSTs the pairing request to the factory (spec §7.1):
// {pairing_token, self:{id, capabilities}} where capabilities are the
// startup-detected tags (spec §6.7): PATH tool detection merged with
// os/arch.
func pairWithFactory(ctx context.Context, a *App) (pairingRecord, error) {
	body := map[string]any{
		"pairing_token": a.cfg.PairingToken,
		"self": map[string]any{
			"id":           a.id,
			"capabilities": detectCapabilities(a.cfg.Harness),
			"host":         a.hostFacts(),
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return pairingRecord{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(a.factoryURL, "/")+"/v1/pair", bytes.NewReader(data))
	if err != nil {
		return pairingRecord{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pairingRecord{}, fmt.Errorf("foremanapp: pair: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return pairingRecord{}, fmt.Errorf("foremanapp: pair: status %d", resp.StatusCode)
	}
	var rec pairingRecord
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rec); err != nil {
		return rec, fmt.Errorf("foremanapp: pair response: %w", err)
	}
	if rec.Bearer == "" {
		return rec, errors.New("foremanapp: pair response: bearer missing")
	}
	if rec.InboxID == "" {
		rec.InboxID = "foreman:" + a.id
	}
	return rec, nil
}

// detectCapabilities returns the foreman's startup capability tags (spec
// §6.7): PATH-detected tools merged with os/arch.
//
// harness "claude" makes the tools tag reflect the harness in use: the
// opencode tag is dropped (the foreman never runs it) and claude is
// advertised when its binary is on PATH.
func detectCapabilities(harness string) map[string]string {
	dirs := filepath.SplitList(os.Getenv("PATH"))
	caps := registry.DetectTools(dirs)
	if harness == HarnessClaude {
		caps["tools"] = claudeHarnessTools(caps["tools"], registry.HasTool(dirs, "claude"))
	}
	caps["os"] = runtime.GOOS
	caps["arch"] = runtime.GOARCH
	return caps
}

// defaultAdvertiseURL computes http://<hostname>:<port of Addr> (the
// human-facing form; harness traffic uses localBaseURL instead).
func defaultAdvertiseURL(addr string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	return "http://" + host + ":" + portOf(addr, "8090")
}

// localBaseURL computes the 127.0.0.1 form of the foreman's own base URL.
// Workers are spawned in-process on the same machine and the harness talks
// plain HTTP to the foreman's own mux (ResultURL/MCPURL), so the loopback
// form - NOT the hostname advertise form - is what the harness configs get.
// The port comes from cfg.Addr; a listener that picked a free port itself
// (":0") must override BuildHarnessConfig.
func localBaseURL(addr string) string {
	return "http://127.0.0.1:" + portOf(addr, "8090")
}

func portOf(addr, fallback string) string {
	port := fallback
	if i := strings.LastIndex(addr, ":"); i >= 0 && addr[i+1:] != "" {
		port = addr[i+1:]
	}
	return port
}

// postJSON issues one POST with a JSON body and bearer auth; 2xx is a
// success. Used by the remote ack.
func postJSON(ctx context.Context, target, bearer string, body any) error {
	_, _, err := postJSONResponse(ctx, target, bearer, body)
	return err
}

// maxResponseBody caps a response body read by postJSONResponse.
const maxResponseBody = 1 << 20

// postJSONResponse is postJSON returning the 2xx status and body (up to
// maxResponseBody): the capabilities push reads the update advert from it.
func postJSONResponse(ctx context.Context, target, bearer string, body any) (int, []byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil, fmt.Errorf("POST %s: status %d", target, resp.StatusCode)
	}
	if readErr != nil {
		return resp.StatusCode, nil, fmt.Errorf("POST %s: read response: %w", target, readErr)
	}
	return resp.StatusCode, respBody, nil
}

// sleepCtx sleeps for d and reports whether ctx is still live.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// HarnessClaude is the Config.Harness value pinning the Claude Code CLI.
const HarnessClaude = "claude"

// claudeHarnessTools rewrites a detected tools tag for a claude-pinned
// foreman: opencode dropped, claude added when installed; sorted.
func claudeHarnessTools(tools string, haveClaude bool) string {
	set := map[string]bool{}
	for _, t := range strings.Split(tools, ",") {
		if t != "" && t != "opencode" {
			set[t] = true
		}
	}
	if haveClaude {
		set["claude"] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
