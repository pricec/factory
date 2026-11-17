package foremanapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/pricec/factory/internal/mcpworker"
)

// requestMerge is the worker request_merge relay (spec §9.2, §6.11): it
// files the calling job's merge request on the factory - POST
// {FactoryURL}/v1/merge-requests with job_id set, which makes the request
// job-owned - and returns the queued request. A refusal (validation, an
// active request that is already merging) surfaces the factory's §10
// error message so the worker can act on it.
func (a *App) requestMerge(ctx context.Context, jobID string, in mcpworker.MergeRequestArgs) (mcpworker.MergeRequestResult, error) {
	var out mcpworker.MergeRequestResult
	data, err := json.Marshal(map[string]any{
		"branch": in.Branch, "head": in.Head, "note": in.Note, "repo": in.Repo, "after": in.After, "job_id": jobID,
	})
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, trimSlash(a.factoryURL)+"/v1/merge-requests", bytes.NewReader(data))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.paired.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+a.paired.Bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, fmt.Errorf("factory merge queue unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			return out, fmt.Errorf("%s: %s", e.Error.Code, e.Error.Message)
		}
		return out, fmt.Errorf("factory answered status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("decode factory response: %w", err)
	}
	return out, nil
}
