# Idle timeout + live spend cap (task_01M7K5CSGG3JNWE5C8J54XP188)

- **Branch:** `feat/idle-timeout-spend-cap` — rebased onto `origin/master` @ `0d94dae`; see "Review fixes" below
- **Base:** originally `52c8346`, now `0d94dae`. S1 (`feat/token-s1-capture`) and S2 (`feat/token-s2-window`) are already ancestors of master, so no token branch was needed.
- Not merged, not deployed.

## New defaults
| Setting | Old | New |
|---|---|---|
| `timeout_s` (hard ceiling) | 300 | **14400** (4h) |
| `idle_timeout_s` | none | **900** (15m) |
| Ceiling by tier when `timeout_s` is unset | – | flash 3600, default 7200, hardest 14400 (empty/unknown tier: 14400) |
| Loop guard | – | 8 back-to-back tool calls with identical name, input AND result; kills the job; `loop_limit` per-job override |
| Budget kill | – | at `budget.max_cost_usd` (default **$20**; API-equivalent estimate, not real money — we run on a Claude subscription usage plan) |

Explicit per-job `timeout_s` / `idle_timeout_s` always win.

## Fields and config names
- `JobSpec.idle_timeout_s` (new, omitempty); `timeout_s` is now the ceiling.
- `JobError.reason` (new, omitempty): `idle | ceiling | budget | loop`. `idle` and `ceiling` use `kind=timeout`. `budget` and `loop` use `kind=other`. The reason is also in `error.message` and so in the job's `last_error`.
- Tier table: `wire.TierCeilingS` (`internal/wire/spec.go`). It is a code constant, not an env or config-file setting.
- Harness test seams on `workerharness.Config`: `IdleTimeout`, `Timeout`, `LoopLimit` (negative disables), `TermGrace`.
- `docs/spec.md` §8.6 and the defaults table are updated.

## Behavior
- **Heartbeat:** any write to the child's stdout or stderr resets the idle timer (`beatWriter`). The harness polls limits at most every 1s.
- **Budget:** `claudecli.Stream` parses stream-json lines live. It prices each assistant message id once from its token usage. The final `result.total_cost_usd` replaces the estimate. At cost >= cap the harness sends SIGTERM to the process group, waits 5s, then SIGKILLs. The job fails with `reason=budget`. The summary reads "Stopped at spend cap ($x of $y). Partial: …". Partial stdout is kept. Cost is posted as usage if the CLI never printed a result.
- **Factory side:** `ApplyResult` sets `over_budget=1` for `reason=budget`. The consumer dead-letters `budget` and `loop` kills with no retry, so they cannot re-spend. `idle` and `ceiling` keep the timeout retry policy (1 retry).
- **Sweeper:** its deadline is `updated_at + timeout_s + 60s`, and a missing `timeout_s` falls back to 14400. The harness enforces idle, budget and loop itself, and always posts by the ceiling. The sweeper therefore never marks `timed_out` a job the harness still sees as alive. I did **not** add a harness->factory last-activity report.
- **No usage data (opencode runner):** only idle and ceiling apply.

## Tests
`make ci` is green (fmt-check, vet, all packages including `test/e2e`, build).
- `internal/workerharness/limits_test.go`:
  - `TestIdleKill`
  - `TestCeilingKillDespiteHeartbeats`
  - `TestHeartbeatResetsIdleTimer` (stdout and stderr both count)
  - `TestBudgetKillKeepsPartialOutput`
  - `TestBudgetNoUsageDataFallsBackToIdleCeiling`
  - `TestLoopGuard`
- `internal/claudecli/stream_test.go`: cost accumulation, duplicate message ids, split lines, loop counting, authoritative result total.
- `internal/jobs/jobs_test.go`: `TestSweeperUsesCeilingNotIdle`, `TestApplyResultBudgetKillMarksOverBudget`.
- `internal/wire/wire_test.go`: `TestNormalizeTimeoutDefaults`.
- The new harness tests also passed under `-race`.

## Review fixes (job_01M7MCGMYCYT8VMY2HTXJ6N076 → MERGE AFTER FIXES)
Operator decisions: default cap $20; loop guard kills; dollar figures are API-equivalent estimates standing in for subscription usage-window consumption, not invoices (stated in code comments on `DefaultMaxCostUSD`, `Budget.MaxCostUSD`, the price table, and in the spec).

1. **Rebase** onto `0d94dae`. One conflict, `internal/workerharness/harness.go` `recordUsage`: now `usage := claudecli.UsageWithStderr(parsed.Usage, errOut)`, then the branch's live-estimate fallback, then `recordUsage(ctx, cfg, spec, usage)`.
2. **Default `max_cost_usd` 5 → 20** (`wire.DefaultMaxCostUSD`, used by `NewJobSpec`/`Normalize`/create paths; spec.md; tests).
3. **Price table** (`internal/claudecli/stream.go`), source https://platform.claude.com/docs/en/about-claude/pricing (2026-11-17), USD/MTok, cache write = 5m rate, cache read = "cache hits" column:

