#!/bin/bash
# Layer 3 of hw-tests/: confirms the resin-pi-agent.service actually recovers
# from a hard crash on real systemd, on the real Pi -- and that killing the
# AGENT never touches the USB gadget, which must keep serving whatever
# file it already has regardless of whether the agent or central portal is up. Driven FROM
# THIS LAPTOP over SSH -- nothing is installed on the Pi; every remote
# command is systemctl/lsmod, already required for the gadget/app to run
# in production at all. The HTTP health check runs locally against the
# Pi's real hostname, same as a member's browser would hit it.
#
# Does NOT reboot the Pi -- an SSH session can't reliably survive
# triggering its own host's reboot, so full boot-recovery (does
# piusb-gadget.service + resin-pi-agent.service come back after a real
# reboot/power cycle) is a manual step -- see MANUAL-CHECKLIST.md.
#
# Usage:
#   bash hw-tests/test-service-resilience-against-pi.sh                  # asks to confirm
#   bash hw-tests/test-service-resilience-against-pi.sh --host resin1.lan --yes
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/lib/remote.sh"

HOST=resin1.lan
SSH_USER=captain
YES=0
while [ $# -gt 0 ]; do
    case "$1" in
        --host) HOST="$2"; shift 2 ;;
        --user) SSH_USER="$2"; shift 2 ;;
        --yes) YES=1; shift ;;
        *) echo "Unknown argument: $1" >&2; exit 1 ;;
    esac
done

HOST_URL="http://$HOST"
IMAGE=/piusb.bin

FAILURES=0
RESULTS=()
pass() { RESULTS+=("PASS: $1"); echo "PASS: $1"; }
fail() { RESULTS+=("FAIL: $1"); echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }

command -v curl >/dev/null 2>&1 || { echo "Missing required LOCAL tool: curl" >&2; exit 2; }

remote_setup "$HOST" "$SSH_USER" || exit 2
trap remote_teardown EXIT

ACTUAL_HOST=$(remote hostname | tr -d '\r\n')
echo "=================================================================="
echo " This SIGKILLs the real resin-pi-agent.service on $ACTUAL_HOST ($HOST)"
echo " to test crash-recovery. The upload page will be briefly unreachable."
echo " The USB gadget itself should NOT be affected."
echo "=================================================================="
if [ "$ACTUAL_HOST" != "resin1" ] || [ "$YES" != "1" ]; then
    read -r -p "Type that Pi's hostname ($ACTUAL_HOST) to confirm you want to run this against it: " CONFIRM
    [ "$CONFIRM" = "$ACTUAL_HOST" ] || { echo "Aborted."; exit 3; }
fi

if ! remote_sudo "systemctl list-unit-files resin-pi-agent.service" >/dev/null 2>&1; then
    echo "resin-pi-agent.service isn't installed on $ACTUAL_HOST -- run pi/install.sh first." >&2
    exit 2
fi
if ! remote_sudo "test -f $IMAGE"; then
    echo "$IMAGE not found on $ACTUAL_HOST -- is the USB gadget set up on this Pi?" >&2
    exit 2
fi

# --- Baseline: service up, gadget loaded -----------------------------
if ! remote_sudo "systemctl is-active --quiet resin-pi-agent"; then
    fail "baseline: resin-pi-agent.service was not active before the test even started"
    echo; printf '%s\n' "${RESULTS[@]}"; exit 1
fi
GADGET_WAS_LOADED=0
remote_sudo "lsmod" | grep -q '^g_mass_storage' && GADGET_WAS_LOADED=1

# --- Kill it hard, then poll for systemd's Restart=on-failure to bring it back
remote_sudo "systemctl kill -s SIGKILL resin-pi-agent" >/dev/null 2>&1

RECOVERED=0
for _ in $(seq 1 20); do   # up to 10s
    if remote_sudo "systemctl is-active --quiet resin-pi-agent"; then
        RECOVERED=1
        break
    fi
    sleep 0.5
done

if [ "$RECOVERED" = "1" ]; then
    pass "systemd restarted resin-pi-agent.service after SIGKILL (Restart=on-failure honored)"
else
    fail "resin-pi-agent.service did NOT come back within 10s of being killed"
fi

# --- Confirm the app actually answers HTTP again, not just "active" -----
HTTP_OK=0
for _ in $(seq 1 10); do   # up to 5s more
    CODE=$(curl -s -o /dev/null -w '%{http_code}' "$HOST_URL/" 2>/dev/null)
    if [ "$CODE" = "200" ] || [ "$CODE" = "302" ]; then
        HTTP_OK=1
        break
    fi
    sleep 0.5
done
if [ "$HTTP_OK" = "1" ]; then
    pass "app answers HTTP again after restart"
else
    fail "app did not answer HTTP within 5s of systemd reporting it active"
fi

# --- The gadget must be completely unaffected by the app crashing --------
if [ "$GADGET_WAS_LOADED" = "1" ]; then
    if remote_sudo "lsmod" | grep -q '^g_mass_storage'; then
        pass "USB gadget stayed loaded throughout the app crash/restart (printer never lost its drive)"
    else
        fail "USB gadget was unloaded as a side effect of the app crashing -- this should be impossible"
    fi
else
    echo "SKIP: gadget wasn't loaded before the test either, nothing to assert here."
fi

echo
echo "=================================================================="
printf '%s\n' "${RESULTS[@]}"
echo "=================================================================="
if [ "$FAILURES" = "0" ]; then
    echo "All sub-tests passed."
    exit 0
else
    echo "$FAILURES sub-test(s) failed."
    exit 1
fi
