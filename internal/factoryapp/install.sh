#!/bin/bash
# factory foreman onboarding script (spec §7.1, issue #24).
# Rendered per-request by the factory with a fresh one-time pairing token
# and the factory's advertised URL. Idempotent: re-running overwrites the
# binary (now under /var/lib/factory-foreman/bin) and the unit file, and
# migrates the old /opt/factory/foreman layout in place; an existing paired.json makes the embedded
# token inert.
set -euo pipefail

FACTORY_URL="{{FACTORY_URL}}"
PAIRING_TOKEN="{{PAIRING_TOKEN}}"

if [ "$(id -u)" -ne 0 ]; then
	echo "error: run as root (sudo)" >&2
	exit 1
fi

if [ "$(uname -s)" != "Linux" ]; then
	echo "error: Linux only" >&2
	exit 1
fi

if ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; then
	echo "error: systemd not present" >&2
	exit 1
fi

FOREMAN_ID="${1:-$(hostname -s)}"
# The id rides URLs, inbox ids, and the ExecStart line; keep it boring.
if ! echo "$FOREMAN_ID" | grep -Eqx '[a-zA-Z0-9_-]+'; then
	echo "error: foreman id must match [a-zA-Z0-9_-]+ (got '$FOREMAN_ID')" >&2
	exit 1
fi

case "$(uname -m)" in
x86_64) GOARCH=amd64 ;;
aarch64 | arm64) GOARCH=arm64 ;;
*)
	echo "error: unsupported architecture $(uname -m)" >&2
	exit 1
	;;
esac

mkdir -p /opt/factory

if ! id factory-foreman >/dev/null 2>&1; then
	useradd --system --home /var/lib/factory-foreman --shell /usr/sbin/nologin factory-foreman
fi

# Install layout (auto-update v1, contract C5): the live binary is
# /var/lib/factory-foreman/bin/foreman, owned by the service user so the
# foreman can swap itself (bin/foreman.new -> bin/foreman, previous kept as
# bin/foreman.prev). Idempotent MIGRATION: a host still on the old
# /opt/factory/foreman layout is moved over by simply re-running this script;
# state (paired.json, stores, inbox) lives under /var/lib/factory-foreman and
# is never touched, so the existing pairing survives (the embedded token is
# inert once paired.json exists).
BIN_DIR=/var/lib/factory-foreman/bin
mkdir -p "$BIN_DIR"
chown factory-foreman "$BIN_DIR"
chmod 755 "$BIN_DIR"
echo "downloading foreman (linux/$GOARCH) from $FACTORY_URL..."
curl -fsSL "$FACTORY_URL/download/foreman/foreman-linux-$GOARCH" -o "$BIN_DIR/foreman.new"
if [ ! -s "$BIN_DIR/foreman.new" ]; then
	echo "error: downloaded foreman binary is empty" >&2
	rm -f "$BIN_DIR/foreman.new"
	exit 1
fi
chmod 755 "$BIN_DIR/foreman.new"
chown factory-foreman "$BIN_DIR/foreman.new"
mv -f "$BIN_DIR/foreman.new" "$BIN_DIR/foreman"
# A reinstall supersedes any in-flight auto-update: drop a stale marker and
# boot counter so the guard cannot roll back over the fresh binary.
rm -f "$BIN_DIR/update-pending.json" "$BIN_DIR/update-boots"

# Worker CLIs (spec §8.4, §7.1): spawned workers run opencode (the pinned
# GLM tiers) or the Claude Code CLI (claude-* worker models, e.g.
# FACTORY_WORKER_HARDEST_MODEL=claude-opus-4-6), so a foreman machine needs
# both. The unit pins PATH to SERVICE_PATH (below): systemd's default
# service PATH omits the service user's ~/.local/bin and ~/.opencode/bin
# (the 2026-11-15 user-unit PATH gotcha; a foreman whose unit lacked the pin
# reported no opencode and could run neither brain nor workers). Each CLI
# is installed as the service user (its self-updater keeps working) and
# symlinked into /usr/local/bin. Idempotent: a service-visible install is detected and
# kept. Every step verifies with command -v against the service PATH - the
# same lookup the harness's exec performs at run time - and a failed or
# unverifiable install aborts the onboarding loudly before the unit file is
# written (re-run: idempotent).
SERVICE_USER=factory-foreman
SERVICE_HOME=/var/lib/factory-foreman
# The pinned unit PATH (rendered into the unit below as Environment=PATH and
# used for every verification here): the service user's own bins first, so a
# CLI installed under its home resolves without a symlink.
SERVICE_PATH=/var/lib/factory-foreman/.opencode/bin:/var/lib/factory-foreman/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mkdir -p "$SERVICE_HOME"
chown "$SERVICE_USER" "$SERVICE_HOME"
if ! command -v su >/dev/null 2>&1; then
	echo "error: su not found; cannot install the worker CLIs as $SERVICE_USER" >&2
	exit 1
