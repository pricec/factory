// Package workerharness implements the worker-side agent harness
// (spec §8): it runs one agent for a single job in a fresh per-job
// directory and reports the result back to the foreman. It backs the
// worker command. The routed runner (§8.4) is opencode for GLM models
// and the Claude Code CLI for claude-* models; the runner binary must be
// on PATH - the foreman's responsibility (§8.4, install scripts §7.1).
package workerharness
