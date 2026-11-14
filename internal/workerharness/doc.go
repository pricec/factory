// Package workerharness implements the worker-side opencode harness
// (spec §8): it runs one opencode agent for a single job in a fresh
// per-job directory and reports the result back to the foreman. It backs
// the worker command. opencode must be on PATH - the foreman's
// responsibility (§8.4).
package workerharness
