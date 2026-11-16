// Command worker is a one-shot agent harness: it runs an agent for a
// single job - opencode, or the Claude Code CLI for claude-* models
// (spec §8.4) - and reports the result back to its foreman (spec §8).
// Spawned as `worker -job <path/to/job.json>` with WORKER_RESULT_URL,
// WORKER_MCP_URL, WORKER_TOKEN, and WORKER_DIR in the environment (§8.1).
package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/pricec/factory/internal/workerharness"
)

func main() {
	log.SetPrefix("worker: ")
	job := flag.String("job", "", "path to the dispatched job JSON (spec §8.1)")
	flag.Parse()
	if *job == "" {
		log.Fatal("-job is required")
	}
	cfg := workerharness.Config{
		JobPath:   *job,
		ResultURL: os.Getenv("WORKER_RESULT_URL"),
		MCPURL:    os.Getenv("WORKER_MCP_URL"),
		Token:     os.Getenv("WORKER_TOKEN"),
		WorkDir:   os.Getenv("WORKER_DIR"),
	}
	if err := workerharness.Run(context.Background(), cfg); err != nil {
		log.Fatal(err)
	}
}
