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
if ! command -v opencode >/dev/null 2>&1; then
	echo "hint: opencode is not in PATH; workers need it to run jobs."
	echo "hint: install it (e.g. via your package manager or the opencode"
	echo "hint: docs) on this machine."
fi