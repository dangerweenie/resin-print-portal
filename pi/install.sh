#!/bin/bash
# Install (or update) the resin print portal pi-agent on a Raspberry Pi.
# This is the ONE install path — the manual/scp flow, the fresh-SD-card
# provisioning flow (provisioning/provision-boot.sh calls this), and
# provisioning/test-provision-sd.sh all go through it.
#
# Run FROM the repo root on your dev machine:
#   make pi-agent
#   scp bin/pi-agent-armv6 usb-refresh.sh pi/agent-guard.sh pi/resin-pi-agent.service \
#       pi/install.sh pi/config.example.env captain@<pi>.lan:~/agent-install/
#   ssh captain@<pi>.lan 'sudo bash ~/agent-install/install.sh ~/agent-install'
#
# It:
#   - installs pi-agent          -> /usr/local/bin/pi-agent
#   - installs usb-refresh.sh     -> /usr/local/bin/usb-refresh.sh
#   - installs agent-guard.sh     -> /usr/local/bin/agent-guard.sh (self-update crash-loop guard)
#   - installs the systemd unit   -> /etc/systemd/system/resin-pi-agent.service
#   - seeds /etc/resin-pi-agent.env if missing
#   - retires the old Flask printer-upload.service / /opt/printer-upload
#   - enables + starts resin-pi-agent
#
# Test / packaging hooks (used by test-provision-sd.sh, no effect in normal use):
#   PI_INSTALL_ROOT=<dir>       prefix every install path with <dir> (like DESTDIR)
#   PI_INSTALL_SKIP_SYSTEMD=1   skip the root check and every systemctl call
set -euo pipefail

SRC=${1:-$(dirname "$0")}
BIN=$SRC/pi-agent-armv6
[ -f "$BIN" ] || BIN=$SRC/pi-agent   # accept either name
if [ ! -f "$BIN" ]; then
    echo "ERROR: no pi-agent binary in $SRC (expected pi-agent-armv6 or pi-agent) — run 'make pi-agent'." >&2
    exit 1
fi

ROOT=${PI_INSTALL_ROOT:-}
SKIP_SYSTEMD=${PI_INSTALL_SKIP_SYSTEMD:-}

if [ -z "$SKIP_SYSTEMD" ] && [ "$(id -u)" != "0" ]; then
    echo "run with sudo (or set PI_INSTALL_SKIP_SYSTEMD=1 for a dry run)" >&2
    exit 1
fi

sysctl_() { [ -n "$SKIP_SYSTEMD" ] || systemctl "$@"; }

echo "==> installing pi-agent"
install -D -m 0755 "$BIN"                         "$ROOT/usr/local/bin/pi-agent"
install -D -m 0755 "$SRC/usb-refresh.sh"          "$ROOT/usr/local/bin/usb-refresh.sh"
install -D -m 0755 "$SRC/agent-guard.sh"          "$ROOT/usr/local/bin/agent-guard.sh"
install -D -m 0644 "$SRC/resin-pi-agent.service"  "$ROOT/etc/systemd/system/resin-pi-agent.service"

if [ ! -f "$ROOT/etc/resin-pi-agent.env" ]; then
    # A provisioned card stages a filled resin-pi-agent.env (CENTRAL_BASE_URL +
    # ENROLL_TOKEN already set); a bare scp deploy only has the example.
    SEED="$SRC/resin-pi-agent.env"
    [ -f "$SEED" ] || SEED="$SRC/config.example.env"
    echo "==> seeding /etc/resin-pi-agent.env from $(basename "$SEED")"
    install -D -m 0600 "$SEED" "$ROOT/etc/resin-pi-agent.env"
fi

echo "==> retiring the old Flask printer-upload service (if present)"
if [ -z "$SKIP_SYSTEMD" ] && systemctl list-unit-files 2>/dev/null | grep -q '^printer-upload\.service'; then
    systemctl disable --now printer-upload.service || true
fi
rm -f  "$ROOT/etc/systemd/system/printer-upload.service"
rm -rf "$ROOT/opt/printer-upload"

echo "==> starting resin-pi-agent"
sysctl_ daemon-reload
sysctl_ enable --now resin-pi-agent.service
sysctl_ --no-pager --lines=0 status resin-pi-agent.service || true

echo
echo "Done. If PRINTER_API_KEY is still 'replace-me', edit /etc/resin-pi-agent.env"
echo "then: sudo systemctl restart resin-pi-agent"
