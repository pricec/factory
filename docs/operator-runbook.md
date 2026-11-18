# Operator runbook

## One-time per-host migration to the auto-update layout

Before auto-update v1, foremen ran `/opt/factory/foreman`. Auto-update needs the
binary in a service-writable location, `/var/lib/factory-foreman/bin/foreman`.
Hosts that have not been migrated keep working but never self-update (the
foreman logs "auto-update disabled: binary not in writable layout").

Re-run the installer once on each foreman host (it is idempotent):

    curl -fsSL -H 'Authorization: Bearer $PLANNER_TOKEN' $FACTORY_URL/install.sh \
      | sudo bash -s -- <foreman-id>

Use the host's existing foreman id. What happens:

- the binary is downloaded to `/var/lib/factory-foreman/bin/foreman` (owned by
  `factory-foreman`);
- `/opt/factory/foreman-update-guard.sh` (root-owned crash-loop guard) is
  written and the unit is rewritten with the new `ExecStart`,
  `StartLimitIntervalSec=600`, `StartLimitBurst=10` and the guard as
  `ExecStartPre=+`;
- the service is restarted and the old `/opt/factory/foreman` is deleted.

State (`/var/lib/factory-foreman`, including `paired.json`) is untouched: no
re-pairing is needed and the freshly embedded pairing token is inert. The
factory binary stays at `/usr/local/bin/factory` (foremen-only in v1).

Verify: `systemctl cat factory-foreman | grep -E 'ExecStart|StartLimit'` and
`ls -l /var/lib/factory-foreman/bin/`.

## Crash-loop guard

If an auto-updated foreman fails to confirm, the guard restores
`bin/foreman.prev` on the third unconfirmed start and marks
`bin/update-pending.json` with `"rolled_back":true`. Inspect with
`journalctl -u factory-foreman | grep foreman-update-guard`.

## Deploying master

This is the one deploy procedure for the factory host. Merge jobs run it as
step 7 of [docs/merging.md](merging.md), and a human runs it the same way.
It needs `sudo`, Go on `PATH`, and a clean clone that is **not** the
operator checkout `/home/price/temp/factory`. Facts about the live host:
the `factory` systemd unit runs `/usr/local/bin/factory -addr :8090 -data
/var/lib/factory -downloads /var/lib/factory/downloads`; the stores are
`jobs.db`, `inbox.db` and `auth.db` under `/var/lib/factory`.

Inputs: `REF` is the commit to deploy (normally `origin/master`). `EV` is a
fresh evidence directory, for example
`/var/lib/factory-foreman/evidence/merge-<mr_id>`.

1. **Build.** Run this in the clean clone:

       git fetch origin && git checkout --detach "$REF"
       DEPLOYED=$(git rev-parse HEAD)
       make release
       BUILD=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["build"])' dist/manifest.json)
       sha256sum dist/factory-linux-amd64 dist/factory-linux-arm64 dist/foreman-linux-amd64 dist/foreman-linux-arm64 | tee "$EV/dist.sha256"

   `BUILD` must end in the first 12 characters of `$DEPLOYED`.

2. **Back up** the stores, the installed binary and the downloads. The
   python sqlite3 backup API is safe against the live WAL database. Never
   copy the `.db` files by hand.

       for db in jobs inbox; do
         python3 -I -c 'import sqlite3,sys; s=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro", uri=True); d=sqlite3.connect(sys.argv[2]); s.backup(d); d.close(); s.close()' \
           /var/lib/factory/$db.db "$EV/$db-pre.db"
       done
       cp /usr/local/bin/factory "$EV/factory-pre"
       cp -a /var/lib/factory/downloads "$EV/downloads-pre"

3. **Install the factory binary:**

       sudo install -m755 dist/factory-linux-amd64 /usr/local/bin/factory

4. **Publish the four binaries. Do NOT publish the manifest yet.**

       sudo install -m755 dist/factory-linux-amd64 dist/factory-linux-arm64 \
         dist/foreman-linux-amd64 dist/foreman-linux-arm64 /var/lib/factory/downloads/

5. **Restart:**

       SINCE=$(date -u '+%Y-%m-%d %H:%M:%S UTC')
       sudo systemctl restart factory && sleep 3

6. **Verify** all of these:

       systemctl is-active factory                                            # active
       PID=$(systemctl show -p MainPID --value factory)
       test "$(sudo sha256sum /proc/$PID/exe | cut -d' ' -f1)" = "$(sha256sum dist/factory-linux-amd64 | cut -d' ' -f1)"
       curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8090/      # 200
       curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8090/v1/merge-requests  # 200
       sudo journalctl -u factory --since "$SINCE" --no-pager | tee "$EV/journal.txt" \
         | grep -Ei 'panic|fatal|stopped:|migrat.*(fail|error)'                 # no output

   The journal is clean when the grep prints nothing. One line is expected
   and does not count: the advert/manifest build mismatch logged before
   step 7. If any check fails, roll back:

       sudo install -m755 "$EV/factory-pre" /usr/local/bin/factory
       sudo cp -a "$EV/downloads-pre/." /var/lib/factory/downloads/
       sudo systemctl restart factory

   Then re-verify UI 200, and do NOT do step 7. Restore the database
   backups only when a migration damaged them, and only with the operator.

