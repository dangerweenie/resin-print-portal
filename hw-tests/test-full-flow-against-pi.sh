#!/bin/bash
# Layer 2 of hw-tests/ -- the flagship end-to-end test. Drives the REAL
# deployed pi-agent over HTTP FROM THIS LAPTOP (a more faithful "does a
# member's browser work" test than curl on the Pi), through the REAL
# central portal, then verifies the REAL /piusb.bin by briefly unloading
# the gadget over SSH, mounting it, checking contents, and reloading -- the
# same by-hand sequence CLAUDE.md documents.
#
# Nothing here installs anything on the Pi. HTTP calls happen locally
# (curl); gadget verification uses only losetup/mount/modprobe, which
# usb-refresh.sh already depends on in production. See hw-tests/lib/remote.sh.
#
# PREREQUISITES (set up once in the central portal admin UI):
#   * a printer whose slug matches this Pi's PRINTER_SLUG
#   * a member with a linked Slack name, certified for that printer
#   * that printer's API key (only needed for the central-side assertions;
#     the agent already has its own copy in /etc/resin-pi-agent.env)
#
# WARNING: disconnects/reconnects the USB gadget several times and creates a
# real print_jobs row + decision_log entries under the test member. Meant
# for a DEDICATED test rig (hostname resin1), never a printer in service.
#
# Usage:
#   bash hw-tests/test-full-flow-against-pi.sh \
#     --host resin1.lan --central https://portal.tinkermill.org \
#     --slug resin --api-key <printer key> --slack-name 'test.member' [--yes]
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/lib/remote.sh"

HOST=resin1.lan
SSH_USER=captain
CENTRAL=""
SLUG=resin
API_KEY=""
SLACK_NAME=""
YES=0
while [ $# -gt 0 ]; do
    case "$1" in
        --host) HOST="$2"; shift 2 ;;
        --user) SSH_USER="$2"; shift 2 ;;
        --central) CENTRAL="${2%/}"; shift 2 ;;
        --slug) SLUG="$2"; shift 2 ;;
        --api-key) API_KEY="$2"; shift 2 ;;
        --slack-name) SLACK_NAME="$2"; shift 2 ;;
        --yes) YES=1; shift ;;
        *) echo "Unknown argument: $1" >&2; exit 1 ;;
    esac
done

IMAGE=/piusb.bin
VMOUNT=/mnt/hwtest-verify
AGENT_URL="http://$HOST"
LOCAL_SCRATCH=$(mktemp -d)

command -v curl >/dev/null 2>&1 || { echo "need curl locally" >&2; exit 2; }
[ -n "$CENTRAL" ] && [ -n "$API_KEY" ] && [ -n "$SLACK_NAME" ] || {
    echo "need --central, --api-key and --slack-name (see header)" >&2; exit 2; }

FAILURES=0
RESULTS=()
pass() { RESULTS+=("PASS: $1"); echo "PASS: $1"; }
fail() { RESULTS+=("FAIL: $1"); echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }

api() { curl -s -H "Authorization: Bearer $API_KEY" "$CENTRAL/api/v1/printers/$SLUG$1"; }

remote_setup "$HOST" "$SSH_USER" || exit 2
cleanup() {
    remote_sudo "umount $VMOUNT" >/dev/null 2>&1
    remote_sudo "modprobe -r g_mass_storage" >/dev/null 2>&1
    remote_sudo "modprobe g_mass_storage file=$IMAGE stall=0 ro=0 removable=1" >/dev/null 2>&1
    rm -rf "$LOCAL_SCRATCH"
    remote_teardown
}
trap cleanup EXIT

ACTUAL_HOST=$(remote hostname | tr -d '\r\n')
echo "=================================================================="
echo " Full end-to-end test against $ACTUAL_HOST ($HOST) + $CENTRAL"
echo " Cycles the USB gadget and writes a real print job. Test rig only."
echo "=================================================================="
if [ "$ACTUAL_HOST" != "resin1" ] || [ "$YES" != "1" ]; then
    read -r -p "Type this Pi's hostname ($ACTUAL_HOST) to confirm: " C
    [ "$C" = "$ACTUAL_HOST" ] || { echo "Aborted."; exit 3; }
fi

# --- 0. agent up, central reachable ------------------------------------
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$AGENT_URL/")
[ "$CODE" = "200" ] && pass "agent upload page responds (200)" \
                   || fail "agent upload page returned $CODE"

