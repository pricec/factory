#!/bin/bash
# factory server bootstrap script (spec §7.1, issue #25).
# Rendered per-request by a running factory with its advertised URL as the
# download source. Idempotent: re-running overwrites the binary and the unit
# file; a reinstall preserves the existing planner token.
set -euo pipefail

FACTORY_URL="{{FACTORY_URL}}"

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

PORT="${1:-8080}"
# The port rides the ExecStart -addr line; keep it digits only.
if ! echo "$PORT" | grep -Eqx '[0-9]+'; then
	echo "error: listen port must be digits only (got '$PORT')" >&2
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

# Idempotent reinstall: preserve the existing planner token (hex64) from a
# prior unit file rather than rotating it out from under the operator.
PLANNER_TOKEN=""
if [ -f /etc/systemd/system/factory.service ]; then
	# Portable sed (no grep -P): pull the token out of the Environment line.
	PLANNER_TOKEN=$(sed -n 's/^Environment=FACTORY_PLANNER_TOKEN=\([a-f0-9]\{64\}\)$/\1/p' /etc/systemd/system/factory.service 2>/dev/null || true)
fi
if [ -z "$PLANNER_TOKEN" ]; then
	if command -v openssl >/dev/null 2>&1; then
		PLANNER_TOKEN=$(openssl rand -hex 32)
	elif command -v xxd >/dev/null 2>&1; then
		PLANNER_TOKEN=$(head -c 32 /dev/urandom | xxd -p -c 64)
	else
		echo "error: need openssl or xxd to generate the planner token" >&2
		exit 1
	fi
fi

mkdir -p /opt/factory
echo "downloading factory (linux/$GOARCH) from $FACTORY_URL..."
curl -fsSL "$FACTORY_URL/download/factory/factory-linux-$GOARCH" -o /opt/factory/factory
chmod 755 /opt/factory/factory

if ! id factory >/dev/null 2>&1; then
	useradd --system --home /var/lib/factory --shell /usr/sbin/nologin factory
fi

# Worker CLIs (spec §8.4, §7.1), same contract as install.sh: a factory
# machine runs the planner brain via opencode (spec §8.4), and a foreman
# onboarded here needs opencode (the pinned GLM tiers) plus the Claude Code
# CLI (claude-* worker models) for its spawned workers. systemd runs the
# unit with the DEFAULT service PATH below - the service user's
# ~/.local/bin and ~/.opencode/bin are NOT on it (the 2026-11-15 user-unit
# PATH gotcha) - so each CLI is installed as the service user (its
# self-updater keeps working) and symlinked into /usr/local/bin.
# Idempotent: a service-visible install is detected and kept. Every step
# verifies with command -v against the service PATH - the same lookup the
# harness's exec performs at run time - and a failed or unverifiable
# install aborts the bootstrap loudly before the unit file is written
# (re-run: idempotent).
SERVICE_USER=factory
SERVICE_HOME=/var/lib/factory
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
		echo "error: $cli installation failed; without it this machine" >&2
		echo "error: cannot run every routed model (spec §8.4)." >&2
		echo "error: fix the failure above and re-run this script (idempotent)." >&2
		exit 1
	fi
	ln -sfn "$SERVICE_HOME/$rel" "/usr/local/bin/$cli"
	if ! PATH="$SERVICE_PATH" command -v "$cli" >/dev/null 2>&1; then
		echo "error: $cli was installed but is not visible on the service" >&2
		echo "error: PATH; refusing to bootstrap a factory whose agents" >&2
		echo "error: cannot exec it (spec §8.4)." >&2
		exit 1
	fi
	echo "$cli installed: $(PATH="$SERVICE_PATH" command -v "$cli")"
}
install_worker_cli opencode .opencode/bin/opencode https://opencode.ai/install
install_worker_cli claude .local/bin/claude https://claude.ai/install.sh

# Pre-seed the foreman binary for this arch so this factory can onboard
# workers immediately; StateDirectory recreates /var/lib/factory at boot,
# but the pre-seed happens before first boot. Non-fatal on failure.
mkdir -p /var/lib/factory/downloads
curl -fsSL "$FACTORY_URL/download/foreman/foreman-linux-$GOARCH" \
	-o /var/lib/factory/downloads/foreman-linux-$GOARCH ||
	echo "warn: could not pre-seed foreman binary foreman-linux-$GOARCH (non-fatal; re-run the install script or fetch it manually later)"

# /etc/factory/factory.env is the FACTORY_* override point for this unit;
# create it empty so systemd does not fail on a missing EnvironmentFile.
mkdir -p /etc/factory
[ -f /etc/factory/factory.env ] || touch /etc/factory/factory.env

cat >/etc/systemd/system/factory.service <<'EOF'
[Unit]
Description=factory controller
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=factory
StateDirectory=factory
ExecStart=/opt/factory/factory -data /var/lib/factory -addr :{{PORT}} -downloads /var/lib/factory/downloads
Environment=FACTORY_PLANNER_TOKEN={{PLANNER_TOKEN}}
EnvironmentFile=/etc/factory/factory.env
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
# {{PORT}} and {{PLANNER_TOKEN}} are local values (digits/hex) - sed-safe.
sed -i "s|{{PORT}}|$PORT|" /etc/systemd/system/factory.service
sed -i "s|{{PLANNER_TOKEN}}|$PLANNER_TOKEN|" /etc/systemd/system/factory.service
# The unit carries the planner token (the v1 operator credential); keep it
# root-only.
chmod 600 /etc/systemd/system/factory.service

systemctl daemon-reload
systemctl enable factory >/dev/null
systemctl restart factory

echo "factory installed and running on port $PORT."
echo "planner token (also in /etc/systemd/system/factory.service, root-readable):"
echo "  $PLANNER_TOKEN"
echo "logs: journalctl -u factory -f"
echo "onboard a foreman on another machine:"
echo "  curl -fsSL -H 'Authorization: Bearer $PLANNER_TOKEN' $FACTORY_URL/install.sh | sudo bash -s -- <foreman-id>"