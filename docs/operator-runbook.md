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