| Family (match) | in | out | cache write | cache read |
|---|---|---|---|---|
| opus-5-5 (planner default `claude-opus-5-5`) | 4 | 20 | 5 | 0.20 |
| opus 5 / 4.8 / 4.7 / 4.6 / 4.5 and bare `opus` (e.g. worker override `claude-opus-4-6`) | 5 | 25 | 6.25 | 0.50 |
| opus 4 / 4.1 | 15 | 75 | 18.75 | 1.50 |
| sonnet-5-5 | 2 | 10 | 2.50 | 0.10 |
| sonnet-5 | 2 | 10 | 2.50 | 0.20 |
| sonnet 4.x and bare `sonnet` | 3 | 15 | 3.75 | 0.30 |
| haiku-5-5 (>100K-prompt tier, conservative; ≤100K is 0.10/0.50) | 0.50 | 2.50 | 0.625 | 0.05 |
| haiku 4.5 and bare `haiku` | 1 | 5 | 1.25 | 0.10 |

   The old table priced opus at 15/75 (4 of 5 current Opus models are 3–4x cheaper) and used 0.1x cache reads everywhere. Unknown models no longer get the opus rate: they use the tier's model (`NewStream(cfg.Model)`) rate with a once-per-model warning; if that is unknown too, a neutral Sonnet-4 rate (3/15) with a warning. The final result `total_cost_usd` still replaces the estimate. GLM tiers have no claude stream, so they are unaffected.
4. **Loop guard**: a call counts as a repeat only if name, compacted input AND tool result (matched by `tool_use_id`, hashed) equal the previous completed call. A call with no result yet does not count. Per-job override `JobSpec.loop_limit` (omitempty; 0 = 8; negative = disabled); `Config.LoopLimit` remains a seam. Still a kill. Loop kills now also get SIGTERM + grace first (like budget).
5. **Process cleanup**: after the SIGTERM grace, the harness always sends SIGKILL to the process group (`Kill(-pgid)`), even if the leader already exited.

New/updated tests: `claudecli`: `TestStreamCost`, `TestEstimateCostPriceTable`, `TestUnknownModelUsesTierModelRateNotOpus`, `TestLoopCountsOnlyIdenticalCallAndResult`. `workerharness`: `TestLoopGuardKillsIdenticalLoop`, `TestLoopGuardPollingWithChangingResultsNotKilled`, `TestLoopGuardJobSpecOverride`, `TestBudgetKillReapsSIGTERMIgnoringGrandchild` (fails if the unconditional group SIGKILL is removed), `TestBudgetKillKeepsPartialOutput` (cap now set explicitly). `factoryapp`: `TestLoopKillDeadLetters` (reason=loop → dead, no retry). `wire`: `TestDefaultMaxCostIs20`, `TestLoopLimitOmittedWhenZero`.

## Follow-ups (not built)
- Tier ceilings applied via `Normalize` for all create paths (caveat 3).
- Show `error.reason` in the UI and MCP views.
- Sweeper dead-host reap time (caveat 5).

## Merge / deploy order
The harness runs in the worker/foreman binaries and `wire` is shared. Roll the new harness/worker binaries out first or together with the factory: factory-first is safe too (an old harness just never sets `reason`, and ignores `loop_limit`), but harness-first with an old factory loses `over_budget`/no-retry handling for `reason=budget|loop`, so deploy them together, factory first if staggered.

## Caveats (original; items 1 and 2 are now addressed above)
1. **Live cost is an estimate.** stream-json carries tokens but no cost until the final result event. The per-family price table (opus 15/75, sonnet 3/15, haiku 1/5 USD per M tokens; cache read 0.1x, cache write 1.25x; unknown models priced as opus) is from memory and **needs verifying** in `internal/claudecli/stream.go`. If the table is stale, the cap fires early or late, and the result event's total only helps if the job reaches it.
2. **Loop guard false positives.** 8 identical calls in a row also catches legitimate polling, e.g. repeating the same `Bash` status check. It only applies to the claude runner. Tune with `Config.LoopLimit`. There is no per-job field.
3. **Tier ceilings** apply only where `timeout_s` is 0 when `Normalize` runs. `NewJobSpec` and the create paths still set an explicit `DefaultTimeoutS`, so most jobs currently get 14400 rather than the tier value. Making tier ceilings universal means making `NewJobSpec` leave `timeout_s` at 0 and normalizing after the tier is set. I held back because `Validate` rejects 0 on paths that skip `Normalize`.
4. **mcpworker child cap:** a child's `timeout_s` is capped at the parent's. Parents now default to a 4h ceiling, so children can too. `idle_timeout_s` is not inherited or capped.
5. **Sweeper limits:** it is deliberately ceiling-based, so a harness crash (no result posted) is now only noticed after the ceiling (up to 4h), not after 5 min. Orphan recovery (§8.7) still covers foreman restarts. A harness->factory activity report would restore faster detection.
6. **Scope note:** the brain notes say factory development belongs to foreman:server. I did this because the operator task named it directly.