CFG=$(api /config)
echo "$CFG" | grep -q "\"slug\":\"$SLUG\"" && pass "central /config reachable for slug $SLUG" \
                                          || fail "central /config failed: $CFG"

# --- 1. build a tiny synthetic .goo the sliced parser accepts ---------
# Minimal 195454-byte .goo: "V3.0" marker + a nonzero print_time at 195446.
GOO="$LOCAL_SCRATCH/hwtest.goo"
python3 - "$GOO" <<'PY'
import struct, sys
buf = bytearray(195454)
buf[0:4] = b"V3.0"
struct.pack_into(">I", buf, 195310, 120)      # layer_count
struct.pack_into(">I", buf, 195446, 3600)     # print_time (seconds)
open(sys.argv[1], "wb").write(buf)
PY
GOO_SHA=$(sha256sum < "$GOO" | cut -d' ' -f1)

# --- 2. submit through the agent -------------------------------------
CHECK_ARGS=()
i=0
while [ $i -lt 12 ]; do CHECK_ARGS+=(-F "check_$i=1"); i=$((i+1)); done
SUBMIT=$(curl -s -L -F "slack_name=$SLACK_NAME" "${CHECK_ARGS[@]}" \
    -F "file=@$GOO;filename=hwtest.goo" "$AGENT_URL/submit")
if echo "$SUBMIT" | grep -qi "on the printer"; then
    pass "agent accepted the upload and reported it placed on the gadget"
else
    fail "agent did not confirm placement; response: $(echo "$SUBMIT" | tr -d '\n' | head -c 300)"
fi

# --- 3. central shows the job --------------------------------------
CUR=$(api /current-job)
echo "$CUR" | grep -q '"filename":"hwtest.goo"' && pass "central current-job is hwtest.goo" \
                                                || fail "central current-job wrong: $CUR"
echo "$CUR" | grep -q '"eta_exact":true' && pass "central parsed the .goo ETA as exact" \
                                         || echo "NOTE: eta_exact not true (parser?) -- $CUR"

# --- 4. the real gadget actually holds the file ---------------------
LOOP=$(remote_sudo "losetup -fP --show $IMAGE" | tr -d '\r\n')
PART=$LOOP; remote_sudo "test -b ${LOOP}p1" && PART="${LOOP}p1"
remote_sudo "mkdir -p $VMOUNT && mount $PART $VMOUNT" >/dev/null 2>&1
LISTING=$(remote_sudo "ls -A $VMOUNT" | tr -d '\r')
REMOTE_SHA=$(remote_sudo "sha256sum $VMOUNT/hwtest.goo 2>/dev/null" | tr -d '\r' | cut -d' ' -f1)
remote_sudo "umount $VMOUNT; losetup -d $LOOP" >/dev/null 2>&1
[ "$LISTING" = "hwtest.goo" ] && pass "gadget root contains exactly hwtest.goo" \
                              || fail "gadget root listing = '$LISTING'"
[ "$REMOTE_SHA" = "$GOO_SHA" ] && pass "file on the gadget is byte-identical to what was uploaded" \
                              || fail "gadget file sha mismatch ($REMOTE_SHA vs $GOO_SHA)"

# --- 5. finish clears the gadget ----------------------------------
JOB_ID=$(echo "$CUR" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
curl -s -o /dev/null -F "job_id=$JOB_ID" "$AGENT_URL/finish"
LOOP=$(remote_sudo "losetup -fP --show $IMAGE" | tr -d '\r\n')
PART=$LOOP; remote_sudo "test -b ${LOOP}p1" && PART="${LOOP}p1"
remote_sudo "mkdir -p $VMOUNT && mount $PART $VMOUNT" >/dev/null 2>&1
LISTING=$(remote_sudo "ls -A $VMOUNT" | tr -d '\r\n')
remote_sudo "umount $VMOUNT; losetup -d $LOOP" >/dev/null 2>&1
[ -z "$LISTING" ] && pass "finish cleared the gadget" || fail "finish left files: $LISTING"

echo
echo "=================================================================="
printf '%s\n' "${RESULTS[@]}"
echo "=================================================================="
if [ "$FAILURES" = "0" ]; then echo "All checks passed."; exit 0
else echo "$FAILURES check(s) failed."; exit 1; fi