7. **Copy the manifest LAST.** Foremen auto-update only after the factory is
   healthy:

       sudo install -m644 dist/manifest.json /var/lib/factory/downloads/manifest.json

8. **Confirm the version:**

       go version -m /usr/local/bin/factory | grep -o 'buildinfo.ID=[^"]*'    # == $BUILD

   The foremen pick up `$BUILD` through the auto-update advert (spec §12).
   The foreman that runs the deploying job drains and swaps only after that
   job exits. Rollback for foremen is the updater's `.prev` (see Crash-loop
   guard).

9. **Record** the following in `$EV`: `DEPLOYED`, `BUILD`, the
   `/proc/$PID/exe` sha, and the check results. A merge job puts these in
   its `report_result`.

## Held jobs and the conflict coordinator

A job is `held` for one of three reasons, told apart by the prefix of its
`held_reason` (list_jobs, the Jobs tab, the held panel):

- `usage ...`: the usage throttle (spec §6.10); releases when usage allows.
- `claude auth: ...`: the foreman's Claude CLI is logged out (see below).
- `conflict: waits for job_X (glob)`: the job declared `touches` (file
  globs) overlapping job_X's, and job_X is still active or its merge
  request is `queued`/`merging` (spec §6.14). It releases by itself once
  job_X's merge lands, is cancelled or needs attention, or job_X ends
  without a merge. The usage panel shows `held N (usage a, conflict b)`.

Things to know:

- A job with no `touches` is never held for conflicts, and `touches: ["*"]`
  is exclusive (it waits for, and blocks, every job that declares touches).
  Docs-only overlap (`docs/**`, `*.md`) never holds.
- Max wait is 60 minutes; then the job is released anyway (the task thread
  gets a `notify.conflict` `timed_out` note, the log says `conflict:`).
  Tune with `FACTORY_CONFLICT_MAX_WAIT=30m`.
- A conflict hold that will not clear usually means the blocker's merge is
  stuck: look at `GET /v1/merge-requests` (a paused queue, or an `after`
  dependency nobody queued). Landing, cancelling or resolving that request
  releases the waiter on the next pass (every 15 s).
- To unstick a job now: `cancel_job` it and re-create it without `touches`,
  or set `FACTORY_CONFLICT=off` and restart the factory to disable the
  coordinator entirely (holds clear on the next releaser pass).
- Log lines: `factoryapp: conflict: waiting|released|timed_out: ...`.

## Retries and superseded jobs

A failed worker result is retried per spec §6.4 (validation 0, timeout 1,
crash/llm/other 2; backoff 30s, 2m, 8m), except budget/loop kills and merge
jobs, which dead-letter immediately.

A worker that finds its job is a duplicate of, or superseded by, another job
reports `status=failed` with `error.kind=superseded`. The factory moves the
job to the terminal `superseded` state (the same state
`cancel_job(supersede=true)` produces) and never retries it. Jobs already
`cancelled`/`superseded`/`done`/`dead` are never re-dispatched by a late
result or retry timer. Deterministic failures (no permission, bad auth, bad
config) should be reported as `error.kind=validation` so they are not retried.
If a job was retried wrongly before this fix, look for
`jobs: retrying <id>` in the factory log.

## Credentials

Secrets are named values with an env-var mapping, managed in the operator UI
Credentials panel or over HTTP, and stored plaintext (mode 0600) in
`secrets.json` in the factory data dir (spec §13).

    curl -X POST $FACTORY/v1/secrets/github-push \
      -d '{"value":"ghp_...","env_var":"GITHUB_TOKEN"}'

A job opts in by name: `create_job` with `secrets: ["github-push"]`, or the
`secrets` field of the UI job form (comma-separated). Names must exist at
creation or the create fails. The worker child then runs with
`GITHUB_TOKEN=<value>` in its environment (default env var when `env_var` is
omitted: `SECRET_GITHUB_PUSH`); the value is never in `job.json`, logs or
envelopes, and output echoing it is replaced by `[REDACTED]`.

- **Rotation:** overwrite with another `POST /v1/secrets/{name}`. Jobs pick
  it up at dispatch (the foreman fetches just before starting the child);
  already-running jobs keep the old value.
- **Audit:** `GET /v1/jobs/{id}` lists `granted_secrets` (names); the
  `secret_grants` table in `jobs.db` has timestamps.
- **Deleting** a secret a queued job still names fails that job at start
  with `could not fetch secrets [name]`; recreate the secret and requeue.

## Claude CLI login ("Not logged in · Please run /login")

