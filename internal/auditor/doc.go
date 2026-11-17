// Package auditor is the factory's daily self-audit (spec §6.12): it
// snapshots how the factory ran over the last 24h (with a 7-day trend),
// turns what it sees into fingerprinted findings, files each new finding
// as a "[proposal]" task for the operator, and applies a small set of
// "obvious" fixes on its own - but only inside guardrails that are
// enforced here, in code, not in a prompt:
//
//   - a changed-line cap and a forbidden path/area denylist checked against
//     the real git diff of a fix branch before it may enter the merge queue
//     (guardrails.go: CheckDiff, CheckPlan);
//   - a hard per-run cap on auto-fixes (RunCap, HardMaxAutoFixes);
//   - a per-run cost_usd budget meter (Meter).
//
// The guardrail constants and the denylist live in this package, and the
// package itself (plus its factory wiring) is on the denylist, so an
// auto-fix can never edit the auditor's own rules, guardrails or schedule.
// Runtime knobs (schedule, budget) come from the factory's environment
// (config.go), which no worker job can write.
//
// The package is storage-agnostic: snapshot.go reads the factory's sqlite
// files through *sql.DB (so the dry run can open them read-only), dedup.go
// persists fingerprints in jobs.db, and internal/factoryapp/auditor.go
// owns the side effects (tasks, jobs, merge requests).
package auditor
