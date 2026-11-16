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
| worker | `worker` | One-shot harness process that runs an opencode agent for a single job and reports the result (AGENTS.md; #5). |

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
| `job.event` | Foreman: `{"job_id": ..., "event": "accepted" or "started"}` (#9) | Factory jobs events path (#9); foreman-side routing rule: drop (#4) | #4, #9 |
| `job.question` | Worker: via `ask_parent` (#17), posted to the foreman's own inbox `foreman:{id}` (§3.2) | Foreman brain via routing, 60s SLA, default answer on timeout (#4) | #4, #11, #17 |
| `job.answer` | Foreman brain: via `answer_worker` (#18) | Worker: `ask_parent` long-polls the job inbox (#17); `answer_worker` posts to the job inbox (#18) | #17, #18 |
| `brain.wake` | Not pinned in any body | Routing -> brain (#4) | #4 |
| any other type | `post_to_factory(type, ...)` (#18), `message_foreman(..., type, ...)` (#19) | Routing -> brain ("anything unmatched -> brain") (#4) | #4, #18, #19 |

The type set is open: `post_to_factory` and `message_foreman` accept arbitrary
type strings, and unmatched types route to the brain (#4).

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

States: `pending | dispatched | running | done | failed | timed_out | dead`.
Legal transitions (#9, with retry/dead transitions from #10):

- `pending -> dispatched`
- `dispatched -> running`
- `running -> {done, failed, timed_out}`
- `failed -> dispatched` (retry, #10)
- `failed -> dead` (#10)

Anything else: log + ignore (never panic) (#9).

### 6.2 Job events & sweeper (#9)

- Foreman posts envelope type `job.event` `{job_id, event: accepted|started}`;
  terminal status comes from `job.result`.
- Sweeper (10s tick, fake-clockable): `running` past `timeout_s + 60s` ->
  `timed_out`.
- Offline foreman's jobs are left as-is unless past timeout + grace ->
  `timed_out`.

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
| factory planner | `openrouter/z-ai/glm-5.3` | `FACTORY_PLANNER_MODEL` |
| foreman brain | `openrouter/z-ai/glm-5.3` | `FACTORY_FOREMAN_BRAIN_MODEL` |
| worker, `tier: "flash"` (and no-tier jobs) | `openrouter/z-ai/glm-5.3-flash` | `FACTORY_WORKER_FLASH_MODEL` |
| worker, `tier: "default"` | `openrouter/z-ai/glm-5.3-flash#max` | `FACTORY_WORKER_MODEL` |
| worker, `tier: "hardest"` | `openrouter/z-ai/glm-5.3` | `FACTORY_WORKER_HARDEST_MODEL` |

The coordinator - factory planner and foreman brain - runs full GLM 5.3
(#32): coordination is the factory's judgment layer, and its sessions
are output-light, so the output price barely comes into play. The worker
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
Model strings are injected via the generated child config (§8.4).
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
- v1 stub matcher: first online foreman; #12 swaps the real one in and
  deletes the stub (#8, #12).
- No matching foreman: the job stays `pending`, log once (visible via
  `/v1/jobs`) (#8).
- Pending picker goroutine (10s tick, fake-clockable) (#27): every `pending`
  job is re-matched each pass, oldest first; a qualifying online foreman
  dispatches it (`pending -> dispatched` via the §6.1 transition), envelope
  delivery rides the dispatch notifier (#20). No match keeps it `pending`
  until a foreman qualifies - jobs are never orphaned by a foreman that
  arrives (or pairs) after creation.
- Refresh: `POST /v1/foreman/capabilities` (foreman token) upserts tags and
  bumps `last_seen`.

### 6.8 Schedules (#8)

- Factory-owned scheduling, single writer; no distributed locks in v1 (#8).
- `schedules` table: `{id PK, cron_spec (5-field, robfig/cron/v3),
  job_template JSON (JobSpec sans id/created_at), enabled, last_run_at}`;
  `schedule_runs`: unique `(schedule_id, run_at)` - makes ticks idempotent.
- Ticker goroutine (fake-clockable); on fire: instantiate a JobSpec (new ULID
  id, `created_at = run_at`) -> `jobs.Create` + dispatch via the Matcher.

### 6.9 Control-plane HTTP API (#8, #9, #10, #12, #13)

| Method & path | Notes |
|---|---|
| `GET /v1/jobs?state=&foreman=` | filter by state and foreman (#9) |
| `POST /v1/jobs` | UI twin of the MCP `create_job` tool (§9.4): body `{task, requires[], result_schema, tier, timeout_s, priority, schedule_cron}`; runs the same spend breaker + capability match (no match -> `pending`); `201 + {job_id, state, foreman}` |
| `POST /v1/foremen/{id}/messages` | UI twin of the MCP `message_foreman` tool (§9.4): body `{type, payload, job_id?}` posted from the planner identity; `201` + envelope. `{id}` accepts the bare foreman id or the full `foreman:{id}` form - both normalize to the same inbox (§7.3), never a double-prefixed one. `payload` is required (any JSON value; `{}` for an empty body); a null/missing payload is `400`. An id matching no registered foreman is `404 unknown_foreman` - posting never opens an inbox no foreman consumes (2026-11-14 amendment) |
| `GET /v1/jobs/{id}` | (#9) |
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
| `POST /v1/chat` | `{text}` -> chat.message envelope in the operator inbox; wakes the LLM planner (§9.5) |
| `GET /v1/inboxes/planner:main/stream` | permissive SSE stream of the operator inbox for the chat UI (§9.5) |
| `GET /` | operator web UI (single page; §4.1) |
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
  and enables + restarts the service; it is idempotent. Trust note: the
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
  existing planner token from the prior unit file.
- `GET /download/{kind}/{platform}` (#24, #25): serves the {kind} =
  `factory` | `foreman` binary from the factory's `-downloads` dir for
  `{platform}` = `{kind}-linux-amd64` | `{kind}-linux-arm64` (i.e. the full
  servable name `factory-linux-amd64`, `foreman-linux-arm64`, ... - a bare
  `linux-$GOARCH` platform is rejected with 400; 400 otherwise is also the
  traversal gate; 404 when the file is missing). The onboarding scripts
  curl exactly these names
  (`/download/{kind}/{kind}-linux-$GOARCH`). Unauthenticated per the
  trusted-LAN posture (§7.5).

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
loop calls InvalidateJob(job_id) when it routes a terminal job.result. The
factory-side OnTerminal hook still fires for jobs-bookkeeping (over-budget
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
the agent actually sees.

### 8.4 opencode invocation & generated config (#5, #13, #28, #31)

- Invocation (#28, #31): `opencode run --standalone [-m <model>] "<prompt>"`;
  must be non-interactive. `--standalone` boots a private server inside
  the child process: the shared background service would substitute the
  config's `{env:...}` credential headers against its own environment -
  which has no per-child tokens - and every MCP connect would die with
  401 "missing bearer token", silently stripping the child of its tools.
  The pinned model rides `-m`, never only the config's `model` field:
  V2 drops `#variant` suffixes (reasoning-effort levels like `#max`,
  §6.5) from the config field, so the flag is the only reliable injection
  point. Verify exact flags against `opencode --help` during
  implementation.
- Config discovery (#28): the generated config is written to the V2
  project-config path `opencode.json` IN THE CHILD'S WORKING DIRECTORY
  (worker: the per-job dir; planner/foreman brain: their WorkDir). V2
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
  only, since V2 drops `#variant` suffixes there (#31).
- opencode must be on PATH - the foreman's responsibility (#5; #12 detects
  it at startup).

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
  failure, never a silent fake success.

### 8.6 Timeout (#5)

Kill the entire process group at `timeout_s`; post a JobResult with
`error.kind = timeout`.

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
  foreman and factory surfaces < 1.5KB each (#18, #19). Names,
  descriptions, and advertised input/output schemas all count toward the
  budget (2026-11-14 amendment: typed handlers advertise input schemas so
  callers can introspect argument names).
- LLM sessions (worker agent, foreman brain, factory planner) all reuse the
  #5 opencode harness pattern (`OPENCODE_CONFIG` + generated config),
  differing in MCP URL + prompt (#5, #18, #19).
- Decision (append-only, #17): implemented with
  `github.com/modelcontextprotocol/go-sdk` v1.8.0 (streamable-HTTP server,
  stateless + JSONResponse modes, bearer auth as wrapping middleware);
  hand-roll not needed.

### 9.2 Worker surface: `/v1/mcp/worker` (#17)

`internal/mcpworker`; mounted on the foreman. Bearer = per-job worker token
(#11 scope); it resolves `job_id` and provides no other auth context.

Tools (exact; opencode exposes each as `<server>_<tool>` - with the server
named `inbox`, the agent sees `inbox_report_result` and `inbox_ask_parent`
(#28), and §8.3's prompt wording matches):

- `report_result(status, summary, result?, error?, usage?)` - validate via
  `internal/wire`; post the JobResult to the result inbox (409 dedupe
  applies); write the flag file the harness checks (#5); return
  `{"ok": true}`.
- `ask_parent(question, wait_seconds=60)` - post `job.question` to the
  foreman's inbox `foreman:{id}` (§3.2); long-poll `job:{job_id}` for
  `job.answer`; return the answer; on timeout return "proceed with your best
  judgment" (never blocks forever).

### 9.3 Foreman brain surface: `/v1/mcp/foreman` (#18)

`internal/mcpforeman`; mounted on the foreman. Bearer = foreman brain token
(#11). The brain never touches raw HTTP (#18).

Tools (exact):

- `inbox_next(wait_seconds=0)` - oldest unacked envelope.
- `inbox_ack(ids)`.
- `post_to_factory(type, payload)` - envelope to the parent inbox.
- `spawn_worker(task, result_schema?, tier?, timeout_s?, budget?)` - creates a local
  JobSpec, enqueues it to the harness, returns `job_id` immediately (never
  blocks). `tier` picks the worker model (§5.1, §6.5): flash (GLM 5.3
  Flash, cheapest, also the no-tier default), default (GLM 5.3 Flash max-reasoning,
  same low price), hardest
  (GLM 5.3) - bias towards the cheapest tier that can do the job (#29).
- `answer_worker(job_id, answer)` - posts `job.answer` to the job inbox.

Brain session mechanics: the foreman runtime spawns opencode with
`OPENCODE_CONFIG` pointing here - same code path as #5, different MCP URL +
prompt (#18). The brain token is a LOCAL foreman credential on the foreman's
own auth store (the factory-paired bearer lives in the factory's store and
cannot authenticate against the foreman's local endpoints); the foreman
mints it at startup (#21).

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
  schedule_cron?)` - ad hoc dispatch via the Matcher interface; `tier`
  selects the worker's model tier (§5.1, §6.5: flash -> GLM 5.3 Flash,
  the cheapest and the no-tier default; default -> GLM 5.3 Flash max-reasoning;
  hardest ->
  GLM 5.3) - bias towards the cheapest tier that can do the job (#29);
  with `schedule_cron`, also creates a schedule via
  the #8 API (§6.8). Refused with error `spend_cap_exceeded` when the
  daily spend breaker is open (#13).
- `read_result(job_id)` - full result payload; the ONLY path by which full
  payloads enter LLM context (#19; #7 point 3).
- `message_foreman(foreman_id, type, payload)` - envelope to that
  foreman's inbox; `foreman_id` accepts the bare id or the full
  `foreman:{id}` form (normalized - never double-prefixed); `payload`
  (any JSON value) is required; an id matching no registered foreman is
  a tool error (2026-11-14 amendment: a mis-addressed relay must fail,
  not open an inbox nothing reads).
- `notify_operator(text)` - posts a `brain.reply` envelope into the operator
  inbox (`planner:main`, §9.5); the ONLY way the LLM planner talks back to
  the human.

Input schemas (2026-11-14 amendment): every tool on this surface
advertises a typed input schema - argument names and required fields are
introspectable by MCP clients, unknown arguments are rejected, and
validation failures are tool errors. Advertised schemas count toward the
§9.1 byte budget. Rationale: a `message_foreman` call passing `text`
instead of `payload` was silently dropped and dead-ended two operator
feature requests in an inbox no consumer drains.

Planner session mechanics: same opencode harness pattern as #5 (#19).

### 9.5 Conversational surface: the operator inbox

The factory planner is conversational: the human operator drives the
factory through one inbox, `planner:main`, rendered by the web UI (§4.1
`GET /`).

Envelope types in `planner:main`:

- `chat.message` (from `human:operator`, payload `{text}`) - the operator's
  message, via `POST /v1/chat {text}` (unauthenticated like the rest of the
  UI surface, §7.5).
- `brain.reply` (from `factory:brain`, payload `{text}`) - the LLM
  planner's answer, posted by the `notify_operator` tool (§9.4).
- `notify.job` (from `factory`) - job terminal-state notification (id,
  state, foreman, last_error, over_budget, summary), posted on every
  terminal transition (done/failed/timed_out/dead) to bring completions
  and failures to the operator's attention.

Planner loop (internal/factoryapp, twin of the consumer loop): drains
`planner:main`; `chat.message` wakes one opencode planner session (§9.4
surface, planner bearer, model via `FACTORY_PLANNER_MODEL`, default
per §6.5) with the recent
inbox history replayed into the prompt (per-payload 2KB caps, §6.6);
everything else is informational and acked without a wake. A failed session
posts a `brain.reply` error note so the operator is never left waiting.
The child's MCP endpoint is the factory's loopback address, NOT the
advertised URL (#28): the child runs on the factory's own host, and the
MCP SDK's streamable-HTTP server rejects non-loopback Host headers
(DNS-rebinding protection) - the advertised hostname yields "403 invalid
Host header" and a session silently stripped of every factory tool.
Acking never deletes: history stays replayable via
`GET /v1/inboxes/planner:main/recent` and streams to browsers via the
permissive SSE `GET /v1/inboxes/planner:main/stream`.

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
| `404` | `POST /v1/foremen/{id}/messages` where `{id}` matches no registered foreman (`unknown_foreman`) | 2026-11-14 amendment |

Pinned error codes: `spend_cap_exceeded` (#13). Validation rejects anything
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
| MCP description budgets | worker < 1KB; foreman < 1.5KB; factory < 1.5KB | #17, #18, #19 |
| JobSpec defaults | `timeout_s` 300; `priority` 5 (1-9, 9 highest); `budget` `{max_prompt_bytes 32768, max_output_tokens 8192, max_cost_usd 5.0}` | #3 |

---

Future tickets: reference this document; do not re-pin contracts (#16).