No SSH needed (spec §13.3). When a foreman's Claude CLI loses its login,
the factory holds that foreman's claude jobs (no retries burned) and opens
a task **Claude login needed: foreman:X** in Needs you, with a sign-in link.

1. Open the link, sign in with the Claude account the factory should use.
2. Paste the code the page shows as a reply in that task (or the alert).
   Alternatively run `claude setup-token` anywhere and paste the
   `sk-ant-oat01-...` token - in that task or any message box.

The factory verifies the token, stores it as the `claude-oauth-token`
secret (`CLAUDE_CODE_OAUTH_TOKEN`), and every foreman adopts it within
about a minute, re-checks, and reports in the task; held jobs then run and
the task closes. Your reply is never stored in the thread. Reply `new link`
if the link has expired. The token lasts a year; rotate early by pasting a
new one. Status: `curl $FACTORY/v1/claude-auth`; force a re-check:
`curl -X POST $FACTORY/v1/claude-auth/check`. Deleting the secret returns
every host to its own CLI login.

## Factory auditor

The daily audit (spec §6.12) is configured by the factory service's
environment (systemd unit); changes apply on the next factory restart,
which re-creates the audit schedule:

| Env | Default | Meaning |
|---|---|---|
| `FACTORY_AUDIT_ENABLED` | `1` | `0` removes the schedule |
| `FACTORY_AUDIT_AUTOFIX` | `1` | `0` files every finding as a proposal |
| `FACTORY_AUDIT_CRON` / `FACTORY_AUDIT_TZ` | `0 6 * * *` / host zone | when it runs |
| `FACTORY_AUDIT_BUDGET_USD` | `10` | per-run `cost_usd` meter (audit + fix jobs) |
| `FACTORY_AUDIT_FIX_RESERVE_USD` | `1.5` | what an unfinished fix job counts against the meter |
| `FACTORY_AUDIT_MAX_AUTOFIXES` | `5` | per-run cap; can only be lowered |
| `FACTORY_AUDIT_MAX_CHANGED_LINES` | `150` | per-PR cap; can only be lowered |
| `FACTORY_AUDIT_FOREMAN` | `foreman:server` | where audit and fix jobs run |
| `FACTORY_AUDIT_REPO_DIR` | `/home/price/temp/factory` | clone on the audit foreman whose worktrees the AUDIT JOB scans read-only (empty disables) |
| `FACTORY_AUDIT_WORKTREE_ROOT` | `/home/price/work` | only merged, clean worktrees under it may be cleaned (never forced) |

The factory service runs no git and needs no ssh key or deploy key for the
auditor: the worktree scan runs in the audit job, and fix branches are
checked in CI.

**Required check (one-time, after this lands on master).** Auto-fix PRs
(`auto/audit-*`) are gated by the `audit-guardrail` workflow
(`.github/workflows/audit-guardrail.yml`, run from master's copy via
`pull_request_target`). The merge procedure already waits for every check
to be green; to make GitHub enforce it too, add it as a required status
check on master. Do this only once the workflow is on master: until then
the check never reports and would block every PR. In the UI: Settings ->
Branches (or Rules -> Rulesets) -> master -> Require status checks to pass
-> add `audit-guardrail`. With `gh` (classic branch protection; master
had none on 2026-11-17 - if you add other rules later, keep this check):

    gh api -X PUT repos/pricec/factory/branches/master/protection \
      -H 'Accept: application/vnd.github+json' --input - <<'JSON'
    {"required_status_checks":{"strict":false,"contexts":["audit-guardrail"]},
     "enforce_admins":false,"required_pull_request_reviews":null,"restrictions":null}
    JSON

Check it: `gh api repos/pricec/factory/branches/master/protection/required_status_checks`.

Preview a run without side effects:
`factory audit -data /var/lib/factory -dry-run` (no git: the worktree scan
is reported as skipped; `-worktrees FILE` previews cleanup decisions for a
scan in the audit job's result format).

Raising the hard caps (5 auto-fixes, 150 lines) or editing the denylist is
a code change in `internal/auditor`, which auto-fixes can never touch.

## Factory-repo work and cancelling jobs

- Work on the factory repo (`pricec/factory`, `/home/price/work/factory*`)
  runs on `foreman:server` only. Planner `create_job` calls must pin
  `foreman: "foreman:server"`; an unpinned job that mentions the repo or its
  worktree paths is rejected with `factory_repo_needs_pin`. Do not pin it to
  another foreman (a warning is returned). 2026-11-17: an unpinned job went
  to `foreman:chatfrontend`, and its re-file went to `foreman:agentcloud`.
- Cancelling or superseding a `dispatched`/`running` job posts a `job.cancel`
  to its foreman, which SIGTERMs the worker's process group and SIGKILLs it
  after 10s; no result is posted. Check `journalctl` for
  `job.cancel <id>: stopping worker`. An offline foreman gets the cancel on
  reconnect (durable inbox). If a worker still lingers, `kill -TERM -<pgid>`
  by hand is the fallback.
