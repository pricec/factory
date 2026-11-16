# Idle timeout + live spend cap (task_01M7K5CSGG3JNWE5C8J54XP188)

- **Branch:** `feat/idle-timeout-spend-cap` — code commit `3a0eca7` (this report is committed on top)
- **Base:** `origin/master` @ `52c8346`. S1 (`feat/token-s1-capture`) and S2 (`feat/token-s2-window`) are already ancestors of master, so no token branch was needed.
- Not merged, not deployed.

## New defaults
| Setting | Old | New |
|---|---|---|
| `timeout_s` (hard ceiling) | 300 | **14400** (4h) |
| `idle_timeout_s` | none | **900** (15m) |
| Ceiling by tier when `timeout_s` is unset | – | flash 3600, default 7200, hardest 14400 (empty/unknown tier: 14400) |
| Loop guard | – | 8 identical back-to-back tool calls |
| Budget kill | – | at `budget.max_cost_usd` (default $5) |

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

## Caveats
1. **Live cost is an estimate.** stream-json carries tokens but no cost until the final result event. The per-family price table (opus 15/75, sonnet 3/15, haiku 1/5 USD per M tokens; cache read 0.1x, cache write 1.25x; unknown models priced as opus) is from memory and **needs verifying** in `internal/claudecli/stream.go`. If the table is stale, the cap fires early or late, and the result event's total only helps if the job reaches it.
2. **Loop guard false positives.** 8 identical calls in a row also catches legitimate polling, e.g. repeating the same `Bash` status check. It only applies to the claude runner. Tune with `Config.LoopLimit`. There is no per-job field.
3. **Tier ceilings** apply only where `timeout_s` is 0 when `Normalize` runs. `NewJobSpec` and the create paths still set an explicit `DefaultTimeoutS`, so most jobs currently get 14400 rather than the tier value. Making tier ceilings universal means making `NewJobSpec` leave `timeout_s` at 0 and normalizing after the tier is set. I held back because `Validate` rejects 0 on paths that skip `Normalize`.
4. **mcpworker child cap:** a child's `timeout_s` is capped at the parent's. Parents now default to a 4h ceiling, so children can too. `idle_timeout_s` is not inherited or capped.
5. **Sweeper limits:** it is deliberately ceiling-based, so a harness crash (no result posted) is now only noticed after the ceiling (up to 4h), not after 5 min. Orphan recovery (§8.7) still covers foreman restarts. A harness->factory activity report would restore faster detection.
6. **Scope note:** the brain notes say factory development belongs to foreman:server. I did this because the operator task named it directly.