fi
install_worker_cli() {
	local cli="$1" rel="$2" url="$3"
	if PATH="$SERVICE_PATH" command -v "$cli" >/dev/null 2>&1; then
		echo "$cli already on the service PATH: $(PATH="$SERVICE_PATH" command -v "$cli")"
		return 0
	fi
	echo "installing $cli (as $SERVICE_USER) from $url ..."
	if ! su -s /bin/bash -c "export HOME='$SERVICE_HOME'; curl -fsSL '$url' | bash" "$SERVICE_USER"; then
		echo "error: $cli installation failed; without it this foreman's" >&2
		echo "error: workers cannot run every routed model (spec §8.4)." >&2
		echo "error: fix the failure above and re-run this script (idempotent)." >&2
		exit 1
	fi
	ln -sfn "$SERVICE_HOME/$rel" "/usr/local/bin/$cli"
	if ! PATH="$SERVICE_PATH" command -v "$cli" >/dev/null 2>&1; then
		echo "error: $cli was installed but is not visible on the service" >&2
		echo "error: PATH; refusing to onboard a foreman whose workers" >&2
		echo "error: cannot exec it (spec §8.4)." >&2
		exit 1
	fi
	echo "$cli installed: $(PATH="$SERVICE_PATH" command -v "$cli")"
}
install_worker_cli opencode .opencode/bin/opencode https://opencode.ai/install
install_worker_cli claude .local/bin/claude https://claude.ai/install.sh

# Crash-loop guard (contract C8): root-owned, run by systemd before every
# start via ExecStartPre=+. POSIX sh, no jq/python. Always exits 0 - it must
# never itself block the start.
cat >/opt/factory/foreman-update-guard.sh <<'GUARD_EOF'
#!/bin/sh
# factory foreman auto-update crash-loop guard. Usage: guard.sh <data-dir>
data="${1:-}"
log() { echo "foreman-update-guard: $*" >&2; }
[ -n "$data" ] || { log "no data dir given; nothing to do"; exit 0; }
bin="$data/bin"
marker="$bin/update-pending.json"
boots="$bin/update-boots"
if [ ! -f "$marker" ]; then
	rm -f "$boots"
	exit 0
fi
owner="$(ls -ldn "$bin" 2>/dev/null | awk '{print $3":"$4}')"
n=0
if [ -f "$boots" ]; then
	read -r n <"$boots" 2>/dev/null || : # last line may lack a newline
fi
case "$n" in '' | *[!0-9]*) n=0 ;; esac
n=$((n + 1))
if [ "$n" -lt 3 ]; then
	echo "$n" >"$boots" 2>/dev/null || true
	[ -z "$owner" ] || chown "$owner" "$boots" 2>/dev/null || true
	log "update pending, unconfirmed start $n of 3"
	exit 0
fi
if [ ! -f "$bin/foreman.prev" ]; then
	log "update unconfirmed after $n starts but $bin/foreman.prev is missing; cannot roll back"
	echo "$n" >"$boots" 2>/dev/null || true
	[ -z "$owner" ] || chown "$owner" "$boots" 2>/dev/null || true
	exit 0
fi
if grep -Eq '"rolled_back"[[:space:]]*:[[:space:]]*true' "$marker" 2>/dev/null; then
	log "update already rolled back; leaving $bin/foreman as is"
	rm -f "$boots"
	exit 0
fi
log "update unconfirmed after $n starts: restoring foreman.prev over foreman"
if cp -p "$bin/foreman.prev" "$bin/foreman.guard.tmp" 2>/dev/null &&
	{ [ -z "$owner" ] || chown "$owner" "$bin/foreman.guard.tmp"; } &&
	chmod 755 "$bin/foreman.guard.tmp" &&
	mv -f "$bin/foreman.guard.tmp" "$bin/foreman"; then
	body="$(cat "$marker")"
	rest="${body%\}}"
	case "$rest" in
	*[!{[:space:]]*) printf '%s,"rolled_back":true}\n' "$rest" >"$marker" ;;
	*) printf '{"rolled_back":true}\n' >"$marker" ;;
	esac
	rm -f "$boots"
	log "rolled back to foreman.prev"
