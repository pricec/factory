package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pricec/factory/internal/summarize"
)

// evalRecord is one line of results.jsonl written by summarize-eval.
type evalRecord struct {
	N             int     `json:"n"`
	JobID         string  `json:"job_id"`
	State         string  `json:"state"`
	UpdatedAt     string  `json:"updated_at"`
	InputBytes    int     `json:"input_bytes"`
	OutputBytes   int     `json:"output_bytes"`
	Ratio         float64 `json:"ratio"`
	PromptTokens  int     `json:"prompt_tokens"`
	OutputTokens  int     `json:"output_tokens"`
	DurationMS    int64   `json:"duration_ms"`
	LoadMS        int64   `json:"load_ms"`
	Cold          bool    `json:"cold"`
	DoneReason    string  `json:"done_reason,omitempty"`
	ThinkingBytes int     `json:"thinking_bytes,omitempty"`
	Truncated     bool    `json:"truncated,omitempty"`
	Accepted      bool    `json:"accepted"`
	RejectReason  string  `json:"reject_reason,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// runSummarizeEval implements `factory summarize-eval` (spec §6.13): it
// opens jobs.db READ-ONLY, picks the most recent terminal jobs whose last
// result is over -min-bytes (or the -jobs list), summarises each with
// -model, and writes original/summary pairs, the sanity-check verdict and
// timings to -out. It changes nothing in the factory.
func runSummarizeEval(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("summarize-eval", flag.ContinueOnError)
	dataDir := fs.String("data", "./factory-data", "factory data directory (jobs.db is opened read-only)")
	outDir := fs.String("out", "", "output directory (required; created)")
	url := fs.String("url", os.Getenv("FACTORY_SUMMARIZER_URL"), "Ollama base URL")
	model := fs.String("model", summarize.DefaultModel, "Ollama model")
	think := fs.String("think", "", "think parameter: false|true|low|medium|high (default auto)")
	n := fs.Int("n", 20, "number of jobs")
	minBytes := fs.Int("min-bytes", summarize.DefaultMinBytes, "only results at least this many bytes")
	jobList := fs.String("jobs", "", "comma-separated job ids (overrides the recent-jobs pick)")
	timeout := fs.Duration("timeout", summarize.DefaultTimeout, "per-request timeout")
	unload := fs.Bool("unload", true, "unload the model (keep_alive 0) when done")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outDir == "" || *url == "" {
		return errors.New("summarize-eval: -out and -url are required")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	db, err := openRO(filepath.Join(*dataDir, "jobs.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	type pick struct{ id, state, updated, text string }
	var picks []pick
	want := map[string]bool{}
	for _, id := range strings.Split(*jobList, ",") {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	rows, err := db.Query(`SELECT id, state, results, updated_at FROM jobs
		WHERE state IN ('done','failed','dead','timed_out','cancelled','superseded')
		ORDER BY updated_at DESC`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p pick
		var results string
		if err := rows.Scan(&p.id, &p.state, &results, &p.updated); err != nil {
			rows.Close()
			return err
		}
		if len(want) > 0 && !want[p.id] {
			continue
		}
		var entries []struct {
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal([]byte(results), &entries) != nil || len(entries) == 0 {
			continue
		}
		p.text = summarize.ResultText(entries[len(entries)-1].Result)
		if len(want) == 0 && len(p.text) < *minBytes {
			continue
		}
		picks = append(picks, p)
		if len(want) == 0 && len(picks) >= *n {
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "summarize-eval: %d jobs, model %s\n", len(picks), *model)

	client := summarize.New(summarize.Config{
		URL: strings.TrimRight(*url, "/"), Model: *model, Think: *think, Timeout: *timeout, MinBytes: 1,
	})
	ctx := context.Background()
	jl, err := os.Create(filepath.Join(*outDir, "results.jsonl"))
	if err != nil {
		return err
	}
	defer jl.Close()
	enc := json.NewEncoder(jl)
	var durs []time.Duration
	accepted := 0
	for i, p := range picks {
		base := filepath.Join(*outDir, fmt.Sprintf("%02d_%s", i+1, p.id))
		if err := os.WriteFile(base+".orig.txt", []byte(p.text), 0o644); err != nil {
			return err
		}
		rec := evalRecord{N: i + 1, JobID: p.id, State: p.state, UpdatedAt: p.updated, InputBytes: len(p.text)}
		raw, err := client.Call(ctx, p.text)
		if err != nil {
			rec.Error = err.Error()
		} else {
			rec.OutputBytes = len(raw.Content)
			rec.Ratio = float64(len(raw.Content)) / float64(len(p.text))
			rec.PromptTokens, rec.OutputTokens = raw.PromptEvalCount, raw.EvalCount
			rec.DurationMS, rec.LoadMS = raw.Duration.Milliseconds(), raw.LoadDuration.Milliseconds()
			rec.Cold = raw.LoadDuration > time.Second
			rec.DoneReason, rec.ThinkingBytes, rec.Truncated = raw.DoneReason, raw.Thinking, raw.Truncated
			if err := os.WriteFile(base+".summary.txt", []byte(raw.Content), 0o644); err != nil {
				return err
			}
			if _, cerr := summarize.Check(p.text, raw, 0); cerr != nil {
				rec.RejectReason = cerr.Error()
			} else {
				rec.Accepted = true
				accepted++
			}
			durs = append(durs, raw.Duration)
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%2d %s %6dB -> %6dB %6.1fs cold=%v accepted=%v %s%s\n", rec.N, p.id, rec.InputBytes,
			rec.OutputBytes, float64(rec.DurationMS)/1000, rec.Cold, rec.Accepted, rec.RejectReason, rec.Error)
	}
	if *unload {
		if err := client.Unload(ctx); err != nil {
			fmt.Fprintf(stdout, "unload: %v\n", err)
		}
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	fmt.Fprintf(stdout, "accepted %d/%d; latency p50 %s p95 %s\n", accepted, len(picks),
		quantile(durs, 0.5).Round(time.Millisecond), quantile(durs, 0.95).Round(time.Millisecond))
	return nil
}

// quantile returns the nearest-rank q-quantile of sorted durations.
func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q*float64(len(sorted))+0.999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
