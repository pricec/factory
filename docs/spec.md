# factory: wire protocol & schemas (normative)

Status: **normative**. This document is the single source of truth for every wire
contract in the system (AGENTS.md; #16). It consolidates the contracts pinned in
#1-#19. Every statement cites the issue that pinned it as (#N). Future tickets
reference this file instead of re-pinning contracts (#16).

Rules of transcription (#16):

- Statements transcribe pinned bodies verbatim; producers/consumers in the
  taxonomy table are derived from the pinned routing rules (not invention).
- Gaps are marked *Not pinned* and left open; they are never silently filled.
- Where two pinned bodies disagree on the same field or behavior, both are
  transcribed, the point is marked **TODO**, and neither variant is normative
  until the conflict is closed ("comment on both and stop", #16).

Open conflicts: none. TODO-1 and TODO-2 (found during consolidation) are
resolved by the orchestrator; resolutions are transcribed in §3.2, §3.4 and
§7.3, and recorded in the updated bodies of #7 and #11.

## 1. Versioning rule (/v1)

- Every endpoint in this document is prefixed `/v1` (#1, #2, #8, #9, #10, #11,
  #12, #13, #17, #18, #19).
- Additive changes (new endpoints, new optional fields) stay in `/v1`.
- An incompatible change to any contract in this document must not silently
  mutate `/v1`: it requires bumping the version prefix (e.g. `/v2`) and
  updating this document in the same change. Code never diverges from this
  spec (AGENTS.md).

## 2. Terminology & roles

Decided in #14 and used consistently in protocol, code, docs, and UI:

| Term | Binary | Role |
|---|---|---|
| factory | `factory` | Controller: orchestration + web UI. Hosts the job state machine, foremen registry, schedules, and the planner MCP surface (AGENTS.md; #15; #19). |
| foreman | `foreman` | Per-machine agent host. Pairs with the factory, runs the worker harness plus the worker and brain MCP surfaces. Its inbox id is `foreman:{id}` (#1; #11). |
| worker | `worker` | One-shot harness process that runs an agent for a single job - opencode, or the Claude Code CLI for claude-* models (§8.4) - and reports the result (AGENTS.md; #5). |

Example inbox id: `foreman:alpha` (#1). Targets: Linux only, amd64 + arm64
(AGENTS.md; #15). Go module `github.com/pricec/factory` (#15). Dependency
policy: stdlib-first; allowed additions are `modernc.org/sqlite`,
`github.com/robfig/cron/v3`, `github.com/oklog/ulid`, and
`github.com/modelcontextprotocol/go-sdk` (fallback decision recorded in §9.1)
(#15).

## 3. Message envelope & type taxonomy

### 3.1 Envelope (#1, #3)

```json
{
  "id": "msg_...",
  "type": "job.result",
  "job_id": "",
  "from": "foreman:alpha",
  "payload": {},
  "ts": "..."
}
```

| Field | Rule | Pinned in |
|---|---|---|
| `id` | Assigned by the server on POST: ULID with `msg_` prefix | #1, #3 |
| `type` | String; taxonomy in §3.2 (open set) | #1, #4 |
| `job_id` | Present for job-scoped types | #1 |
| `from` | Sender identity, e.g. `foreman:alpha` | #1 |
| `payload` | JSON value; type-specific | #1 |
| `ts` | Assigned by the server on POST: RFC3339 | #1 |
| `thread_id` | Optional (omitted when empty): the task thread the message belongs to (§9.5.1). Persisted and returned by every read path | inbox redesign S1 |

Go types for `MessageEnvelope`, `JobSpec`, and `JobResult` live in
`internal/wire` - the single source of truth, with validation (#3). ULIDs via
`oklog/ulid`; id prefixes `msg_` / `job_` (#3).

The POST request body supplies the client-side fields (`type`, `job_id`,
`from`, `payload`); the exact request shape is not pinned beyond that - the
response is the full envelope with server-assigned `id` and `ts` (#1).

### 3.2 Type taxonomy: producers & consumers

| Type | Produced by | Consumed by / next hop | Pinned in |
|---|---|---|---|
| `job.request` | Factory dispatch: `create_job` (#19), schedule fire (#8), retry re-dispatch (#10) - derived from the routing and dispatch rules | Foreman routing -> `spawn_worker` (#4); the envelope carries the JobSpec (derived: #4 enqueues "the JobSpec", #13 rejects `job.request` on prompt size) | #4, #8, #10, #13, #19 |
| `job.result` | Worker: via `report_result` (#17) or the harness fallback (#5) | Foreman routing -> forward to parent, byte-identical payload (§3.4); then the factory jobs events path: terminal status (#9), retry policy (#10), usage accounting (#13) | #1, #4, #5, #7, #9, #10, #17 |
| `job.event` | Foreman: `{"job_id": ..., "event": "accepted" or "started"}` (#9); `accepted` also carries the full JobSpec - `{"event": "accepted", "spec": {...}}` - so foreman-local spawns register in the job registry (2026-11-15 spawn-visibility amendment, §6.2) | Factory jobs events path (#9); foreman-side routing rule: drop (#4) | #4, #9 |
| `job.question` | Worker: via `ask_parent` (#17), posted to the foreman's own inbox `foreman:{id}` (§3.2) | Foreman brain via routing, 60s SLA, default answer on timeout (#4) | #4, #11, #17 |
| `job.answer` | Foreman brain: via `answer_worker` (#18) | Worker: `ask_parent` long-polls the job inbox (#17); `answer_worker` posts to the job inbox (#18) | #17, #18 |
| `brain.wake` | Not pinned in any body | Routing -> brain (#4) | #4 |
| `foreman.report` | Foreman brain: `post_to_factory("foreman.report", payload)` (§9.3); the operator report | Factory `factory:main` consumer: relayed verbatim into `planner:main` with a planner wake (§7.3, §9.5) | 2026-11-16 report-routing amendment |
| `brain.failed` | Foreman: the brain reporter, when a non-question brain session fails (§3.3) | Factory `factory:main` consumer: relayed into `planner:main` with a planner wake (§9.5) | 2026-11-15 amendment |
| any other type | `post_to_factory(type, ...)` (#18), `message_foreman(..., type, ...)` (#19) | Routing -> brain ("anything unmatched -> brain") (#4) on the foreman; on `factory:main`, no handler: the factory posts `notify.unhandled` to `planner:main` and acks (§9.5) | #4, #18, #19 |

The type set is open: `post_to_factory` and `message_foreman` accept arbitrary
type strings, and unmatched types route to the brain (#4). An envelope that
reaches `factory:main` with no handler is never acked silently: the operator
gets a P2 alert (`notify.unhandled` before the 2026-11-16 inbox redesign; §9.5.3).

Inbox destinations for the worker conversation flow (resolution of TODO-2;
transcribed from #17, #18, with the corrected #11 scope model): a worker's
question (`job.question`) is posted to the foreman's own inbox `foreman:{id}`
(#17); the brain's answer (`job.answer`) is posted to the per-job inbox
`job:{job_id}` (#18), which the worker long-polls via `ask_parent` (#17).
Worker-token scopes follow in §7.3.

### 3.3 Foreman routing rules (#4)

`internal/routing`: rule table in Go code, no config file in v1 (#4). Handler
actions: `forward | drop | brain | spawn_worker` (#4).

| Envelope type | Action | Notes (#4) |
|---|---|---|
| `job.request` | `spawn_worker` | The factory only dispatches to capable foremen; #12 owns matching. Enqueues the JobSpec to the worker harness queue. |
| `job.result` | `forward` | Forward to parent, byte-identical payload (§3.4); ZERO LLM calls - this is the hot path. |
| `job.event` | `drop` | State flows through the jobs events path. |
| `job.question` | `brain` | 60s SLA; default answer on timeout: "proceed with your best judgment". |
| `brain.wake` and anything unmatched | `brain` | |

`brain.Interface { Handle(ctx, envelope) ([]Action, error) }` with a stub
implementation recording calls; the real LLM brain lands with #18 (#4).
`spawn_worker` enqueues the JobSpec to the worker harness queue; `forward`
posts to the parent inbox (#4).

Brain-delegated envelopes (`job.question`, `brain.wake`, anything unmatched)
never block the foreman's inbox consume loop (2026-11-15 post-incident
amendment). The consumer hands them to a durable local brain queue - the
sub-inbox `foreman:{id}:brain` on the foreman's own inbox store (§7.3) - and
acks the source copy once the enqueue lands; a dedicated drain loop runs one
brain session at a time and acks each queue copy only after its session
returns. The hot paths (`job.request` -> `spawn_worker`, `job.result` ->
forward) therefore never wait on an LLM session, however long the §9.3
session bound runs. Semantics stay at-least-once (§4): a crash before the
drain loop acks leaves the queue copy unacked and redelivered on restart (no
loss); a crash between enqueue and source ack can re-deliver from the source
and enqueue twice - brain sessions are idempotent-by-judgment, and within a
single process the one-at-a-time, ack-before-next-Get discipline means an
in-flight envelope is never re-read, so no double-handling occurs in normal
operation. A failed or timed-out session is logged by the router and the
queue advances: a poison envelope is consumed, not redelivered forever, so
it can never block later brain work. The queue copy carries type, job id,
sender, and payload verbatim; only its message id and ts are re-assigned by
the store (like any inbox post), so brain prompts show the queue copy's id.

Summaries flow up, never transcripts - three enforcement points (#7):

1. Worker reports: summary required, 1-240 chars (schema in #3; tool in #17)
   (#7).
2. Storage, not transport: forwarded `job.result` payloads are byte-identical
   (#4); full results are stored on the factory job record (#9) and never
   enter LLM context (updated #7).
3. Context builder: any payload entering an LLM context is hard-capped at 2KB,
   except explicit `read_result` pulls (#7) - see §6.6.

### 3.4 Resolved conflict: forwarded `job.result` payload (TODO-1)

Resolution (orchestrator; recorded in the updated #7 body): #4 wins. `forward`
posts the envelope to the parent inbox with byte-identical payload - full
result payloads cross the wire and are stored on the factory job record (#9:
`results` array), where `read_result` (#19) serves them. Rationale: network
bytes are free - token cost is governed by what enters LLM context, which the
2KB cap (§6.6) enforces; storing full results at the factory means
`read_result` needs no cross-machine fetch and no new protocol. The contested
#7 variant (strip to `{job_id, status, summary, usage}`) is withdrawn.

## 4. Inbox service: REST, long-poll, SSE

`internal/inbox` is shared by factory + foreman (#1). Pure-Go SQLite via
`modernc.org/sqlite` (no CGo), WAL mode, `busy_timeout=5s` (#1).

### 4.1 REST endpoints (#1)

| Method & path | Request | Response |
|---|---|---|
| `POST /v1/inboxes/{id}/messages` | client-side envelope fields (§3.1) | `201` + full envelope (server assigns `id`, `ts`) |
| `GET /v1/inboxes/{id}/messages?limit=50&wait_seconds=0` | `limit` default 50, max 500; `wait_seconds` 0-60 | `200` + `[envelope]`, unacked only, FIFO |
| `POST /v1/inboxes/{id}/ack` | `{"ids": [...]}` | `204`; double-ack is also `204` (not an error) |
| `GET /v1/inboxes/{id}/messages/{mid}` | - | `200` + envelope (replay/UI) |
| `GET /v1/inboxes` | - | `200` + `[{inbox_id, total, unacked, last_ts}]`, one aggregate row per inbox that has ever received a message, ordered by inbox id (UI listing) |
| `GET /v1/inboxes/{id}/recent?limit=50` | `limit` default 50, max 500 | `200` + newest `limit` envelopes in chronological order, acked **included** (UI history; the long-poll GET above is unacked-only) |

The two UI endpoints are unauthenticated like the §6.9 control plane (trusted-LAN
posture, §7.5) and exist because no factory credential carries inbox-read
scopes: the UI reads the store server-side instead. `GET /` serves the
single-page operator UI (§9.5 conversational chat over `planner:main`, foremen
list with `last_seen`, inbox listing, per-item detail views) over these
endpoints plus §6.9.

`wait_seconds` 0-60 long-polls until non-empty (#1). Error responses use the
error model in §10.

### 4.2 Delivery semantics (#1)

- FIFO per inbox by `seq`; explicit client ack; at-least-once.
- No leases in v1: single consumer per inbox.
- Duplicate `job.result` for the same `(inbox_id, job_id)` -> `409` with the
  original envelope id (idempotent results).
- Restart/replay: kill + restart redelivers unacked messages.

### 4.3 Storage (#1)

`messages(id PK, inbox_id, type, job_id, sender, payload, ts, seq
AUTOINCREMENT, acked INT, acked_at)`; partial index `(inbox_id, seq)` WHERE
`acked=0`; partial unique `(inbox_id, job_id)` WHERE `type='job.result'`.
`thread_id TEXT NULL` + index `(thread_id, seq)` (inbox redesign S1, see
§9.5 Storage): added in place by `ALTER TABLE` when absent. The migration
runs on every open, is additive only and idempotent; no row is ever
deleted or rewritten by it. The envelope's optional `thread_id` (§3.1,
`omitempty`) is persisted on POST and returned by every read path.

### 4.4 SSE stream (#2)

- Transport: SSE (no websockets). `GET /v1/inboxes/{id}/stream`, same auth as
  the inbox REST API.
- Events: `message` (data = full envelope) fired on POST; heartbeat comment
  `:hb` every 15s.
- No `Last-Event-ID` in v1: events are wake-ups, not the delivery path. On
  (re)connect the client resyncs via the GET snapshot.

### 4.5 Client loop: subscribe-before-check (#2)

Normative loop for `internal/notify`:

1. Open stream.
2. GET snapshot.
3. Process + ack.
4. Wait for `message` event -> goto 2.

### 4.6 Reconnect & backoff (#2)

Backoff: 1s, 2s, 4s, ... max 60s, with +-25% jitter; always resync after
reconnect. (This is the SSE reconnect curve; the job-retry backoff in §6.4 is
a different, fixed curve.)

### 4.7 Liveness observer (#2)

`Observer` interface: `OnActivity(identity, ts)`, `OnConnect(identity)`,
`OnDisconnect(identity)` (#2). #9 subscribes; liveness storage itself lands in
#9 (#2).

## 5. Job contracts (internal/wire)

### 5.1 JobSpec (#3)

```json
{
  "id": "job_...",
  "task": "prompt for the worker",
  "payload": {},
  "result_schema": {},
  "requires": ["gpu"],
  "foreman": "foreman:server",
  "tier": "flash",
  "timeout_s": 300,
  "priority": 5,
  "budget": {
    "max_prompt_bytes": 32768,
    "max_output_tokens": 8192,
    "max_cost_usd": 5.0
  },
  "created_by": "factory",
  "created_at": "..."
}
```

Defaults shown; `priority` 1-9, 9 highest (#3). `tier` selects the
worker's model tier (#28, #29): `flash` (or absent - the cost-biased
default), `default`, or `hardest`; see §6.5.

`foreman` (2026-11-16 foreman-pin amendment, §6.7) is the explicit
routing pin: when set, the job runs on that foreman instead of the
Matcher's capability pick. Absent on ordinary matcher-dispatched jobs;
when present it is the full `foreman:{id}` form at rest (the create
paths normalize both forms, §7.3). `requires` stay informational on a
pinned job.

`parent_job_id` (2026-11-15 sub-spawning amendment, §9.2) appears only on
child jobs spawned by a worker's `spawn_child` tool and is absent otherwise
(factory-dispatched and brain-spawned jobs never carry it): it names the
parent job, whose per-job inbox receives a copy of the child's terminal
`job.result`. Validation requires a `job_`-prefixed id different from the
job's own id. `spawn_child` is the only writer of the field.

### 5.2 JobResult (#3)

```json
{
  "job_id": "job_...",
  "status": "done",
  "summary": "1-240 chars, REQUIRED",
  "result": {},
  "error": {
    "kind": "validation|timeout|crash|llm|other",
    "message": "..."
  },
  "usage": {
    "input_tokens": 0,
    "output_tokens": 0,
    "cost_usd": 0.0
  }
}
```

- `status`: `done` | `failed`.
- `error.kind`: `validation` | `timeout` | `crash` | `llm` | `other`.

### 5.3 `result_schema` subset (#3)

Only these keys are accepted: `type`, `properties`, `required`, `items`.
Anything outside the subset rejects at validation.

### 5.4 Validation rules (#3)

- `summary`: required always, 1-240 chars.
- `status=done`: `result` required when `result_schema` is present, and must
  validate against it.
- `status=failed`: `error` required; `result` ignored.
- `usage` optional; all fields >= 0.

## 6. Job lifecycle, liveness & budgets

### 6.1 Job states & transitions (#9)

`internal/jobs`; SQLite `jobs` table: `{id PK, spec JSON, state, foreman_id,
attempts INT 0, next_attempt_at NULL, last_error, usage JSON, results JSON
array, over_budget INT 0, created_at, updated_at}` (#9).

States: `pending | dispatched | running | done | failed | timed_out | dead |
cancelled | superseded`.
Legal transitions (#9, with retry/dead transitions from #10 and the
cancel/supersede transitions from the 2026-11-15 amendment):

- `pending -> dispatched`
- `dispatched -> running`
- `running -> {done, failed, timed_out}` (`failed` also via a startup
  orphan result after a foreman restart, 2026-11-16 amendment, §8.7)
- `failed -> dispatched` (retry, #10)
- `failed -> dead` (#10)
- `pending -> {cancelled, superseded}` (cancel API, 2026-11-15 amendment)
- `dispatched -> {cancelled, superseded}` (cancel API, 2026-11-15 amendment)
- `running -> {cancelled, superseded}` (cancel API, 2026-11-15 amendment)
- `failed -> {cancelled, superseded}` (cancel API, 2026-11-15 amendment;
  also clears the armed retry so the picker cannot re-dispatch)

Anything else: log + ignore (never panic) (#9).

Cancel/supersede (2026-11-15 amendment, post-incident: a duplicate pending
job had to be killed by a direct `jobs.db` write because no API path
existed - direct production DB writes are never the workaround). `cancelled`
and `superseded` are TERMINAL states: `Terminal` admits no further
transitions, and a late `job.result` for a cancelled job is the usual
terminal-idempotent no-op (§6.4). Every non-terminal job (pending,
dispatched, running, failed) may cancel; every terminal job rejects the
cancel with `409 already_terminal` (§6.9, §10). v1 records the factory-side
state change only: there is no foreman-side kill, so in-flight worker work
is not force-stopped, and a cancelled running job's late result is ignored
rather than re-transitioning the job. The cancel reason rides the job's
`last_error` (`"<to>: <reason>"`), surfacing in the §9.5 `notify.job`
payload and `GET /v1/jobs/{id}`.

### 6.2 Job events & sweeper (#9)

- Foreman posts envelope type `job.event` `{job_id, event: accepted|started}`;
  terminal status comes from `job.result`.
- Spawn visibility (2026-11-15 spawn-visibility amendment): foreman-local
  spawns (the §9.3 `spawn_worker` seam) mint their JobSpec on the foreman -
  the factory never created a registry row for them, so their events and
  forwarded `job.result` were ignored as unknown and the jobs were invisible
  at the factory (they existed only in the foreman's local `jobs/` store).
  Fix: the foreman's `accepted` job.event carries the full JobSpec, and the
  factory registers insert-if-absently - an accepted event for an unknown
  job id creates the row in state `dispatched`, attributed to the sending
  foreman (the envelope `from`); the paired `started` event then drives
  `dispatched -> running` and the forwarded `job.result` applies
  terminal-idempotently, the ordinary lifecycle from there. The events post
  before the harness starts, so the row always exists before any worker
  report - the §8.5 loud spawn-failure path surfaces at the factory like any
  worker report. Registration never modifies an existing row: re-delivered
  accepted events (at-least-once, §4) and jobs the factory created itself
  are no-ops, so registration can neither duplicate nor clobber.
- Sweeper (10s tick, fake-clockable): `running` past `timeout_s + 60s` ->
  `timed_out`.
- Offline foreman's jobs are left as-is unless past timeout + grace ->
  `timed_out`.
- Foreman restart orphans (2026-11-16 orphan-recovery amendment, §8.7): a
  restarted foreman fails its own running jobs that have no live worker
  process at startup (`crash`, retry armed). The sweeper above is the
  backstop for what that pass cannot reach; its `timed_out` is terminal and
  never retries.

### 6.3 Foremen registry & liveness (#9, #12, #2)

- `foremen` table: `{id PK, capabilities JSON, last_seen, online INT}` (#9).
- `last_seen` is updated by the auth middleware on any authed request + SSE
  connect (#9); the capabilities refresh also bumps `last_seen` (#12).
- Sweeper marks a foreman offline after 90s (#9; covers the 60s
  capabilities push cadence plus jitter and one missed push).
- SSE heartbeat comment `:hb` every 15s keeps the stream alive (#2); #9
  subscribes to the `Observer` interface from #2 (#2, #9).

### 6.4 Retries & dead-letter (#10)

- Terminal-idempotency: applying a `job.result` to an already-terminal job is
  a no-op (log). Envelope-level dedupe (409) lives in the inbox (#1).
- Retry policy (normative), by `error.kind`: `validation` -> no retry;
  `timeout` -> 1 retry; `crash | llm | other` -> 2 retries.
- Backoff fixed curve: 30s, 2m, 8m (attempt n waits `backoff[n-1]`;
  `next_attempt_at` is set when entering `failed`).
- Picker goroutine (10s tick, fake-clockable): `failed` with
  `next_attempt_at <= now` -> re-dispatch (`failed -> dispatched`,
  `attempts++`). Per-attempt results are appended to the results array.
- Exhausted -> `dead` (dead-letter).
- Retries re-dispatch the SAME job id - never a new one.
- Dead-letter API: `GET /v1/deadletter` (job + `last_error` + results);
  `POST /v1/deadletter/{id}/requeue` (resets attempts, state `pending`).

### 6.5 Budget enforcement & spend breaker (#13)

True token spend is not enforceable mid-run; v1 uses proxies + post-hoc
accounting (#13). Enforcement proxies, in order (#13):

1. Prompt bytes: foreman rejects `job.request` where the assembled prompt
    (§8.3: task + payload JSON + boilerplate) exceeds
    `budget.max_prompt_bytes`; immediate failed result, kind `validation`.
2. `max_output_tokens`: set as model `max_tokens` in the generated opencode
   config (#5 wires this).
3. Wall clock via `timeout_s`.
4. Post-hoc: usage from `report_result` recorded on the job; if
   `usage.cost_usd > max_cost_usd`, set the `over_budget` flag.

Circuit breaker (#13): daily spend = sum of `usage.cost_usd` over jobs
created today; above the config cap (default $20/day) the factory refuses new
`create_job` with error `spend_cap_exceeded`; reset by config bump or env
`FACTORY_SPEND_OVERRIDE=1`.

Model tiering (#13, #28, #29, #30, #31, #32): the factory standardizes on GLM via
OpenRouter (one provider, one credential) and optimizes for cost -
`internal/models` pins the per-role defaults, every role is
env-overridable, and swapping a role's model stays a config change only:

| Role | Pinned default | Env override |
| --- | --- | --- |
| factory planner | `claude-opus-5-5` (Claude Code CLI) | `FACTORY_PLANNER_MODEL` |
| foreman brain | `openrouter/z-ai/glm-5.3` (opencode; a `claude-*` pin runs the Claude Code CLI, §9.3) | `FACTORY_FOREMAN_BRAIN_MODEL` |
| worker, `tier: "flash"` (and no-tier jobs) | `openrouter/z-ai/glm-5.3-flash` | `FACTORY_WORKER_FLASH_MODEL` |
| worker, `tier: "default"` | `openrouter/z-ai/glm-5.3-flash#max` | `FACTORY_WORKER_MODEL` |
| worker, `tier: "hardest"` | `openrouter/z-ai/glm-5.3` | `FACTORY_WORKER_HARDEST_MODEL` |

The coordinator - factory planner and foreman brain - runs full-strength
models, coordination is the factory's judgment layer, and its sessions
are output-light, so the output price barely comes into play. The
factory planner runs Claude Opus 5.5 via the Claude Code CLI
(2026-11-16 planner-claude amendment: Claude Code's default model for
long-running agentic work, $4/$20 per M tokens in/out, exact snapshot id
- the floating `opus` alias is accepted on overrides but never pinned;
the Anthropic CLI's own auth is a second credential path alongside the
OpenRouter key); the foreman brain stays on full GLM 5.3 (#32) by default
(2026-11-16 foreman-brain-claude amendment: an env pin to a claude-* id
routes the brain sessions to the Claude Code CLI, §9.3, and a foreman is
pinned to one only once its binary carries the claude runner). Each
coordinator's runner follows its model (§9.3, §9.4): a claude-* model runs
the Claude Code CLI, everything else opencode - so an env override back to a
GLM string also flips the runner back, no restart. The worker
ladder is one GLM 5.3 generation across two price points (#30, #31):
Flash (~$0.04/$0.08 per M tokens; the `default` tier is the same model
at max reasoning effort - same token prices, the only extra spend is the
reasoning tokens actually consumed) and full GLM 5.3 (~$0.08/$3.6) for
the hardest tasks. GLM 5.2 (~$6.8/M output) and GLM 5.3 FlashX
(~$0.37/M input) both priced themselves out of the middle: the
factory's measured job profile is ~99:1 input:output, where FlashX
costs more than the stronger model.
Reasoning-effort variants (`#max`) ride the `opencode run -m` flag, never
the generated config's `model` field - V2 drops `#variant` suffixes
there (#31). Dispatching agents (the factory
planner's `create_job`, the foreman brain's `spawn_worker`) choose among
the three worker tiers and BIAS TOWARDS SAVING MONEY (#29): pick the
cheapest tier that can do the job - flash for routine work, escalate to
flash#max or full GLM 5.3 only when the task needs it. A job that carries no
tier runs flash (the cost-biased default). The tier rides JobSpec (§5.1)
through `create_job`, `POST /v1/jobs`, schedules, and the job create UI.
Model strings reach the child via the generated config or its runner
flags (§8.4; §9.4 for the planner's claude runner, where the model rides
`--model`).

Claude runner override (2026-11-16 amendment): a tier may also be pointed
at a claude model through the SAME env override (`FACTORY_WORKER_*_MODEL`
= a bare `claude-*` id, the bare `claude` alias, or
`anthropic/claude-*`); such a tier's jobs run via the Claude Code CLI
(routing + invocation, §8.4) instead of opencode. The pinned GLM defaults
above are unchanged - claude runs only by explicit operator override -
and the GLM token-cost defaults are unaffected.
Reporting (v1 amendment, #13): `GET /v1/spend?day=` returns daily totals
`{total_usd, jobs}` - jobs do not yet carry a role tag, so per-role
totals `{planner, foreman_brain, worker}` are deferred until they do.

### 6.6 LLM context cap (#7)

Any payload entering an LLM context (foreman brain prompt, factory planner
prompt) is hard-capped at 2KB, except explicit `read_result` pulls (#7). The
factory planner's list view carries summaries only (#7, #19).

### 6.7 Dispatch matching & capabilities (#12, #8)

- Capabilities: flat string tags on the foreman (`os`, `arch`, `gpu`, `tools`
  as comma-separated, ...). Sent at pairing (#11) and at foreman startup
  (config + runtime detection of opencode/docker/git on PATH); stored in the
  foremen table (#9).
- `Match(requires []string, foremen)`: online foremen whose tags are a
  superset of `requires` (AND semantics; no negation in v1). Deterministic
  pick: sort by `(running_jobs asc, id asc)`, take first.
- Requires entry forms (2026-11-15 amendment): an entry is a capability
  TAG NAME - bare (`os`, `arch`, `tools`, `gpu`) or `name:value`
  (`os:linux`, `arch:amd64`, `tools:git`), where the tag's value or its
  comma-separated value list must contain `value`. Capability VALUES
  (`linux`, `amd64`) are not tag names and never match. `:` separates
  name from value; tag names must not contain it.
- Creation-time requires validation (2026-11-15 amendment): the create
  paths (MCP `create_job`, `POST /v1/jobs`) validate every requires
  entry's tag name against the union of registered foremen tag keys
  (offline foremen count - only `online` filters dispatch). Unknown
  entries are rejected - `unknown_requires`, 400 / MCP tool error - so a
  job no registered foreman can ever match cannot park pending silently
  (2026-11-15 incident: requires `["linux", "amd64"]` sat pending ~7h).
  With no foremen registered the union is empty: creation succeeds but
  the response carries a loud `warnings` array (the pending picker
  dispatches when a matching foreman arrives).
- Refresh response + foreman update advert (2026-11-16 auto-update
  amendment): `POST /v1/foreman/capabilities` (the foreman's 60s caps
  push) answers `204` as before, or `200 application/json`
  `{"foreman_update":{"build":"<BUILD_ID>","platforms":{"linux-amd64":
  {"sha256":"<hex>","path":"/download/foreman/foreman-linux-amd64"},
  "linux-arm64":{...}}}}` when the `-downloads` dir holds a valid
  `manifest.json` (§8.4). The factory hashes each manifest-listed
  `foreman-linux-*` file itself (cache keyed by path+size+mtime, so
  re-hashing happens only when the file changes) and advertises a platform
  only when that hash equals the manifest's sha256; a missing/unreadable/
  malformed manifest, an empty build, an invalid sha or a mismatch/missing
  file omits that platform (the problem is logged loudly once per change,
  not per push). Nothing advertisable -> `204`. Old foremen ignore the
  body. The foreman reports its own build as the `build` capability tag,
  shown in the UI foremen list.
- v1 stub matcher: first online foreman; #12 swaps the real one in and
  deletes the stub (#8, #12).
- No matching foreman: the job stays `pending`, log once (visible via
  `/v1/jobs`) (#8).
- Explicit foreman pin (2026-11-16 amendment): a JobSpec's `foreman`
  field (§5.1) routes the job explicitly - the pin WINS over the
  Matcher, and a pinned job is never capability-matched. The create
  paths (MCP `create_job`, `POST /v1/jobs`) normalize the pin (bare id
  or `foreman:{id}`, §7.3) and resolve it against the registry: a pin
  naming no registered foreman is a hard error (`unknown_foreman`, the
  §6.9 message_foreman contract - it never parks a job pending behind a
  typo); a registered, online foreman dispatches immediately; a
  registered, offline foreman leaves the job `pending` with a loud
  `warnings` entry, and the pending picker dispatches it when that
  foreman returns (never to a different one - no capability fallback).
  A pinned job skips creation-time requires validation: requires stay
  informational on its spec, and the tag-name rules exist to prevent
  silent matcher parking, which a pin cannot suffer. Retries re-dispatch
  to the job's previous foreman (§6.4), which for a pinned job is always
  the pin. Schedules carry the pin in the template, so every fired run
  lands on the same foreman (§6.8). Rationale: dispatching agents were
  pinned out of the routing decision - the Matcher's tags could not
  express "this task belongs on THAT machine", and mis-routed requests
  had no override.
- Pending picker goroutine (10s tick, fake-clockable) (#27): every `pending`
  job is re-matched each pass, oldest first; a qualifying online foreman
  dispatches it (`pending -> dispatched` via the §6.1 transition), envelope
  delivery rides the dispatch notifier (#20). No match keeps it `pending`
  until a foreman qualifies - jobs are never orphaned by a foreman that
  arrives (or pairs) after creation.
- Long-pending alert (2026-11-15 amendment): a `pending` job with no
  matching online foreman for more than 30 minutes (§11) is not left
  invisible - the 2026-11-15 incident left two jobs pending ~7h before
  anyone noticed. A periodic check in the factory server (1-minute tick,
  §11) re-runs the Matcher over the pending set; every job past the
  threshold that still matches no online foreman raises one P1 alert
  (§9.5.3; formerly a `notify.pending` chat row) naming the job id, its requires,
  and why nothing matched (required tags no foreman advertises, or the
  tags living only on offline foremen). A pending job that DOES match an
  online foreman is skipped: the pending picker dispatches it on its next
  pass, so a job merely queued behind a deliberate hold never alerts.
  A pinned pending job (2026-11-16 foreman-pin amendment) is judged by
  its pin instead of the Matcher: it alerts only while its pinned
  foreman is offline or unregistered, the alert names the pin (its
  requires ride along as informational), and a pinned job whose foreman
  is online never alerts - the pending picker dispatches it.
  Dedupe is per job per pending episode (process-local, at-least-once like
  the dispatch notifier #20): one alert until the job leaves `pending`; a
  deadletter requeue (§6.4) starts a fresh episode and re-alerts.
- Refresh: `POST /v1/foreman/capabilities` (foreman token) upserts tags and
  bumps `last_seen`.

### 6.8 Schedules (#8)

- Factory-owned scheduling, single writer; no distributed locks in v1 (#8).
- `schedules` table: `{id PK, cron_spec (5-field, robfig/cron/v3),
  job_template JSON (JobSpec sans id/created_at), enabled, last_run_at}`;
  `schedule_runs`: unique `(schedule_id, run_at)` - makes ticks idempotent.
- Ticker goroutine (fake-clockable); on fire: instantiate a JobSpec (new ULID
  id, `created_at = run_at`) -> `jobs.Create` + dispatch via the Matcher.
  A template's `foreman` pin (§6.7, 2026-11-16 amendment) rides every
  fired run: pinned runs dispatch when the pinned foreman is online and
  pend otherwise - the Matcher is bypassed, so the schedule stays on the
  pinned machine.

### 6.9 Control-plane HTTP API (#8, #9, #10, #12, #13)

| Method & path | Notes |
|---|---|
| `GET /v1/jobs?state=&foreman=` | filter by state and foreman (#9) |
| `POST /v1/jobs` | UI twin of the MCP `create_job` tool (§9.4): body `{task, requires[], result_schema, tier, timeout_s, priority, schedule_cron, foreman?, task_id?}`; runs the same spend breaker + capability match (no match -> `pending`); `201 + {job_id, state, foreman}`. `requires` entries are tag names, validated at creation against the union of registered foremen tag keys (§6.7, 2026-11-15 amendment): unknown tag names -> `400 unknown_requires`; a loud `warnings` array rides the 201 when nothing could be validated (no foremen registered). `foreman` (2026-11-16 foreman-pin amendment, §6.7) pins the job to that foreman (bare or `foreman:{id}` form): the pin wins over the Matcher, requires validation is skipped (requires stay informational), an unknown foreman is `404 unknown_foreman` (the `POST /v1/foremen/{id}/messages` contract), and an offline pin leaves the job `pending` with a `warnings` entry until it returns |
| `POST /v1/foremen/{id}/messages` | UI twin of the MCP `message_foreman` tool (§9.4): body `{type, payload, job_id?}` posted from the planner identity; `201` + envelope. `{id}` accepts the bare foreman id or the full `foreman:{id}` form - both normalize to the same inbox (§7.3), never a double-prefixed one. `payload` is required (any JSON value; `{}` for an empty body); a null/missing payload is `400`. An id matching no registered foreman is `404 unknown_foreman` - posting never opens an inbox no foreman consumes (2026-11-14 amendment) |
| `GET /v1/jobs/{id}` | (#9) the job record; plus `task_id` when the job is directly linked to a task (§9.5.2; 2026-11-16 deploy-18 follow-up - omitted when unlinked, a read never writes lineage links) |
| `POST /v1/jobs/{id}/cancel` | Cancel/supersede a job (2026-11-15 amendment, §6.1): optional body `{reason?, terminal_state?}` with `terminal_state` = `cancelled` (default) \| `superseded`; an empty body is a plain cancel. Every non-terminal state moves to that terminal state; every terminal job is rejected `409 already_terminal`; unknown job `404`; malformed body `400`. `200` + the refreshed job record. UI twin of the MCP `cancel_job` tool (§9.4) |
| `GET /v1/foremen` | returns `id, online, capabilities` (#9) |
| `GET /v1/deadletter` | job + `last_error` + results (#10) |
| `POST /v1/deadletter/{id}/requeue` | resets attempts, state `pending` (#10) |
| `POST /v1/schedules` | (#8) |
| `GET /v1/schedules` | (#8) |
| `DELETE /v1/schedules` | (#8) |
| `POST /v1/schedules/{id}/enable` | (#8) |
| `POST /v1/schedules/{id}/disable` | (#8) |
| `POST /v1/foreman/capabilities` | foreman token; upserts tags, bumps `last_seen` (#12) |
| `GET /v1/spend?day=` | daily totals `{total_usd, jobs}`; per-role deferred until jobs carry role tags (#13) |
| `GET /v1/inboxes` | per-inbox aggregate listing `{inbox_id, total, unacked, last_ts}` (UI; §4.1) |
| `GET /v1/inboxes/{id}/recent` | recent envelopes for one inbox, acked included (UI; §4.1) |
| `POST /v1/chat` | `{text, task_id?}` -> chat.message envelope in the operator inbox, filed under a task (new without `task_id`; 404 for an unknown one); wakes the LLM planner (§9.5, §9.5.2) |
| `GET /v1/tasks`, `GET /v1/tasks/{id}`, `POST /v1/tasks`, `PATCH /v1/tasks/{id}` | task list, task + thread, the "new task" alias of chat, and operator rename `{title}` (§9.5.2) |
| `POST /v1/tasks/{id}/close` | operator closes a task from the UI; clears `needs_operator` and wakes the planner with `task.closed_by_operator` (§9.5.2) |
| `GET /v1/alerts`, `POST /v1/alerts/{id}/ack`, `POST /v1/alerts/{id}/reply` | open alerts, ack, and reply-creates-a-task (§9.5.3) |
| `GET /v1/inboxes/planner:main/stream` | permissive SSE stream of the operator inbox for the chat UI (§9.5) |
| `GET /` | operator web UI (single page; §4.1) |
| `GET /manifest.webmanifest`, `GET /apple-touch-icon.png`, `GET /icon-512.png` | home-screen (PWA) manifest and icons for the UI (§9.5.5) |
| `POST /v1/pairing-tokens` | planner-credential auth (v1: the planner token doubles as the operator credential); issues a one-time pairing token, `200 {pairing_token, expires_at}` (#23) |

Request/response shapes and status codes beyond those shown are not pinned.

## 7. Pairing & scoped auth

### 7.1 Pairing flow (#11)

- The parent prints a one-time token: 32B random hex, 15-min TTL, single use.
- `POST /v1/pairing-tokens` (factory; planner-credential auth: the planner
  token doubles as the operator credential in v1 - same trust class as
  create_job/message_foreman) issues a one-time pairing token; `200
  {pairing_token, expires_at}` (#23).
- The child POSTs `POST /v1/pair`:

```json
{
  "pairing_token": "...",
  "self": { "id": "...", "capabilities": {} }
}
```

- Response `200`:

```json
{ "bearer": "...", "inbox_id": "...", "parent_url": "..." }
```

- `inbox_id = foreman:{id}` when pairing with the factory.
- Worker tokens are minted by a library call inside the foreman - not an HTTP
  endpoint.

Onboarding endpoints (#24, #25) - all outside `/v1`:

- `GET /install.sh`: planner-credential auth (same as
  `POST /v1/pairing-tokens`; 401 unknown/revoked, 403 non-planner). The
  factory renders the onboarding script per-request with a fresh embedded
  one-time pairing token (15-min TTL) and its own advertise URL - no manual
  token copy-paste. The script (as root, on Linux with systemd) downloads
  the foreman binary, installs `/etc/systemd/system/factory-foreman.service`
  (`Environment=FACTORY_PAIRING_TOKEN`, inert once `paired.json` exists),
  and enables + restarts the service; it is idempotent. Install layout
  (auto-update v1 piece C, 2026-11-16 amendment): the binary lands at
  `/var/lib/factory-foreman/bin/foreman`, owned by the service user and
  written via `bin/foreman.new` + `rename(2)`, and the unit's `ExecStart`
  points there so the foreman can swap itself (`bin/foreman.prev` is the
  rollback copy; `bin/update-pending.json` the update marker). The unit also
  sets `StartLimitIntervalSec=600`, `StartLimitBurst=10` (with
  `Restart=always`, `RestartSec=5`) and
  `ExecStartPre=+/opt/factory/foreman-update-guard.sh <data-dir>`, a
  root-owned POSIX-sh crash-loop guard written by the installer: with no
  marker it deletes `bin/update-boots` and does nothing; with a marker it
  counts unconfirmed starts in `bin/update-boots` and on the third restores
  `foreman.prev` over `foreman` (owner/mode preserved), sets
  `"rolled_back":true` in the marker and resets the counter. It always exits
  0 so it can never block a start. Migration: re-running the installer on a
  host with the old `/opt/factory/foreman` layout moves it to the new layout
  in place - state under `/var/lib/factory-foreman` (including `paired.json`,
  so no re-pairing) is never touched, a stale marker/boot counter is dropped,
  and the old `/opt/factory/foreman` is removed after the restart. This
  one-time per-host re-run is required before a host can auto-update
  (un-migrated hosts keep working, just without auto-update); see
  `docs/operator-runbook.md`. It also installs
  BOTH worker CLIs - opencode and the Claude Code CLI (2026-11-16
  amendment, §8.4) - so spawned workers can run either runner: each
  missing CLI is installed via its official installer as the service user
  (its self-updater keeps working) and symlinked into `/usr/local/bin`,
  because systemd runs the unit with the default service PATH and the
  service user's `~/.local/bin` / `~/.opencode/bin` are NOT on it (the
  2026-11-15 user-unit PATH gotcha). 2026-11-16 amendment: the unit also
  pins `Environment=PATH=` to `<service-home>/.opencode/bin:<service-home>/.local/bin`
  followed by the default service dirs, and the install-time verification uses
  that same value, so a CLI resolves in every unit-spawned process whether or
  not a symlink exists. `GET /install-server.sh` pins its `factory.service`
  the same way (its service home is `/var/lib/factory`). A service-visible install is kept
  (idempotent); every step verifies with `command -v` against the service
  PATH - the same lookup the harness's exec performs - and a failed or
  unverifiable install aborts the onboarding loudly (before the unit file
  is written; re-run to complete). Trust note: the
  pairing token rides the rendered script, so the v1 operator-credential
  note extends here - anyone holding the planner token can onboard machines.
- `GET /install-server.sh` (#25): planner-credential auth identical to
  `GET /install.sh` (401 unknown/revoked, 403 non-planner); same trust note
  as above - the v1 operator-credential note extends here: anyone holding the
  planner token can bootstrap new factories. The factory renders the
  bootstrap script per-request with its own advertise URL as the download
  source. The script (as root, on Linux with systemd) downloads the factory
  binary, installs `/etc/systemd/system/factory.service`
  (`Environment=FACTORY_PLANNER_TOKEN`, `chmod 600` - the unit carries the
  planner token; `EnvironmentFile=/etc/factory/factory.env`, the `FACTORY_*`
  override point), pre-seeds the local-arch foreman binary into
  `/var/lib/factory/downloads/` (non-fatal on failure), and enables +
  restarts the service; it is idempotent, and a reinstall preserves the
  existing planner token from the prior unit file. It also installs BOTH
  worker CLIs exactly like `/install.sh` (2026-11-16 amendment, §8.4):
  the factory machine itself runs the planner brain - routed on its
  model per §9.4 (2026-11-16 planner-claude amendment: the pinned
  default runs the Claude Code CLI) - and a foreman onboarded here needs
  opencode plus the Claude Code CLI for its spawned workers.
- `GET /download/{kind}/{platform}` (#24, #25): serves the {kind} =
  `factory` | `foreman` binary from the factory's `-downloads` dir for
  `{platform}` = `{kind}-linux-amd64` | `{kind}-linux-arm64` (i.e. the full
  servable name `factory-linux-amd64`, `foreman-linux-arm64`, ... - a bare
  `linux-$GOARCH` platform is rejected with 400; 400 otherwise is also the
  traversal gate; 404 when the file is missing). The onboarding scripts
  curl exactly these names
  (`/download/{kind}/{kind}-linux-$GOARCH`). Unauthenticated per the
  trusted-LAN posture (§7.5).
- Release manifest (2026-11-16 auto-update amendment): `make release` /
  `make release-docker` stamp every binary with a build id
  (`<UTC commit time YYYYMMDDTHHMMSSZ>-<12-char git sha>`, via `-ldflags -X
  .../internal/buildinfo.ID`; release-docker computes it on the host since
  the container has no `.git`) and write `dist/manifest.json` with
  `internal/tools/mkmanifest` (stdlib only):
  `{"build":"<id>","files":{"foreman-linux-amd64":{"sha256":"<hex>"},
  "foreman-linux-arm64":{...},"factory-linux-amd64":{...},
  "factory-linux-arm64":{...}}}`. Deploy copies the manifest into the
  `-downloads` dir after the binaries (manifest last); the factory
  advertises per §6.7 only what verifies against it.

Bootstrap rule (#25): the first factory in a deployment is installed
manually (`make release` + provision); thereafter factories serve
factories - `GET /install-server.sh` from any running factory bootstraps the
next one.

These endpoints are not part of the §6.9 control-plane surface.

### 7.2 Credentials & storage (#11)

- Bearer: 32B random; stored SHA-256 only - no plaintext bearer at rest.
- Constant-time hash lookup.
- Revocation: delete the row -> `401`.

### 7.3 Scopes & enforcement (#11)

Scope struct: `{inbox, may_read, may_post, job_id?}`.

- Foreman tokens: read + post on their own inbox `foreman:{id}`, plus
  post-only on the factory's main inbox `factory:main` (job.event #9,
  forwarded job.result #4, post_to_factory #18). The pair response's
  `parent_url` names the factory base URL; `factory:main` is the inbox id
  that foreman-originated posts target.
- The foreman keeps a LOCAL `factory:main` copy as a staging inbox
  (#33): `post_to_factory` (§9.3) writes there, and the parent
  forwarder loop drains it, forwarding every envelope verbatim to the
  factory-side `factory:main` over HTTP and acking locally. Without the
  forwarder the staging copy is a dead end - brain replies never reached
  the factory.
- Foreman report routing (2026-11-16 amendment): `post_to_factory`'s `to`
  is optional, and the only destination it routes is `factory:main` (§9.3). Any
  other `to` - notably `factory:planner`, which is the planner's auth identity
  and not an inbox, or `planner:main`, the operator inbox that foreman
  credentials cannot write - is refused at post time with `unroutable_address`;
  nothing is staged. The factory consumer relays `foreman.report` and
  `brain.failed` verbatim into `planner:main` and wakes the planner (§9.5), so a
  foreman reaches the operator through `post_to_factory` alone; the foreman
  holds no new inbox scope, and the relay is factory-side.
- The foreman also keeps a LOCAL brain queue sub-inbox
  `foreman:{id}:brain` (spec §3.3, 2026-11-15 amendment): brain-delegated
  envelopes are enqueued there by the consume loop and drained by the
  brain queue loop. It is internal - no token is scoped to it, and
  nothing outside the foreman process reads or writes it.
- Worker tokens (resolution of TODO-2; corrected #11): post-only on
  `foreman:{id}` (job.result, job.question) and read-only on the per-job
  inbox `job:{job_id}` (job.answer; the foreman creates it at dispatch).
  Invalidated on terminal job state (hook interface defined in #11; #9 wires
  it).
- Ack authorization: `POST .../ack` requires `may_read` on that inbox.
- Enforcement: `401` unknown/invalid token; `403` wrong scope (pinned test
  cases: post to a foreign inbox; read without `may_read`; ack without
  `may_read`).

Inbox-id formats (pinned with the TODO-2 resolution): `foreman:{id}` (child's
own inbox at the factory), `factory:main` (factory processing inbox),
`job:{job_id}` (per-job answer inbox). Workers post `job.result` and
`job.question` to `foreman:{id}`; `job.answer` lands in `job:{job_id}`.

### 7.4 Worker-token invalidation hook (#9, #11)

Called exactly once on terminal transition; registered by #11, wired by #9.
Execution site (integration note, #21): worker tokens live in the FOREMAN's
auth store, so revocation runs foreman-side - the foreman's local consumer
loop calls InvalidateJob(job_id) when it routes a terminal job.result.
Amendment (2026-11-17, usage-401): for a job whose harness runs in this
foreman process, that revocation is deferred until the harness returns
(runWorker revokes on exit; a job with no live local harness is still
revoked at result routing). The harness posts job.usage with the per-job
bearer AFTER the agent child exits, which is after report_result when the
agent reported mid-run; revoking at result routing made that post 401 and
silently lost the worker usage row. The credential is still revoked before
any new work for the job can start, a revoked token still cannot post, and
the inbox's per-(inbox, job) job.result dedupe (409 = idempotent success)
means the extra live window cannot produce a second result. The factory-side OnTerminal hook still fires for jobs-bookkeeping (over-budget
flagging).

### 7.5 TLS posture (#11)

Bearer-only over a trusted LAN in v1; `base_url` configurable. TLS termination
is via a reverse proxy - documented here, not in code: components serve
plain HTTP; front them with a TLS-terminating reverse proxy and point each
`base_url` at its externally reachable URL.

## 8. Worker harness contract (env/CLI)

### 8.1 Process interface (#5)

Spawned by the foreman runtime:

```
worker -job <path/to/job.json>
```

Environment: `WORKER_RESULT_URL`, `WORKER_MCP_URL`, `WORKER_TOKEN`,
`WORKER_DIR`. Nothing else crosses the process boundary.

### 8.2 Job directory (#5)

Fresh dir per job: `WORKER_DIR/jobs/{job_id}/`; the process cwd is that dir;
no shared state.

### 8.3 Prompt construction (#5, #28)

```
{task}\n\nMachine input:\n{payload JSON}\n\nReport with the inbox_report_result tool. If you cannot, your final stdout is used as fallback.
```

The tool name is `inbox_report_result` (#28): the generated config mounts
the worker MCP server under the name `inbox`, and opencode exposes MCP
tools with a `<server>_<tool>` prefix - the §8.3 wording must use the name
the agent actually sees. The Claude Code runner (§8.4) exposes the same
server's tools as `mcp__inbox__report_result`, so a claude-routed job's
prompt uses that spelling; task, machine-input block, and the fallback
sentence stay byte-identical across runners.

### 8.4 Child CLI invocation & generated config (#5, #13, #28, #31; claude amendment 2026-11-16)

- Runner routing (2026-11-16 amendment): the pinned model string routes
  the child CLI. A bare `claude-*` model id (or the bare `claude` alias),
  optionally behind the `anthropic/` provider prefix, runs via the
  Claude Code CLI in non-interactive mode (below); everything else -
  including OpenRouter-hosted claude models (`openrouter/...`: a
  different credential path, the OpenRouter key, not the Anthropic CLI's
  own auth) - runs via opencode. The routing decision lives in the
  harness (`RunnerFor`); the tier model mapping (§6.5) is unchanged: an
  operator points a worker tier at a claude model via its existing env
  override, and that tier's jobs run on the claude runner.
- opencode invocation (#28, #31): `opencode run --standalone [-m <model>]
  "<prompt>"`; must be non-interactive. `--standalone` boots a private
  server inside the child process: the shared background service would
  substitute the config's `{env:...}` credential headers against its own
  environment - which has no per-child tokens - and every MCP connect
  would die with 401 "missing bearer token", silently stripping the child
  of its tools. The pinned model rides `-m`, never only the config's
  `model` field: V2 drops `#variant` suffixes (reasoning-effort levels
  like `#max`, §6.5) from the config field, so the flag is the only
  reliable injection point. Verify exact flags against `opencode --help`
  during implementation.
- Claude Code invocation (2026-11-16 amendment; flags verified against
  the installed CLI, 2.1.325): `claude -p [--model <model>] --mcp-config
  <claude-mcp.json> --strict-mcp-config --permission-mode
  bypassPermissions --no-session-persistence "<prompt>"` - the same
  per-job cwd, prompt (§8.3), child env (WORKER_TOKEN guaranteed), flag
  file (§8.5), and fallback reporting (§8.5) as the opencode runner.
  `-p`/`--print` is the non-interactive mode; `--strict-mcp-config` makes
  the generated config the ONLY MCP source (the isolation analog of
  opencode `--standalone`: no user/project MCP servers leak into a
  per-job child); `--permission-mode bypassPermissions` is the headless
  analog of the opencode allow-all permissions (a headless run has no one
  to answer a permission prompt); `--no-session-persistence` keeps the
  one-shot worker from persisting sessions; the prompt is the FINAL
  positional argument - `--mcp-config` is variadic and would otherwise
  swallow it (observed live). The `anthropic/` provider prefix is
  stripped for `--model` (the CLI takes bare ids and aliases).
- Claude Code generated config (2026-11-16 amendment): the MCP injection
  lands at `<job dir>/claude-mcp.json`, passed explicitly via
  `--mcp-config` (never name-discovered; a non-`.mcp.json` name also
  keeps claude's project scan from double-loading it), written 0600
  before every run:

```json
{
  "mcpServers": {
    "inbox": {
      "type": "http",
      "url": "WORKER_MCP_URL",
      "headers": { "Authorization": "Bearer ${WORKER_TOKEN}" }
    }
  }
}
```

  `type: "http"` is Claude Code's streamable-HTTP transport (the opencode
  `remote` analog). The token is referenced via Claude Code's `${VAR}` env
  expansion so the secret never lands in the file at rest; the expansion
  was verified live (the child connects with the expanded bearer). There
  is no generated permissions block on this runner - headless permission
  policy rides `--permission-mode` (above) - and no generated model field
  (the model rides `--model`).
- Loud failures (2026-11-16 amendment): a routed runner that cannot run
  fails the job loudly, never silently. A missing CLI surfaces at exec
  (start failure → §8.5 crash result). Claude Code reports in-run
  failures - missing auth among them - as the result on STDOUT with
  empty stderr and a NON-ZERO exit (observed live on 2.1.325: `Not logged
  in · Please run /login`, exit 1), so the exit code contract already
  catches them; on the claude runner the crash message falls back to
  stdout when stderr is empty (the opencode runner keeps the
  stderr-or-exit-status semantics, §8.5). An unauthenticated claude run
  therefore surfaces as a failed job with the auth error as the crash
  message - consistent with the §8.5 spawn-failure surfacing.
- Config discovery (#28): the generated config is written to the V2
  project-config path `opencode.json` IN THE CHILD'S WORKING DIRECTORY
  (worker: the per-job dir; foreman brain: its WorkDir; planner: its
  WorkDir when routed to the opencode runner - a claude-runner planner
  session writes §9.4's claude-mcp.json instead). V2
  opencode discovers `opencode.json` from the working directory - the
  V1-era `OPENCODE_CONFIG` env var (and the `.opencode.json` dotfile name)
  are IGNORED by V2 (upstream regression, umbrella issue #36990), and
  pointing the child at a temp file silently loses MCP injection,
  permissions, and the model pin. The file is rewritten before every run,
  0600; tokens are referenced via `{env:...}` substitution so secrets
  never land in the file at rest (the harness guarantees the env var is
  present in the child env).
- MCP injection (worker shape; planner and foreman brain mount their own
  URL under the same pattern):

```json
{
  "permissions": [
    { "action": "*", "resource": "*", "effect": "allow" }
  ],
  "mcp": {
    "servers": {
      "inbox": {
        "type": "remote",
        "url": "WORKER_MCP_URL",
        "oauth": false,
        "headers": { "Authorization": "Bearer {env:WORKER_TOKEN}" }
      }
    }
  }
}
```

- Permissions (#27): every generated child config (worker, foreman brain,
  factory planner) pins the one allow-all rule shown above. A headless
  `opencode run` has no client to answer an `ask`; an unanswered ask is
  auto-declined and aborts the whole session mid-flight (observed live:
  exit status 1, completed work discarded). The children run with the
  enclosing service user's authority either way, so gating them is the
  enclosing service's job, not the prompt's.
- max_tokens: the opencode 1.18 config schema exposes no per-request
  max-output-tokens setting, so the pin "config sets model max_tokens =
  budget.max_output_tokens" is withdrawn; #13 proxy 2 is deferred - wall
  clock (proxy 3) and post-hoc usage accounting (proxy 4) remain the
  enforcement path.
- Model strings per role come from the tiering config (§6.5), injected via
  the `opencode run -m` flag (worker jobs select their tier per
  §5.1/§6.5: flash -> GLM 5.3 Flash, default -> GLM 5.3 Flash max-reasoning, hardest ->
  GLM 5.3); the generated config's `model` field stays as documentation
  only, since V2 drops `#variant` suffixes there (#31). On the claude
  runner the model rides `claude --model` instead (the generated
  claude-mcp.json carries no model field).
- The routed runner's CLI must be on PATH - the foreman's responsibility
  (#5; #12 detects it at startup): opencode for the GLM tiers, the Claude
  Code CLI for claude-* models. Both install scripts (§7.1) install both
  CLIs so spawned workers can use either; the visibility check is
  PATH-aware (systemd's default service PATH, not the service user's
  interactive PATH).

### 8.5 Reporting: primary & fallback (#5, #17, #1)

- Primary: the agent called `report_result` (harness detects the flag file
  at `WORKER_DIR/jobs/{job_id}/.result_posted`, written by #17).
- Fallback: the harness posts a JobResult to `WORKER_RESULT_URL`:
  `status = exit==0 ? done : failed`; `error.kind = crash` when exit != 0;
  `summary` = first 240 chars of stdout.
- Empty stdout: summary is `(no output)` (satisfies the 1-240 rule, #3).
- Safety net: a second `job.result` for the same `(inbox_id, job_id)` is
  `409` with the original envelope id (#1).
- Spawn-failure surfacing (2026-11-15 amendment): a spawned worker runs
  under the foreman's lifetime context, never the spawner's request
  context - the `spawn_worker` MCP request context is canceled the moment
  the tool call returns, and a worker inheriting it died instantly while
  the dispatch looked successful. When the harness errors without
  delivering a report (and the foreman is still up), the foreman
  synthesizes a `job.result` (`status=failed`, `error.kind=other`,
  `summary=(no output)`, sender `worker:{job_id}`) into its own inbox,
  which routes to the factory like any worker report; the factory applies
  it terminal-idempotently, so it is ignored if the harness did deliver a
  report. A worker death can therefore only ever surface as a visible job
  failure, never a silent fake success. The claude runner (§8.4) leans on
  the same semantics: a missing CLI surfaces at exec as a §8.5 crash
  result, and a claude auth failure exits non-zero (the §8.5 fallback
  contract maps it to `failed`/`crash`, with the crash message taken from
  stdout when stderr is empty).

### 8.6 Timeout (#5)

Kill the entire process group at `timeout_s`; post a JobResult with
`error.kind = timeout`.

### 8.7 Worker lifecycle across a foreman restart (2026-11-16 orphan-recovery amendment)

Workers are children of the foreman process (§8.1, §8.4): their goroutine,
process group and per-job credentials all die with it. The factory does not
learn of the restart by itself, so a job the previous foreman process was
running stays `running` at the factory with nothing behind it. Live
incident: brain-claude-routing job `job_01M7J7SH29WG7T4WQFFKNZD3H1`
dispatched 13:12:24Z; the foreman restarted 13:14:36Z; the worker died
mid-flight; the registry kept `running` until an operator cancelled it at
13:45Z. This amendment makes a starting foreman reconcile the registry
against the processes it actually has.

**When and what.** Once per foreman process start, inside `Run`, before any
loop starts (the inbox client, capabilities push, brain queue, parent
forwarder). No spawn can race the pass: a job's worker exists only after a
spawn, and no spawn path runs before the loops. The pass covers only jobs
owned by THIS foreman:

- It lists `GET /v1/jobs?state=running&foreman=<owner>&limit=1000` once for
  each owner spelling a foreman's jobs carry in `foreman_id`: `foreman:{id}`
  (the inbox identity) and the bare `{id}`. A returned row whose
  `foreman_id` is not one of those spellings is ignored. Another foreman's
  jobs are never listed, probed, or posted.
- The reconciled state is `running` only. `dispatched` is not reconciled: the
  foreman's durable inbox may still hold its unacknowledged `job.request`,
  and failing the job could race its re-spawn. `pending` and `failed` have no
  worker (a `failed` job's retry is already armed at the factory). Terminal
  jobs are not touched.

**Live workers are protected.** A job is live when some process's working
directory is its job directory `<data>/jobs/<job_id>` or lies beneath it.
That holds for a worker child, which runs with the job directory as its cwd
(§8.4), and for anything it started that still has that cwd. The probe reads
`/proc/*/cwd`. Exited processes and processes owned by another user are
ignored. Any other read error aborts the whole pass, so nothing is failed
without a complete view of the process table. A job this process spawned
(recorded at spawn) is never reconciled. A live job stays `running`; its
worker's results still post normally, because its credentials are still
valid.

**Orphans are failed loudly, and the retry re-arms.** For each job with no
live worker, the foreman posts one `job.result` to `factory:main`:
`from: foreman:{id}`, `status: failed`, `error.kind: crash`, summary
`worker orphaned by foreman restart`, and an `error.message` naming the job
and the restart. The factory applies it through the ordinary result path
(`running -> failed`, §6.1) and arms the crash retry (§6.4: 2 retries on the
30s / 2m backoff). The picker then re-dispatches the same job id (§6.4), or
the job dead-letters once retries are exhausted. The result is
terminal-idempotent (§6.4): a late genuine result for the same job is
ignored. The foreman also revokes the job's worker credentials (§7.4); with
no process behind them they are dead.

**Failure handling.**

- Registry listing fails: the pass retries up to 5 attempts, 2s apart, at
  startup. If every attempt fails, the foreman still starts, logs
  `ORPHAN RECOVERY SKIPPED`, and any stranded jobs fall to the §6.2 sweeper.
- A result post fails: the pass continues with the remaining jobs. The
  failed job stays `running` until the next foreman start or the sweeper.
- A listing that returns the full 1000-row page is logged as truncated; the
  remainder is handled by the next start.

**Observability.** Each orphan logs `ORPHAN <job_id>`. The registry carries
the message in `last_error`, and it surfaces in `GET /v1/jobs/{id}` and the
dead-letter view if retries exhaust. A pass logs its outcome: listed,
orphaned, live, and local job ids.

## 9. MCP surfaces

### 9.1 Common mechanics (#6, #15, #17, #18, #19)

- MCP is the veneer for LLM callers only; non-LLM paths (scheduler, web UI,
  heartbeats) talk plain HTTP and never instantiate MCP (#6). The
  provisional tool names in #6 are superseded by the exact lists below and by
  the #14 terminology decision.
- Transport: streamable-HTTP MCP (#17, #18, #19).
- Implementation: `github.com/modelcontextprotocol/go-sdk` (#17; allowlist
  #15). Pending decision: if streamable-HTTP server use is blocked, hand-roll
  a minimal jsonrpc-over-HTTP MCP (2 tools) and record the decision in this
  section (#17, #15).
- Tool descriptions stay tiny (per-turn token cost for every LLM caller,
  #6); byte budgets asserted by test: worker surface < 1KB total (#17);
  foreman surface < 1.5KB (#18; 2KB since the 2026-11-16 `raise_alert` tool); factory surface < 4KB (2026-11-16
  inbox redesign: raised from 2KB for the task tools of §9.5.2 and
  the `task_id` arguments, with headroom for the alert tool, which S3 added as `raise_alert` (§9.5.3); 2026-11-15
  amendments: raised from 1.5KB for the incident-driven `cancel_job` tool,
  2026-11-16: the `raise_alert` tool (§9.5.3) fits the 4KB budget, and `create_job` must state the requires tag-name forms - §6.7 -
  because undocumented semantics let the planner pass capability
  values as requires, parking two jobs pending for ~7h). Names,
  descriptions, and advertised input/output schemas all count toward the
  budget (2026-11-14 amendment: typed handlers advertise input schemas so
  callers can introspect argument names).
- LLM sessions (worker agent, foreman brain, factory planner) all reuse the
  #5 harness pattern - a generated per-child config plus one
  non-interactive CLI session, differing in MCP URL + prompt (#5, #18,
  #19). The child CLI routes on the pinned model: workers per §8.4
  (claude-* ids via the Claude Code CLI, else opencode), the factory
  planner per §9.4.
- Decision (append-only, #17): implemented with
  `github.com/modelcontextprotocol/go-sdk` v1.8.0 (streamable-HTTP server,
  stateless + JSONResponse modes, bearer auth as wrapping middleware);
  hand-roll not needed.

### 9.2 Worker surface: `/v1/mcp/worker` (#17)

`internal/mcpworker`; mounted on the foreman. Bearer = per-job worker token
(#11 scope); it resolves `job_id` and provides no other auth context.

Tools (exact; opencode exposes each as `<server>_<tool>` - with the server
named `inbox`, the agent sees `inbox_report_result`, `inbox_ask_parent`,
`inbox_spawn_child`, and `inbox_collect_children` (#28), and §8.3's prompt
wording matches):

- `report_result(status, summary, result?, error?, usage?)` - validate via
  `internal/wire`; post the JobResult to the result inbox (409 dedupe
  applies); write the flag file the harness checks (#5); return
  `{"ok": true}`.
- `ask_parent(question, wait_seconds=60)` - post `job.question` to the
  foreman's inbox `foreman:{id}` (§3.2); long-poll `job:{job_id}` for
  `job.answer`; return the answer; on timeout return "proceed with your best
  judgment" (never blocks forever).

Sub-spawning (2026-11-15 amendment): a worker can split ONE large dispatch
into child worker jobs without funneling anything through a foreman-brain
session - brain/chat history stays small, the parent worker does the
collecting:

- `spawn_child(task, result_schema?, tier?, timeout_s?, budget?)` - builds a
  child JobSpec with `parent_job_id` set to the calling job (§5.1) and
  enqueues it through the SAME Spawn seam the foreman brain's `spawn_worker`
  uses (§9.3): the child is an ordinary foreman-spawned job - registry
  visible via the accepted/started events (§6.2; no second visibility
  path), run by the same harness, and loud on harness failure (the §8.5
  synthesized failed result applies at the factory AND is copied to the
  parent's per-job inbox, so the parent sees the failure too). Returns
  `job_id` immediately (never blocks on the child). DEPTH is hard-capped at
  ONE level: a job that is itself a spawned child (its persisted spec
  carries `parent_job_id`) is refused - children never spawn children, and
  the refusal is a loud tool error. Inheritance and caps, enforced against
  the caller's persisted JobSpec: the child's `tier` is the caller's
  argument, or the parent's tier when unset; `timeout_s` defaults to and is
  hard-capped at the parent's timeout_s (a child never outlives its
  parent); every budget field defaults to the parent's field and is
  hard-capped at it (a child never out-spends its parent). `created_by` is
  `worker:{parent_job_id}`.
- `collect_children(wait_seconds=60)` - long-polls the CALLER's per-job
  inbox `job:{job_id}` for terminal `job.result` copies of the children the
  current attempt spawned: the foreman's consumer copies every child's
  terminal result there (byte-identical payload, §3.4) before forwarding it
  to the factory, so collection never routes through the foreman brain
  (zero LLM calls). The copies include the §8.5 synthesized spawn-failure
  results. Non-matching envelopes stay untouched (`job.answer` remains
  available to `ask_parent`; copies of other/previous attempts stay in
  place), returned copies are acked (consumed). Returns
  `{"results": [...]}` as soon as at least one child result is pending,
  `{"results": []}` once `wait_seconds` elapses (capped at 60, like
  `ask_parent`); the caller re-polls until every `spawn_child` job_id has
  reported or it gives up. The child filter (which copies belong to the
  current attempt) is foreman-side state next to the spawner: a restarted
  foreman - which kills its whole worker tree anyway - collects nothing
  rather than a previous attempt's stale results.

The depth cap and the inheritance caps read the caller's persisted JobSpec
(the spawner's `jobs/{id}/job.json`) through a foreman-provided seam; the
Spawn seam itself is unchanged.

### 9.3 Foreman brain surface: `/v1/mcp/foreman` (#18)

`internal/mcpforeman`; mounted on the foreman. Bearer = foreman brain token
(#11). The brain never touches raw HTTP (#18).

Tools (exact):

- `inbox_next(wait_seconds=0)` - oldest unacked envelope.
- `inbox_ack(ids)`.
- `post_to_factory(type, payload, job_id?, to?, task_id?)` - envelope to the parent
  inbox. `task_id` (optional) sets `payload.task_id` so a report sent outside a
  job threads into that existing open task (§9.5.2 resolution order); a
  non-object payload is wrapped as `{text, task_id}`. 2026-11-16 report-routing amendment: `to` is optional and accepts only
  `factory:main` (the default); any other value is a tool error
  `unroutable_address` that names the routable path and is never staged
  (`factory:planner` and `planner:main` get targeted hints). A `foreman.report`
  requires a `payload` and reaches the operator via the factory relay (§9.5).
- `spawn_worker(task, result_schema?, tier?, timeout_s?, budget?)` - creates a local
  JobSpec, enqueues it to the harness, returns `job_id` immediately (never
  blocks). `tier` picks the worker model (§5.1, §6.5): flash (GLM 5.3
  Flash, cheapest, also the no-tier default), default (GLM 5.3 Flash max-reasoning,
  same low price), hardest
  (GLM 5.3) - bias towards the cheapest tier that can do the job (#29).
- `answer_worker(job_id, answer)` - posts `job.answer` to the job inbox.

Brain session mechanics: the #5 harness pattern (#18) - same code path as
the worker harness, different MCP URL + prompt - with runner routing
(2026-11-16 foreman-brain-claude amendment, the §9.4 planner twin). The brain
token is a LOCAL foreman credential on the foreman's own auth store (the
factory-paired bearer lives in the factory's store and cannot authenticate
against the foreman's local endpoints); the foreman mints it at startup (#21).

- Runner routing: the brain session model (`FACTORY_FOREMAN_BRAIN_MODEL`,
  §6.5) routes the child CLI with the §8.4 semantics exactly. A bare
  `claude-*` id (or the bare `claude` alias), optionally behind the
  `anthropic/` provider prefix, runs via the Claude Code CLI in
  non-interactive mode (below). Everything else - including OpenRouter-hosted
  claude models (`openrouter/...`, the OpenRouter key's credential path) -
  runs via opencode, unchanged: `opencode.json` in the brain WorkDir,
  `opencode run --standalone -m <model>`, allow-all permissions.
- The default stays the GLM pin (`openrouter/z-ai/glm-5.3`); the claude route
  is opt-in by env pin. A foreman is not pinned to a claude-* model until its
  binary carries the claude runner: an older foreman would run the pin on
  opencode, where the model cannot resolve.
- Claude Code invocation (flags as §9.4, verified against the installed CLI,
  2.1.325): `claude -p [--model <model>] --mcp-config <claude-mcp.json>
  --strict-mcp-config --permission-mode bypassPermissions
  --no-session-persistence "<wake prompt>"`. The wake prompt (§6.6) is the
  FINAL argument. The session has the same brain WorkDir, 30m bound, child
  env (`FOREMAN_BRAIN_TOKEN` guaranteed), and loopback endpoint
  `/v1/mcp/foreman` as the opencode runner. The `anthropic/` prefix is
  stripped for `--model`.
- Claude Code generated config: `<WorkDir>/claude-mcp.json`, passed
  explicitly via `--mcp-config` (never name-discovered), written 0600 before
  every run:

```json
{
  "mcpServers": {
    "foreman": {
      "type": "http",
      "url": "FOREMAN_BRAIN_MCP_URL",
      "headers": { "Authorization": "Bearer ${FOREMAN_BRAIN_TOKEN}" }
    }
  }
}
```

  The server name `foreman` makes the surface arrive as
  `mcp__foreman__<tool>` (e.g. `mcp__foreman__inbox_next`); the opencode
  runner exposes the same tools as `foreman_<tool>`. The model rides
  `--model`, and the headless policy rides `--permission-mode`: the config has
  no model field and no permissions block.
- Shared code: both coordinator runners (§9.3 brain, §9.4 planner) run
  through one helper (`internal/claudecli`: the routing predicate, the
  argument vector, the config writer, and the bounded run with its error
  semantics). The worker harness keeps its §8.4 copy.
- Loud failures: a missing Claude Code binary fails the session by name
  (`claude CLI not found`). An unauthenticated run surfaces Claude Code's
  auth text (on stdout with empty stderr, §9.4). A session past its 30m bound
  reports `brain session exceeded`; a caller deadline (the job.question SLA)
  reports as cancellation, not as the bound. Every failure is returned from
  the brain session, and the router logs it (`routing: <type> <id>: brain:
  <error>`). No failure is reported as success.

The `spawn_worker` seam is shared (2026-11-15 sub-spawning amendment): the
worker surface's `spawn_child` (§9.2) enqueues through the same Spawn seam,
so worker-spawned child jobs get the identical registry-visibility (§6.2)
and loud-failure treatment as brain-spawned jobs. The brain surface itself
gains nothing: children are the parent worker's responsibility, and no
child result ever relays through a brain session.

### 9.4 Factory planner surface: `/v1/mcp/factory` (#19)

`internal/mcpfactory`; mounted on the factory. Planner token auth. The
planner never formats raw HTTP or picks hosts itself (#19).

Tools (exact):

- `list_foremen()` - `{foremen: [{id, online, capabilities}]}` (record-
  wrapped; 2026-11-14 amendment: MCP structured content is an object -
  bare arrays are rejected by client harnesses).
- `list_jobs(state?, limit=50)` - `{jobs: [{id, state, summary, usage,
  over_budget}]}` (summaries only, per #7; record-wrapped per the
  `list_foremen` amendment).
- `create_job(task, requires?, result_schema?, tier?, timeout_s?, priority?,
  schedule_cron?, foreman?)` - ad hoc dispatch via the Matcher interface;
  `requires`
  entries are capability tag names - accepted forms `os:linux`,
  `arch:amd64`, `tools:<name>`, or a bare tag name (`os`, `arch`,
  `tools`) - never capability values like `linux`/`amd64`; entries naming
  no registered foreman tag key are rejected with error
  `unknown_requires` at creation (2026-11-15 amendment: values parked
  jobs pending silently for ~7h; the response carries a `warnings` array
  when requires could not be validated because no foremen are
  registered). `tier`
  selects the worker's model tier (§5.1, §6.5: flash -> GLM 5.3 Flash,
  the cheapest and the no-tier default; default -> GLM 5.3 Flash max-reasoning;
  hardest ->
  GLM 5.3) - bias towards the cheapest tier that can do the job (#29);
  with `schedule_cron`, also creates a schedule via
  the #8 API (§6.8). Refused with error `spend_cap_exceeded` when the
  daily spend breaker is open (#13). Optional `task_id` (inbox redesign
  S2, §9.5.2) files the job under a task (`task_jobs`), defaulting to the
  session's woken task; the link is written before dispatch, the result
  carries `task_id`, and an unknown task is a tool error with no job
  created. Optional `foreman` (2026-11-16 foreman-pin amendment, §6.7)
  pins the job to that foreman (bare or `foreman:{id}` form): the pin
  wins over the Matcher, requires validation is skipped (requires stay
  informational), an unknown foreman is a tool error (`unknown
  foreman`), and an offline pin leaves the job pending with a
  `warnings` entry until the foreman returns.
- `read_result(job_id)` - full result payload; the ONLY path by which full
  payloads enter LLM context (#19; #7 point 3).
- `cancel_job(job_id, reason?, supersede?)` - terminal-cancels a
  non-terminal job through the §6.1 state machine (`pending | dispatched |
  running | failed -> cancelled`, or `-> superseded` with `supersede=true`);
  returns `{ok, job_id, state}` with the resulting state. A terminal job is
  a tool error naming the state; an unknown id is a tool error
  (`unknown job`). The planner's alternative to direct `jobs.db` writes
  (2026-11-15 amendment).
- `message_foreman(foreman_id, type, payload)` - envelope to that
  foreman's inbox; `foreman_id` accepts the bare id or the full
  `foreman:{id}` form (normalized - never double-prefixed); `payload`
  (any JSON value) is required; an id matching no registered foreman is
  a tool error (2026-11-14 amendment: a mis-addressed relay must fail,
  not open an inbox nothing reads).
- `notify_operator(text, task_id?)` - posts a `brain.reply` envelope into
  the operator inbox (`planner:main`, §9.5); the ONLY way the LLM planner
  talks back to the human. It is filed under `task_id`, else the woken
  task, else a NEW task titled from the text (§9.5.2), so a reply is never
  orphaned; an unknown task is a tool error. Returns `{ok, id, task_id}`.
- Task tools (inbox redesign S2, §9.5.2): `list_tasks(status?=open|closed|
  all (default open), q?, limit?=50)` -> `{tasks}`; `get_task(task_id)` ->
  `{task, messages}` (the last 40 thread messages with an `internal` flag,
  each payload 2KB-capped and rendered as a string); `update_task(task_id,
  title?, priority?, status?=open|closed, reason?, needs_operator?)` ->
  `{task}` (closing defaults `reason` to `planner`; a reopen ignores it;
  `needs_operator` true|false sets or clears the task's "waiting on the
  operator" flag, §9.5.2; a planner close never wakes the planner);
  `set_priority_criteria(text)` -> `{ok}`.

Input schemas (2026-11-14 amendment): every tool on this surface
advertises a typed input schema - argument names and required fields are
introspectable by MCP clients, unknown arguments are rejected, and
validation failures are tool errors. Advertised schemas count toward the
§9.1 byte budget. Rationale: a `message_foreman` call passing `text`
instead of `payload` was silently dropped and dead-ended two operator
feature requests in an inbox no consumer drains.

Planner session mechanics: the #5 harness pattern (#19) with runner
routing (2026-11-16 planner-claude amendment, the §8.4 worker twin):

- Runner routing: the session model routes the child CLI - the §8.4
  semantics exactly. A bare `claude-*` model id (or the bare `claude`
  alias), optionally behind the `anthropic/` provider prefix, runs via
  the Claude Code CLI in non-interactive mode (below); everything else
  - including OpenRouter-hosted claude models (`openrouter/...`: the
  OpenRouter key's credential path, not the Anthropic CLI's own auth) -
  runs via opencode (§8.4: `opencode.json` in the planner WorkDir,
  `--standalone`, `-m`, allow-all permissions, unchanged). The session
  model resolves per session: `FACTORY_PLANNER_MODEL` > the §6.5 pinned
  default (`claude-opus-5-5`), so the factory planner runs on the
  claude runner out of the box and an env override to a non-claude
  string flips the runner back without a restart.
- Claude Code invocation (flags per §8.4, verified against the
  installed CLI, 2.1.325): `claude -p [--model <model>] --mcp-config
  <claude-mcp.json> --strict-mcp-config --permission-mode
  bypassPermissions --no-session-persistence "<prompt>"` - the same
  planner WorkDir, wake prompt (§9.5), 30m session bound, child env
  (`FACTORY_PLANNER_TOKEN` guaranteed), and loopback MCP endpoint
  (§9.5) as the opencode runner. The prompt is the FINAL positional
  argument - `--mcp-config` is variadic and would otherwise swallow it
  (observed live). The `anthropic/` prefix is stripped for `--model`
  (the CLI takes bare ids and aliases); the pinned default is an exact
  snapshot id, never the floating `opus` alias.
- Claude Code generated config: the MCP injection lands at
  `<WorkDir>/claude-mcp.json`, passed explicitly via `--mcp-config`
  (never name-discovered; a non-`.mcp.json` name also keeps claude's
  project scan from double-loading it), written 0600 before every run:

```json
{
  "mcpServers": {
    "factory": {
      "type": "http",
      "url": "FACTORY_PLANNER_MCP_URL",
      "headers": { "Authorization": "Bearer ${FACTORY_PLANNER_TOKEN}" }
    }
  }
}
```

  The server name `factory` is what Claude Code prefixes its tools
  with (`mcp__<server>__<tool>`), so the §9.4 surface arrives as
  `mcp__factory__create_job`, `mcp__factory__notify_operator`, and
  friends - the opencode runner exposes the same tools as
  `factory_<tool>` under its `<server>_<tool>` naming (§9.2).
  `type: "http"` is Claude Code's streamable-HTTP transport (the
  opencode `remote` analog). The token is referenced via Claude Code's
  `${VAR}` env expansion (evaluated against the child env) so the
  secret never lands in the file at rest. There is no generated model
  field (the model rides `--model`) and no permissions block (the
  headless policy rides `--permission-mode`).
- Loud failures: Claude Code reports in-run failures - missing auth
  among them - as the result on stdout with empty stderr and a
  non-zero exit (observed live on 2.1.325), so the session error
  surfaced to the operator (§9.5 brain.reply note) falls back to
  stdout when stderr is empty; an unauthenticated claude planner
  therefore names the auth error instead of failing silently.

### 9.5 Conversational surface: tasks, alerts and the operator inbox

The factory planner is conversational. Since the inbox redesign
(2026-11-16, operator GO 14:30Z) the operator's world is TWO things, rendered
by the web UI (§4.1 `GET /`), both stored in the factory's `inbox.db`:

- **Tasks** (§9.5.2): every conversation is a task. A message belongs
  to a task through `messages.thread_id`. The planner is woken per task and
  sees only that task's thread. Jobs are NOT part of the operator's
  interface: job traffic is stored in the task thread but hidden by default.
- **Alerts** (§9.5.3): notifications that are not chat (a pending job, an
  unhandled envelope, a failed planner, anything a foreman or the planner
  raises). They have a priority, a TTL, and are acked or replied to.

`planner:main` remains the planner's wake queue: the one inbox that the
operator's UI streams and the planner loop drains. Sections of this chapter:
§9.5.1 storage, §9.5.2 task API, relay stamping and per-task wakes,
§9.5.3 alerts, §9.5.4 cross-slice glue, §9.5.5 UI, §9.5.6 live acceptance. Where an earlier amendment contradicts
this chapter, this chapter wins.

Envelope types in `planner:main` (the "internal" flag is derived from the type,
never stored: only `chat.message` and `brain.reply` are operator-visible,
`tasks.IsInternal`, §9.5.1):

| Type | From / payload | Internal | Wakes planner | Notes |
|---|---|---|---|---|
| `chat.message` | `human:operator`, `{text}` | no | yes | `POST /v1/chat {text, task_id?}` / `POST /v1/tasks {text}`; unauthenticated like the UI surface, §7.5 |
| `brain.reply` | `factory:brain` or `factory`, `{text}` | no | no | the planner's `notify_operator` (§9.4), the planner-failure note, and the alert seed of §9.5.3 |
| `foreman.report` | `foreman:{id}`, free-form (typically `{text}`) | yes | yes | relayed verbatim from `factory:main`, stamped with its task (§9.5.2 relay stamping) |
| `brain.failed` | `foreman:{id}`, `{envelope_id, envelope_type, error}` | yes | yes | a foreman brain session failed on a delegated envelope (§3.3); relayed exactly like `foreman.report`, so a failed delegation is never silent |
| `notify.job` | `factory`, `{id, state, foreman, last_error, over_budget, summary}` and `job_id` | yes | no | terminal job transition (done/failed/timed_out/dead/cancelled/superseded); stamped with its task when attributable, else unthreaded; hidden by default, no chips (operator DECISION 2) |
| `task.changed`, `alert.changed` | synthetic, payload = Task / Alert | n/a | no | SSE hints only, NOT persisted (§9.5.2, §9.5.3) |
| `notify.pending`, `notify.unhandled` | `factory` | yes | no | **legacy**: no longer produced; since the redesign they are P1 and P2 alerts (§9.5.3). Old rows stay readable (backfilled into the legacy task) |

Planner loop (internal/factoryapp, twin of the consumer loop): drains
`planner:main`; `chat.message`, `foreman.report` and `brain.failed` wake the
planner, ONE session per touched task with that task's thread as context
(§9.5.2). A session is the §9.4 surface with the planner bearer and the model
from `FACTORY_PLANNER_MODEL` (default per §6.5; the child CLI routes on the
model: the Claude Code CLI for claude-* ids, opencode otherwise, §9.4).
Per-payload 2KB caps apply (§6.6). The wake reason names the trigger:
operator chat needs a reply; a foreman report must be relayed to the operator
and acted on. Everything else is informational and acked without a wake. A
failed session is never silent: a `brain.reply` note INTO the task AND a P0
alert (§9.5.4). The child's MCP endpoint is the factory's loopback address,
NOT the advertised URL (#28): the child runs on the factory's own host, and
the MCP SDK's streamable-HTTP server rejects non-loopback Host headers
(DNS-rebinding protection); the advertised hostname yields "403 invalid Host
header" and a session silently stripped of every factory tool. Acking never
deletes: history stays replayable via `GET /v1/inboxes/planner:main/recent`
and streams to browsers via the permissive SSE
`GET /v1/inboxes/planner:main/stream`.

#### 9.5.1 Storage: threads, tasks, alerts (inbox redesign S1, 2026-11-16)

All of it lives in the factory's `inbox.db` (package `internal/tasks`,
sharing the inbox's single connection). Additive DDL only, idempotent at
every start; existing files migrate in place.

- `messages.thread_id` (nullable) files a message under a task; `NULL` =
  unthreaded. Threads are global (not per inbox) and include acked rows.
  Ordering is `seq`.
- `tasks(id 'task_<ulid>' PK, title, status open|closed, priority INT
  default 2 (0 = most urgent), created_at, updated_at, closed_at NULL,
  close_reason, needs_operator INT NOT NULL default 0)`. Closing stamps
  `closed_at` once and clears `needs_operator` (unless the same update sets
  it); reopening clears `closed_at` and `close_reason`. `needs_operator`
  was added after the first release: an existing `tasks` table gets it by
  an idempotent `ALTER TABLE ... ADD COLUMN` at start (2026-11-17).
- `task_jobs(task_id, job_id, PK(task_id, job_id))`, indexed by job_id.
- `alerts(id 'alert_<ulid>' PK, priority INT, title, body, source,
  created_at, expires_at NULL = never, state open|acked|replied|expired,
  task_id NULL, acked_at NULL)`.
- `settings(key PK, value)`; key `priority_criteria` holds the operator's
  free-text ranking criteria.
- `tasks_migrations(name PK, applied_at, detail)`: one-shot migration
  guards.
- Timestamps in these tables are fixed-width UTC
  `YYYY-MM-DDTHH:MM:SS.mmmZ`, so text order is time order and a
  `YYYY-MM-DD` prefix selects a day.
- Internal vs operator-visible is derived from the envelope type and never
  stored: only `chat.message` and `brain.reply` are operator-visible;
  everything else (`foreman.report`, `brain.failed`, `notify.*`, `job.*`,
  unknown) is internal (`tasks.IsInternal`).
- Legacy backfill: exactly once per database, guarded by
  `tasks_migrations.legacy_backfill_v1` and written in the same transaction,
  so a crash retries cleanly. It creates ONE closed task
  `Legacy chat (pre-tasks)` (close_reason `migration`, created_at = oldest
  row's ts) and stamps it on every `planner:main` row that has no thread.
  Nothing is deleted, re-typed or re-acked. If `planner:main` is empty
  (fresh install), no task is created, but the guard is still written, so
  later rows are never swept in.
- The store is policy-free: TTL defaults, expiry tickers, wakes and the idle
  reaper live in the factory app, which uses the store's primitives
  (`ExpireAlerts(now)`, `TransitionAlert` compare-and-set, and so on).

#### 9.5.2 Tasks and per-task planner wakes (inbox redesign S2, 2026-11-16)

Every operator conversation is a task (§9.5.1 storage). Messages attach
to a task through `messages.thread_id`.

Chat and task API (unauthenticated, like the rest of the UI surface,
§7.5):

- `POST /v1/chat {text, task_id?}`: without `task_id` it opens a task
  titled with the first 60 characters of the text (whitespace collapsed;
  the planner may retitle). With `task_id` it posts into that task, and a
  closed task is reopened. An unknown `task_id` returns 404
  `unknown_task`; blank text returns 400. The response is 201 with the
  stored envelope (every field it had before, `thread_id` included) plus
  `task_id`.
- `POST /v1/tasks {text}` is the same as chat without `task_id` (the "new
  task" box).
- `PATCH /v1/tasks/{id} {title}` renames a task (operator rename from the
  UI). Same (absent) auth as the rest of this surface: the browser holds no
  token. The title is whitespace-trimmed; missing, empty or blank -> 400; more
  than 200 characters (runes, the same limit as alert titles; the 60 of
  `POST /v1/chat` only shortens DERIVED titles) -> 400; invalid JSON or a body
  over 64 KiB -> 400; unknown id -> 404 `unknown_task`. 200 returns `{task}`
  (Task JSON). Closed tasks MAY be renamed: a rename is bookkeeping, it does
  not reopen the task and leaves status and close_reason alone (there is no
  reason to freeze the label of an archived task, and the operator searches
  closed tasks by title). It bumps `updated_at` like any `update_task`, so it
  counts as activity (resets the idle-24h auto-close clock, re-ranks the
  task). On success a `task.changed` envelope (payload = Task) is published
  on `planner:main` SSE, so other tabs and the task list update live. It
  shares the store path with the planner's MCP `update_task`, whose
  semantics are unchanged (no length limit there); concurrent retitles are
  last-write-wins.
- Task JSON: `{id, title, status open|closed, priority int (0 = most
  urgent), created_at, updated_at, closed_at?, close_reason?, job_ids[],
  last_activity_at, preview, preview_from, awaiting, awaiting_since?,
  needs_operator}`. `preview` is the newest operator-visible
  message (`chat.message`/`brain.reply`) of the thread, whitespace
  collapsed, at most 120 characters, and "" if there is none.
  `preview_from` names its author: `operator` (`chat.message`), `planner`
  (`brain.reply`), or "" if there is none (display only).
  `awaiting` is `operator` iff the task is open AND its newest
  operator-visible message is a `brain.reply` (internal rows never count);
  otherwise (closed, no visible message, or the operator spoke last) it is
  `factory`. `awaiting_since` is the ts of that unanswered `brain.reply`
  (only when `awaiting` is `operator`).
- `needs_operator` (bool, 2026-11-17) is the planner's explicit flag that
  the task is blocked on an operator decision, approval or answer. It
  lives on the server, so every device agrees. The planner sets or clears
  it with `update_task(needs_operator=true|false)` (§9.4). It clears
  automatically when the operator posts on the task (a `chat.message`
  through `POST /v1/chat {task_id}`: the thread composer or the phone
  quick reply)
  and when the task is closed (operator close, planner close or reaper).
  The UI's "Needs you" list and Inbox badge use only this flag (§9.5.5).
- `GET /v1/tasks?status=open|closed|all&q=&limit=` -> `{tasks}`. `status`
  defaults to `all`, `limit` defaults to 200 (clamped at 1000), and a bad
  status or limit returns 400. Ranking: open first, then priority ASC,
  then last_activity_at DESC. `q` matches a title substring, an id prefix
  or a `YYYY-MM-DD` date prefix. `awaiting=operator|factory` keeps only
  tasks awaiting that party (a bad value returns 400); the operator list
  is in triage order: priority ASC (0 first), then oldest `awaiting_since`.
- `GET /v1/tasks/{id}?internal=1&limit=` -> `{task, messages: [{id, type,
  from, job_id, ts, payload, internal}]}`: the thread oldest first,
  limited to the last `limit` rows (default 500, max 5000). Internal rows
  (every type except `chat.message` and `brain.reply`) are excluded
  unless `internal=1` (or `true`). An unknown id returns 404.
- `POST /v1/tasks/{id}/close {reason?}` -> 200 `{task}`: the operator
  closes a task from the UI. `reason` is trimmed, defaults to `operator`,
  and is at most 200 characters (longer, or bad JSON, returns 400). The
  body may be empty. Closing a closed task is a no-op 200 that keeps the
  original reason and posts nothing. An unknown id returns 404
  `unknown_task`. A real close clears `needs_operator`, publishes
  task.changed, and posts an internal `task.closed_by_operator` envelope
  `{reason, text}` (from `human:operator`) into the task on
  `planner:main`. That envelope WAKES the planner on the task, the same
  way operator chat does (2026-11-17, operator decision; no confirm
  dialog), so the planner can cancel the task's running, pending and held
  jobs with `cancel_job`. The wake does not reopen the task. Planner
  closes (`update_task(status=closed)`) and reaper closes post nothing and
  wake nothing, so there is no close/wake loop. A later reply reopens the
  task as usual.
- `POST /v1/jobs` accepts `task_id` (the twin of `create_job`, §9.4). It
  is linked before dispatch, echoed in the 201 body, and an unknown task
  returns 404 `unknown_task`.
- SSE (I5): every change to a task (creation through chat, a post into
  it, reopen, close, update_task, job link, reaper close) publishes a
  synthetic, NOT persisted envelope `{type: "task.changed", thread_id:
  <task id>, payload: Task}` on the `planner:main` stream. Threaded
  messages also stream as before. Clients treat both as "re-fetch this
  task".

Job -> task attribution (relay stamping): `foreman.report`,
`brain.failed` and `notify.job` carry a job id. The job resolves to a task
through `task_jobs`. If the job is not linked, resolution walks the job
spec's `ParentJobID` (spawn_child children, which reach the factory
through `RegisterSpawned`, §6.2), and a child resolved this way is linked
itself. The relayed row is posted with that `thread_id` and stays internal
(hidden by default). Resolution order for a `foreman.report`/`brain.failed`
(2026-11-16 report-task-routing amendment): (a) the envelope `job_id`'s task,
as above; (b) else `payload.task_id` (also read from a legacy double-encoded
JSON-string payload), but only when it names an existing OPEN task - a CLOSED
task is treated as not found, never silently reopened by an automated report
(the safer choice: a reopen would resurrect finished work unseen, whereas the
fall-through is visible as a new task plus a P2 alert, whose body names the
ignored `task_id`); (c) else the behaviour below. If (a) and (b) both resolve
but disagree, the `job_id` task wins and the conflict is logged; the factory
never switches silently. A report that matches neither (a) nor (b)
OPENS A NEW TASK, titled from its payload (the first
non-empty `text`/`title`/`summary`/`message`/`error` field - also of an
object payload delivered as a JSON string, one level deep (2026-11-16
deploy-18 follow-up: no raw-JSON titles) - else
`<type> from <sender> (<job>)`), and the planner wakes on it there, so it
is never lost. An unattributed `notify.job` is stored unthreaded and is
hidden. `notify.job` envelopes also carry the job id in `job_id`.

Per-task wakes (operator DECISION 1):

- The planner loop maps every waking envelope (`chat.message`,
  `foreman.report`, `brain.failed`, `task.closed_by_operator`) to a task.
  A thread that names an existing task is used as is, and is reopened if
  closed: a report on a closed task reopens it, and the planner may close
  it again. The exception is `task.closed_by_operator`, which never
  reopens; its wake reason tells the planner to cancel the task's
  non-terminal jobs and not to reopen it. Under the S5 usage pause it is a
  non-chat wake (deferred until the resume). An envelope
  with no thread, or with a thread naming no task (a legacy client, or a
  relay whose task lookup failed), is attributed through its job, else
  given a new task, and the stored row is re-threaded. If even that fails,
  an unthreaded `brain.reply` note names the envelope.
- Envelopes are grouped by task and run as ONE planner session per
  touched task. Different tasks run IN PARALLEL with NO cap in v1. Two
  sessions never run concurrently on the SAME task: envelopes arriving
  while that task's session runs are coalesced (deduplicated by id) into
  one follow-up session, which starts when the current one ends. The hand
  off is decided under one lock, so no waker is lost.
- Concurrency seam: `WakeLimiter interface{ Acquire(ctx) (release func(),
  err error) }` gates every session at the dispatch point. The default is
  unlimited. `App.SetWakeLimiter` installs a cap, and
  `NewSemaphoreWakeLimiter(n)` is the ready-made one.
- Prompt: the wake reason, then the task header (id, title, status,
  priority, job ids), then the operator's `priority_criteria` text, then
  the task's own thread (last 40 messages, oldest first, internal rows
  included and marked `[internal]`, triggering rows marked `[new]`), each
  payload capped at 2KB (§6.6). It is not the global history.
- Woken-task binding: each session's MCP URL is the planner endpoint plus
  `?task_id=<id>`. The planner MCP auth wrapper copies that into the
  `X-Factory-Task-Id` header, which tools read (a client may also send the
  header directly). `notify_operator` and `create_job` default to it. Each
  session runs in its own work dir, `<DataDir>/planner-tasks/<id>`, which
  holds the generated config and is removed after the session, so
  parallel sessions never share a config file.
- Failures are never silent. A session error, a limiter refusal or a
  panic posts the `brain.reply` note "The planner could not run: ..."
  INTO the task (unthreaded if that post fails).
- Delivery: the loop acks envelopes once they are handed to the
  dispatcher. The dispatcher persists its in-flight set (the running and
  pending message ids per task) in `settings` key `planner_wake_inflight`
  on every change. At start-up the planner loop re-dispatches whatever
  that set still holds, so a restart mid-session re-runs the interrupted
  wake (at least once). A shutdown posts no failure note.

Reaper (operator DECISION 3): every 10 minutes, an open task whose
`last_activity_at` is at least 24h old, that has no non-terminal job (a
job id the jobs store does not know counts as terminal), and that has no
planner session running or queued is closed with close_reason
`idle-24h`, and task.changed is published. The planner normally closes
tasks itself (`update_task(status=closed)`) once their jobs are terminal
and the operator has been told; the reaper is the backstop.

#### 9.5.3 Alerts (inbox redesign S3, 2026-11-16)

Alerts are operator notifications that are NOT chat: they live in the S1
`alerts` table (§9.5.1) and render in the UI's left pane. Alert JSON:
`{id, priority, title, body, source, created_at, expires_at|null, state,
task_id|null}`; `priority` is an integer, 0 = most urgent.

- **Producers.** (1) The long-pending check (§6.7) raises a **P1** alert
  (source `factory:pending`; title names the job and wait, body carries the
  diagnosis, requires and foremen counts) instead of a `notify.pending`
  row. (2) An unhandled `factory:main` type raises a **P2** alert (source
  `factory:consumer`) instead of `notify.unhandled`. Neither produces a
  chat-visible row any more; the log lines stay. (3) The planner MCP tool
  `raise_alert(priority, title, body?, ttl_s?, no_expiry?, task_id?)`
  (source `planner`). (4) The foreman MCP tool `raise_alert` (same args)
  posts an envelope of type `alert.raise` (payload = the args) to
  `factory:main`; the consumer turns it into an alert with source
  `foreman:<id>`. A malformed or invalid `alert.raise` is never dropped: it
  raises a P2 alert (source `factory:consumer`) describing the rejection.
  Validation: priority >= 0, non-empty title (<= 200 B), body <= 8 KB,
  `ttl_s` > 0 and exclusive with `no_expiry`, `task_id` must exist.
- **TTL.** Default by priority: P0 and P1 never expire, P2 24h, P3+ 4h.
  `ttl_s` overrides it and `no_expiry` removes it, for any priority
  including P0; `no_expiry` wins over defaults. `expires_at` is fixed at
  creation (factory clock). Expiry is applied lazily on every alert read,
  ack and reply, and by a 1-minute ticker (`ExpireAlertsPass`, clock
  injectable via `Config.Now`). Expired alerts leave `GET /v1/alerts`.
- **HTTP** (same unauthenticated UI-surface class as `/v1/chat`, §7.5):
  - `GET /v1/alerts` -> `{"alerts":[Alert]}`: open only, priority ASC then
    created_at DESC.
  - `POST /v1/alerts/{id}/ack` -> 200 `{"alert":Alert}`; unknown -> 404;
    already acked, replied or expired -> 409 `{"error":"already_terminal"}`.
  - `POST /v1/alerts/{id}/reply {text}` -> 201 `{"alert":Alert,"task_id"}`.
    Creates a task (title = the alert title, priority = the alert's),
    seeds its thread with a `brain.reply` from `factory` rendering the
    alert, then posts the reply as an operator `chat.message` carrying the
    task's `thread_id`, which wakes the planner like any chat. The alert
    becomes `replied` only if it was still open; an acked or expired alert
    still gets a task and keeps its state. `task_id` on the alert points at
    the new task. Empty text -> 400, unknown alert -> 404 (no task created).
- **SSE.** Every create and state change publishes a synthetic,
  non-persisted `alert.changed` envelope (payload = Alert JSON) on the
  `planner:main` stream (contract I5).
- **MCP sizes.** `raise_alert` pushed the planner surface to its raise_alert tool within the < 4KB budget of §9.5.2
  and the foreman surface to < 2KB (§9.1).

#### 9.5.4 Cross-slice glue (inbox redesign S5, 2026-11-16)

Behaviors that need both the task and the alert machinery, owned by neither:

- **Planner-session failure -> P0 alert.** When a planner session errors,
  panics, or is refused by the wake limiter, the factory posts the
  `brain.reply` note INTO the task (§9.5.2) AND raises a P0 alert, source
  `factory:planner`, title `Planner session failed`, body = the error,
  `task_id` = the task (when the store knows it). P0 never expires (§9.5.3).
  A shutdown posts neither.
- **Unattributed report -> task AND P2 alert.** A `foreman.report` or
  `brain.failed` that cannot be attributed to a task opens a new task
  (§9.5.2); the factory also raises a P2 alert, source `factory:relay`, title
  `Unattributed <type>: <task title>`, linked to that task, so the operator
  sees it in the alert pane even if no one opens the task. An unattributed
  `notify.job` stays unthreaded and hidden, with no alert.
- **Alert reply wakes the planner via `thread_id`.** `POST
  /v1/alerts/{id}/reply` posts the operator's reply as a `chat.message`
  carrying the new task's `thread_id`; the per-task wake (§9.5.2) groups on
  that thread, so the planner session's prompt is the task thread: the alert
  seed (`brain.reply`) then the reply. No alert-specific wake path exists.

All three are asserted end to end by `test/e2e/inbox_test.go`.

#### 9.5.5 UI (inbox redesign S4, 2026-11-16)

`GET /` is a single page. Tabs: Tasks (formerly Chat), Foremen, Jobs, Inboxes
(the last three unchanged). The Tasks tab has three panes: LEFT alerts
(priority badge, ack button, reply box; `GET /v1/alerts`, `POST
/v1/alerts/{id}/ack|reply`); CENTER a "new task" box (`POST /v1/tasks`), the
expanded thread of the selected task with a composer (`POST /v1/chat
{text, task_id}`) and a per-task "show internal" toggle (refetches
`GET /v1/tasks/{id}?internal=1`); RIGHT the ranked task list with an "only
active" toggle (default ON, `status=open`) and a search box (`q=`). Job and
other internal rows are hidden by default and carry no chips (operator
DECISION 2). Live updates: the `planner:main` SSE stream; any event
(`message`, `task.changed`, `alert.changed`) debounces a refetch, with a 10s
poll as fallback. If `/v1/tasks` or `/v1/alerts` 404s (slice not deployed),
the page degrades to the legacy chat log or an "alerts not available" note.

Task rename (2026-11-16): the title in the thread header is editable in
place. Click it (or focus it and press Enter/Space), or click the pencil
button next to it, to swap it for a text input (maxlength 200, selected).
Enter or blur saves via `PATCH /v1/tasks/{id}`; Escape cancels; an unchanged
title makes no request. While the input is open live refreshes do not
re-render the header, so typing is never clobbered. On failure (400/404/
network) the title reverts to the stored value and an inline `role="alert"`
message `rename failed: ...` stays beside it until the next edit. On success
the header and the task list row update immediately; other tabs follow
through `task.changed`. Keyboard: title (role=button, tabindex 0) and
pencil button are tab stops; focus returns to the title after save/cancel.
Not exercised by an automated browser test: no JS runtime or browser exists
in CI; `TestUIRenameHooks` only pins the markup and key handling.

Phone layout (2026-11-16). The same page has a separate phone UI (`#phone`).
It is chosen before first paint when the viewport is at most 760px wide, or
on a touch device in landscape at most 520px tall. `localStorage`
`factory.layout` = `phone`|`desktop` overrides this; the phone "More" screen
and a desktop "phone layout" button set it. The desktop layout is unchanged.
The phone UI uses the same endpoints, with hash routes so the browser back
gesture works: `#/inbox`, `#/task/<id>`, `#/jobs`, `#/job/<id>`, `#/foremen`,
`#/foreman/<id>`, `#/more` (usage and inboxes), and `#/ibx/<id>`. A bottom tab
bar has Inbox, Jobs, Foremen and More.

The Inbox shows, in order: open alerts (Ack and Reply buttons), "Needs you",
then the other open tasks; closed tasks appear on request. "Needs you" is
exactly the open tasks with `needs_operator` set (§9.5.2; 2026-11-17, this
replaces the earlier `preview_from` + per-device heuristic), and such rows
carry a "needs you" badge. The tab badge (and `navigator.setAppBadge` where
supported) counts open alerts plus needs-you tasks. Read/unread is a
per-device visual only (bold title and dot; `localStorage`, not synced): a
task is unread when the planner spoke last and this device has not opened
it since, or when it was marked unread by hand. It never affects "Needs
you" or the badge. The desktop task list shows the same small "needs you"
badge inline before the title of a flagged open task; the desktop layout is
otherwise unchanged.

Swiping a task left reveals Reply (a bottom-sheet composer; it posts
`POST /v1/chat {text, task_id}` without leaving the list) and Close (`POST
/v1/tasks/{id}/close`). Swiping right toggles read. The thread view has a
composer pinned to the bottom that is kept above the iOS keyboard
(`visualViewport`), and a menu with show internal, close, mark unread, copy
id and the linked jobs. All controls are at least 44px and all inputs at
least 16px. Safe-area insets are respected (`viewport-fit=cover`). The page
links a manifest and an apple-touch-icon so it can be added to the home
screen.

#### 9.5.6 Live acceptance (inbox redesign S5)

`scripts/inbox-live-acceptance.sh <factory-url>` drives the four steps with
curl against a running factory: (a) operator chat opens a task; (b) a planner
reply lands in the task thread; (c) a job-attached foreman.report is filed
under its task and hidden by default (visible with `?internal=1`); (d) a
foreman `raise_alert` shows in `GET /v1/alerts` and acks. Steps (c) and (d)
need credentials (a foreman bearer, the planner bearer) passed by
environment, as documented in the script header. The e2e suite
(`test/e2e/inbox_test.go`) is the automated twin.

## 10. Error model

```json
{ "error": { "code": "...", "message": "...", "retryable": true } }
```

`{"error":{"code","message","retryable"}}` is the normative error shape (#1).
Pinned status codes:

| Code | Context | Pinned in |
|---|---|---|
| `201` | POST message (server-assigned id/ts) | #1 |
| `200` | GET messages / GET message by id / SSE stream / pair | #1, #2, #11 |
| `204` | ack, including double-ack | #1 |
| `401` | unknown/invalid token; revoked token (row deleted) | #11 |
| `403` | wrong scope (post to foreign inbox; read without `may_read`) | #11 |
| `409` | duplicate `job.result` for the same `(inbox_id, job_id)`; response carries the original envelope id | #1 |
| `409` | cancel/supersede of a terminal job (`already_terminal`, `POST /v1/jobs/{id}/cancel`) | 2026-11-15 amendment |
| `404` | `POST /v1/foremen/{id}/messages` where `{id}` matches no registered foreman (`unknown_foreman`) | 2026-11-14 amendment |
| `404` | `POST /v1/jobs` / MCP `create_job` whose `foreman` pin matches no registered foreman (`unknown_foreman`, §6.7) | 2026-11-16 foreman-pin amendment |
| `400` | job creation (`POST /v1/jobs`, MCP `create_job`) whose `requires` entries name no registered foreman tag key (`unknown_requires`) | 2026-11-15 amendment |
| `404` | cancel of an unknown job id (`POST /v1/jobs/{id}/cancel`) | 2026-11-15 amendment |
| `404` | unknown task id (`POST /v1/chat` / `POST /v1/jobs` with `task_id`, `GET /v1/tasks/{id}`; code `unknown_task`) | 2026-11-16 inbox redesign S2 |

Pinned error codes: `spend_cap_exceeded` (#13), `unknown_foreman`,
`unknown_requires`. Validation rejects anything
outside the `result_schema` subset at validation time (#3).

## 11. Normative constants (quick reference)

| Constant | Value | Pinned in |
|---|---|---|
| SSE heartbeat comment `:hb` | every 15s | #2 |
| Foreman offline threshold | 90s since `last_seen` | #9 |
| Jobs sweeper tick | 10s | #9 |
| Retry picker tick | 10s | #10 |
| Job timeout grace | `timeout_s + 60s` -> `timed_out` | #9 |
| Retries by error kind | validation 0; timeout 1; crash/llm/other 2 | #10 |
| Retry backoff curve | 30s, 2m, 8m (attempt n waits `backoff[n-1]`) | #10 |
| SSE reconnect backoff | 1s, 2s, 4s, ... max 60s, +-25% jitter | #2 |
| Question SLA / `ask_parent` default wait | 60s; default answer "proceed with your best judgment" | #4, #17 |
| Summary length | required, 1-240 chars | #3, #7 |
| Fallback summary | first 240 chars of stdout | #5 |
| LLM context cap | 2KB per payload (except `read_result`) | #7 |
| Daily spend breaker | $20/day default; `FACTORY_SPEND_OVERRIDE=1` bypass | #13 |
| Pairing token | 32B random hex, 15-min TTL, single use | #11 |
| Bearer token | 32B random, stored SHA-256 only | #11 |
| Long-poll window | `wait_seconds` 0-60 | #1 |
| Inbox fetch limit | default 50, max 500 | #1 |
| MCP description budgets | worker < 1KB; foreman < 2KB (2026-11-16 raise_alert; was 1.5KB); factory < 4KB (2026-11-16 inbox redesign; was 2KB) | #17, #18, #19 |
| JobSpec defaults | `timeout_s` 300; `priority` 5 (1-9, 9 highest); `budget` `{max_prompt_bytes 32768, max_output_tokens 8192, max_cost_usd 5.0}` | #3 |
| Long-pending alert | `pending` with no matching online foreman for > 30 min -> one `notify.pending` in `planner:main` (dedupe per job per pending episode); check tick 1 min | 2026-11-15 amendment |
| Foreman orphan recovery | startup pass before any loop; listing 1000 rows per owner spelling; 5 attempts, 2s apart; orphan result kind `crash` | 2026-11-16 orphan-recovery amendment, §8.7 |

## 12. Foreman auto-update (v1) (2026-11-16 amendment)

Operator-approved plan 2026-11-15, GO 2026-11-16; shared contract
C1-C8 (`investigations/auto-update-contract.md`). Scope: foremen only,
all-at-once rollout, SHA-256 integrity only (no signatures), detection via
the capabilities response. This section pins the FOREMAN side (C1, C4-C7);
the factory advert (C3), the release manifest (C2) and the install-time
crash-loop guard (C8) are summarized where the foreman depends on them.

### 12.1 Build id (C1)

- Package `internal/buildinfo`: `var ID` is stamped at link time
  (`-ldflags "-X github.com/pricec/factory/internal/buildinfo.ID=$(BUILD_ID)"`).
  Format `<UTC commit time YYYYMMDDTHHMMSSZ>-<12-char lowercase git sha>`,
  e.g. `20261116T141100Z-38a6368abcde`.
- `Current()`: `ID` if stamped; else the embedded `vcs.time` +
  `vcs.revision` in the same format; else `"dev"`.
- `Newer(candidate, running)`: true only when both ids parse and the
  candidate's timestamp is strictly later. Ties (even with a different
  sha), `"dev"` and unparseable ids are false - never downgrade, never loop.
- The foreman logs `foreman build <id>` as its first startup line.

### 12.2 Build report (C4)

The 60s capabilities push (§6.7) carries `capabilities["build"] =
buildinfo.Current()`. The pairing payload is unchanged.

### 12.3 Advert (C3, factory side; foreman consumption)

`POST /v1/foreman/capabilities` answers 200 `application/json`
`{"foreman_update":{"build":"<id>","platforms":{"linux-amd64":{"sha256":"<hex>","path":"/download/foreman/foreman-linux-amd64"},...}}}`
when the factory holds a verified manifest, else 204 as before. The
foreman: 204 (or any 2xx without a body / without `foreman_update`) = no
advert; a malformed 200 body is logged and treated as no advert (the push
still succeeds); a failed push keeps the last advert. `path` is resolved
against the factory URL (an absolute `http(s)://` URL is used as is; a
relative path not starting with `/` is a download failure). The paired
bearer is sent on the download (the endpoint is unauthenticated today).

### 12.4 Drain and update (C5, C6)

Layout (C5), all in `<data>/bin`: `foreman` (live; systemd `ExecStart`),
`foreman.prev` (previous build), `foreman.new` (staging),
`update-pending.json` (marker `{"from","to","started_at" RFC3339,
"attempts", "rolled_back"?}`), `update-boots` (guard counter, C8),
`update-blocklist` (one build id per line).

Each caps push hands the advert to the updater, which acts only when ALL
hold: the advert's build is `Newer` than the running build; it is not
blocklisted (§12.5); the advert lists this foreman's `<GOOS>-<GOARCH>` with
a sha256 and path; the running executable (symlinks resolved) IS
`<data>/bin/foreman` and that directory is writable (probe file). A layout
failure logs ONE loud `AUTO-UPDATE DISABLED: binary not in writable layout`
line per advertised build and does nothing else (un-migrated hosts keep
working). No update is attempted while the running build is itself
unconfirmed (§12.5).

Drain (C6). Live work = spawned workers (from spawn until the harness
returns), the running brain session, and `job.request` dispatches between
their intake check and their spawn. The updater pauses intake FIRST and
then samples live work (pause and admission share one lock, so an idle
sample stays idle through the swap):

- Idle: update now (below).
- Busy: drain. While paused, a NEW `job.request` (factory or local inbox)
  is not routed and not acked (redelivered on resume or after the
  restart), and no new brain session starts (brain-delegated envelopes
  still enter the durable brain queue, §3.3, and wait there). All other
  traffic (results, events, forwards) flows normally. Live workers and
  brain sessions are never interrupted or killed; workers they spawn
  (`spawn_worker`, `spawn_child`) are admitted and counted. The drain is
  re-checked every 5s; the moment it is idle, the update runs.
- Ceiling: still busy 10 min after the drain began -> intake resumes (the
  foreman re-snapshots its factory inbox at once so deferred dispatches
  are delivered without waiting for new traffic), and that build is NOT
  drained for again: only a later 60s tick that finds the foreman idle
  updates. A newer advertised build starts its own drain. A withdrawn
  advert (204, older build, blocklisted) ends any drain.

Update (all-or-nothing; any failure before the final rename leaves
`foreman`, `foreman.prev` and the marker exactly as before, removes the
staging/temp files, resumes intake, logs `AUTO-UPDATE FAILED`, and retries
at the next caps push):

1. Download into `foreman.new` (status must be 200; byte count must equal
   Content-Length; <= 512 MiB; 5 min timeout), fsync, verify sha256 (hex,
   case-insensitive), chmod 0755.
2. Copy `foreman` to `foreman.prev.tmp` (fsync, 0755), rename to
   `foreman.prev`.
3. Delete a stale `update-boots`; write the marker
   (`from`=running, `to`=advertised, `attempts`= this process's attempt
   count for that build) via temp file + fsync + rename.
4. `rename(2)` `foreman.new` over `foreman`; fsync the directory.
5. Exit 0 (intake stays paused until exit): `cmd/foreman` shuts its
   listener and stores down gracefully and exits with the code the updater
   requested; systemd `Restart=always` starts the new binary.

### 12.5 Confirm, rollback, blocklist (C7)

On startup (before the first caps push) the foreman reads the marker:

- Absent: nothing to do.
- `to` == own build: the swap is unconfirmed. After each successful caps
  push the self-checks run - inbox store and auth store answer a query,
  and the bound listener accepts a TCP connection. Pass -> delete the
  marker AND `update-boots`, log `build <id> confirmed`. Fail -> retry at
  the next push.
- Not confirmed within 3 min of startup: copy `foreman.prev` over
  `foreman` (temp + rename, 0755), set `rolled_back: true` in the marker,
  exit 1 (systemd restarts the previous build). A copy/rename failure is
  logged and retried on the next 5s poll. If `foreman.prev` is missing or
  the binary is not in the layout, rollback is impossible: log loudly,
  delete marker + `update-boots`, keep running the new build.
- `to` != own build and `rolled_back` is true (we are the previous build
  after a rollback by the foreman or by the C8 guard): log
  `AUTO-UPDATE ROLLED BACK`, delete marker + `update-boots`, and append
  `to` to `update-blocklist`. A build is refused while it is not strictly
  `Newer` than some blocklisted (parseable) build, or equals a blocklist
  entry: only a newer build re-enables updating. The blocklist persists
  across restarts; a write failure still refuses the build in-process.
- `to` != own build without `rolled_back`: the previous process stopped
  between writing the marker and the swap rename (or an operator
  installed another build by hand). Log `AUTO-UPDATE INTERRUPTED`, delete
  marker + `update-boots`; the build stays eligible.
- Unreadable/corrupt marker: logged; a corrupt one is deleted.

C8 (install.sh, root-owned guard, `ExecStartPre=+`): counts unconfirmed
starts in `update-boots` and at the third restores `foreman.prev` and sets
`rolled_back: true` - which the restored foreman then handles as above.

### 12.6 Local-state compatibility policy

A new foreman build MUST open the existing `inbox.db`, `auth.db` and
`paired.json` written by any earlier build: schema changes are additive
and migrate in place on open; nothing is renamed, re-keyed, or requires
re-pairing. A build that cannot honor this is not eligible for
auto-update (it would fail its confirm self-checks and roll back, and the
rolled-back previous build must still open the state the new one touched -
so a new build must never rewrite state into a form the previous build
cannot read).

### 12.7 Constants

| Constant | Value |
|---|---|
| Update check | every caps push (60s, §6.7) |
| Drain re-check / confirm-deadline poll | 5s |
| Drain ceiling | 10 min |
| Confirm timeout | 3 min from startup |
| Max download / download timeout | 512 MiB / 5 min |
| Exit codes | 0 after a swap; 1 after a rollback |

---

Future tickets: reference this document; do not re-pin contracts (#16).