else
	rm -f "$bin/foreman.guard.tmp"
	log "rollback copy FAILED; starting the current binary"
fi
exit 0
GUARD_EOF
chown root:root /opt/factory /opt/factory/foreman-update-guard.sh
chmod 755 /opt/factory /opt/factory/foreman-update-guard.sh

cat >/etc/systemd/system/factory-foreman.service <<'EOF'
[Unit]
Description=factory foreman agent
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=600
StartLimitBurst=10

[Service]
Type=simple
User=factory-foreman
StateDirectory=factory-foreman
Environment=PATH={{SERVICE_PATH}}
ExecStartPre=+/opt/factory/foreman-update-guard.sh /var/lib/factory-foreman
ExecStart=/var/lib/factory-foreman/bin/foreman -factory {{FACTORY_URL}} -id {{FOREMAN_ID}} -data /var/lib/factory-foreman -addr 127.0.0.1:8090
Environment=FACTORY_PAIRING_TOKEN={{PAIRING_TOKEN}}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
# {{FACTORY_URL}} and {{PAIRING_TOKEN}} were rendered server-side above;
# the foreman id is local, so substitute it here (single-line string).
sed -i "s|{{FOREMAN_ID}}|$FOREMAN_ID|" /etc/systemd/system/factory-foreman.service
# The pinned PATH (see SERVICE_PATH above): no ~/.opencode/bin or ~/.local/bin
# would otherwise be on the service PATH (2026-11-16 foreman tools-cap gap).
sed -i "s|{{SERVICE_PATH}}|$SERVICE_PATH|" /etc/systemd/system/factory-foreman.service
# The unit carries the live pairing token until first boot pairs; keep it
# root-only.
chmod 600 /etc/systemd/system/factory-foreman.service

# Claude model routing (spec §8.4). Without these a new foreman defaults to
# the opencode GLM tiers. They live in an installer-owned DROP-IN, not the
# main unit, so: (1) re-running refreshes them without touching the pairing
# token, paired.json or any other env; (2) values an operator set by hand
# win - systemd applies drop-ins in lexical order and the later assignment
# wins, so 05-* is overridden by an operator's override.conf / 10-*.conf
# (and by the unit's own Environment= lines). No secrets are written here.
DROPIN_DIR=/etc/systemd/system/factory-foreman.service.d
mkdir -p "$DROPIN_DIR"
cat >"$DROPIN_DIR/05-claude-models.conf" <<'MODELS_EOF'
# Written by the factory install script (re-run refreshes it). To override,
# set the same variables in override.conf - it sorts after this file.
[Service]
Environment=FACTORY_WORKER_FLASH_MODEL=claude-haiku-5-5
Environment=FACTORY_WORKER_MODEL=claude-sonnet-5-5
Environment=FACTORY_WORKER_HARDEST_MODEL=claude-opus-5-5
Environment=FACTORY_FOREMAN_BRAIN_MODEL=claude-opus-5-5
MODELS_EOF
chmod 644 "$DROPIN_DIR/05-claude-models.conf"

systemctl daemon-reload
systemctl enable factory-foreman >/dev/null
systemctl restart factory-foreman
# Migration cleanup: the old layout's binary is no longer referenced.
rm -f /opt/factory/foreman

echo "foreman onboarded as '$FOREMAN_ID'."
systemctl --no-pager --lines=5 status factory-foreman || true
echo "note: the embedded pairing token expires 15 minutes after it was"
echo "issued; if pairing failed, re-fetch the install script for a fresh one."
echo "note: worker CLIs are visible on the service PATH (spec §8.4):"
echo "note:   opencode: $(PATH="$SERVICE_PATH" command -v opencode || echo MISSING)"
echo "note:   claude:   $(PATH="$SERVICE_PATH" command -v claude || echo MISSING)"
echo "note: workers and the brain are routed to Claude (see"
echo "note: $DROPIN_DIR/05-claude-models.conf). The claude CLI is NOT"
echo "note: logged in yet. NEXT STEP - authenticate once, as the service user:"
echo "note:"
echo "note:   sudo -u $SERVICE_USER -H claude /login"
echo "note:"
echo "note: (or set ANTHROPIC_API_KEY in a unit drop-in). Until then claude"
echo "note: jobs fail loudly (spec §8.4); the foreman logs a warning at start."
