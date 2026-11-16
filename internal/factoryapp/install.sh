#!/bin/bash
# factory foreman onboarding script (spec §7.1, issue #24).
# Rendered per-request by the factory with a fresh one-time pairing token
# and the factory's advertised URL. Idempotent: re-running overwrites the
# binary and the unit file; an existing paired.json makes the embedded
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
echo "downloading foreman (linux/$GOARCH) from $FACTORY_URL..."
curl -fsSL "$FACTORY_URL/download/foreman/foreman-linux-$GOARCH" -o /opt/factory/foreman
chmod 755 /opt/factory/foreman

if ! id factory-foreman >/dev/null 2>&1; then
	useradd --system --home /var/lib/factory-foreman --shell /usr/sbin/nologin factory-foreman
fi

# Worker CLIs (spec §8.4, §7.1): spawned workers run opencode (the pinned
# GLM tiers) or the Claude Code CLI (claude-* worker models, e.g.
# FACTORY_WORKER_HARDEST_MODEL=claude-opus-4-6), so a foreman machine needs
# both. systemd runs the unit with the DEFAULT service PATH below - the
# service user's ~/.local/bin and ~/.opencode/bin are NOT on it (the
# 2026-11-15 user-unit PATH gotcha) - so each CLI is installed as the
# service user (its self-updater keeps working) and symlinked into
# /usr/local/bin. Idempotent: a service-visible install is detected and
# kept. Every step verifies with command -v against the service PATH - the
# same lookup the harness's exec performs at run time - and a failed or
# unverifiable install aborts the onboarding loudly before the unit file is
# written (re-run: idempotent).
SERVICE_USER=factory-foreman
SERVICE_HOME=/var/lib/factory-foreman
SERVICE_PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
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

cat >/etc/systemd/system/factory-foreman.service <<'EOF'
[Unit]
Description=factory foreman agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=factory-foreman
StateDirectory=factory-foreman
ExecStart=/opt/factory/foreman -factory {{FACTORY_URL}} -id {{FOREMAN_ID}} -data /var/lib/factory-foreman -addr 127.0.0.1:8090
Environment=FACTORY_PAIRING_TOKEN={{PAIRING_TOKEN}}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
# {{FACTORY_URL}} and {{PAIRING_TOKEN}} were rendered server-side above;
# the foreman id is local, so substitute it here (single-line string).
sed -i "s|{{FOREMAN_ID}}|$FOREMAN_ID|" /etc/systemd/system/factory-foreman.service
# The unit carries the live pairing token until first boot pairs; keep it
# root-only.
chmod 600 /etc/systemd/system/factory-foreman.service

systemctl daemon-reload
systemctl enable factory-foreman >/dev/null
systemctl restart factory-foreman

echo "foreman onboarded as '$FOREMAN_ID'."
systemctl --no-pager --lines=5 status factory-foreman || true
echo "note: the embedded pairing token expires 15 minutes after it was"
echo "issued; if pairing failed, re-fetch the install script for a fresh one."
echo "note: worker CLIs are visible on the service PATH (spec §8.4):"
echo "note:   opencode: $(PATH="$SERVICE_PATH" command -v opencode || echo MISSING)"
echo "note:   claude:   $(PATH="$SERVICE_PATH" command -v claude || echo MISSING)"
echo "note: claude auth is the service user's (login once as $SERVICE_USER"
echo "note: or set ANTHROPIC_API_KEY in the unit's environment); an"
echo "note: unauthenticated claude run fails the job loudly (spec §8.4)."