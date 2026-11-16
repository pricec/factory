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
