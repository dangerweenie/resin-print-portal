#!/bin/bash
# Layer 2 of hw-tests/ -- the flagship test. Drives the REAL deployed app
# over HTTP, FROM THIS LAPTOP (not localhost on the Pi -- that's actually a
# more faithful test of "does a member's browser work" than curl running
# on the Pi itself would be), then verifies the REAL /piusb.bin by briefly
# unloading the real gadget over SSH, mounting it, checking contents, and
# reloading -- the same by-hand sequence CLAUDE.md documents.
#
# Nothing here installs anything on the Pi. HTTP calls happen locally
# (curl); DB/settings reads happen through the app's OWN already-installed
# venv python3 over SSH (sqlite3 is stdlib -- no separate package); gadget
# verification uses only losetup/mount/modprobe, which usb-refresh.sh
# already depends on in production. See hw-tests/lib/remote.sh.
#
# WARNING: disconnects/reconnects the USB gadget on the target Pi several
# times, and writes real print_jobs/uploads rows under a dedicated
# "Automated Hwtest" member account. Meant for a DEDICATED test rig
# (hostname resin1), never a printer actually in service.
#
# Usage:
#   bash hw-tests/test-full-flow-against-pi.sh                     # asks to confirm
#   bash hw-tests/test-full-flow-against-pi.sh --host resin1.lan --yes
#
# Env overrides:
#   PRINTER_UPLOAD_BASE   remote app base dir (default /opt/printer-upload)
#   ADMIN_PASSWORD        admin password for the admin-finish sub-test.
#                         Falls back to reading $BASE/.initial_admin_password
#                         on the Pi (present until someone changes it via
#                         Settings -- fine for a dedicated test rig).
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

BASE=${PRINTER_UPLOAD_BASE:-/opt/printer-upload}
IMAGE=/piusb.bin
VMOUNT=/mnt/hwtest-verify
HOST_URL="http://$HOST"
LOCAL_SCRATCH=$(mktemp -d)

TEST_FIRST=Automated
TEST_LAST=Hwtest
TEST_EMAIL=hwtest@resin1.invalid
TEST_PW=hwtest-automated-pw
FOLDER=hwtest_automated

FAILURES=0
RESULTS=()
pass() { RESULTS+=("PASS: $1"); echo "PASS: $1"; }
fail() { RESULTS+=("FAIL: $1"); echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }

MISSING=""
for tool in curl ssh scp sha256sum; do
    command -v "$tool" >/dev/null 2>&1 || MISSING="$MISSING $tool"
done
[ -n "$MISSING" ] && { echo "Missing required LOCAL tools:$MISSING (install these on your laptop, not the Pi)" >&2; exit 2; }

remote_setup "$HOST" "$SSH_USER" || exit 2

ACTUAL_HOST=$(remote hostname | tr -d '\r\n')
echo "=================================================================="
echo " This disconnects/reconnects the REAL USB gadget on $ACTUAL_HOST ($HOST)"
echo " several times, and writes real print_jobs/uploads rows tagged to a"
echo " dedicated 'Automated Hwtest' member account. Meant for a DEDICATED"
echo " test rig (hostname resin1), never a printer actually in service."
echo "=================================================================="
if [ "$ACTUAL_HOST" != "resin1" ] || [ "$YES" != "1" ]; then
    read -r -p "Type that Pi's hostname ($ACTUAL_HOST) to confirm you want to run this against it: " CONFIRM
    [ "$CONFIRM" = "$ACTUAL_HOST" ] || { echo "Aborted."; remote_teardown; exit 3; }
fi

if ! remote_sudo "systemctl is-active --quiet printer-upload"; then
    echo "printer-upload.service isn't active on $ACTUAL_HOST -- deploy first." >&2
    remote_teardown; exit 2
fi
if ! remote_sudo "test -f $IMAGE"; then
    echo "$IMAGE not found on $ACTUAL_HOST -- is the USB gadget set up on this Pi?" >&2
    remote_teardown; exit 2
fi

cleanup() {
    LEFTOVER=$(remote_sudo "losetup -j $IMAGE" 2>/dev/null | cut -d: -f1)
    [ -n "$LEFTOVER" ] && remote_sudo "losetup -d $LEFTOVER" >/dev/null 2>&1
    remote_sudo "umount $VMOUNT" >/dev/null 2>&1
    remote_sudo "modprobe -r g_mass_storage" >/dev/null 2>&1
    remote_sudo "modprobe g_mass_storage file=$IMAGE stall=0 ro=0 removable=1" >/dev/null 2>&1
    rm -rf "$LOCAL_SCRATCH"
    remote_teardown
}
trap cleanup EXIT

COOKIES="$LOCAL_SCRATCH/member.cookies"
ACOOKIES="$LOCAL_SCRATCH/admin.cookies"

# --- Helpers ----------------------------------------------------------

verify_drive_contents() {  # $1 = expected sorted, space-separated filename list ("" for empty)
    local expected="$1" loop part
    remote_sudo "modprobe -r g_mass_storage" >/dev/null 2>&1
    sleep 1
    loop=$(remote_sudo "losetup -fP --show $IMAGE" | tr -d '\r\n') || return 1
    part="$loop"
    remote_sudo "test -b ${loop}p1" && part="${loop}p1"
    remote_sudo "mkdir -p $VMOUNT" >/dev/null
    if ! remote_sudo "mount $part $VMOUNT" >/dev/null 2>&1; then
        remote_sudo "losetup -d $loop" >/dev/null 2>&1
        remote_sudo "modprobe g_mass_storage file=$IMAGE stall=0 ro=0 removable=1" >/dev/null 2>&1
        return 1
    fi
    local actual ok=1
    actual=$(remote_sudo "ls -A $VMOUNT" | tr -d '\r' | tr '\n' ' ' | sed 's/ $//')
    [ "$actual" = "$expected" ] || ok=0
    remote_sudo "umount $VMOUNT" >/dev/null 2>&1
    remote_sudo "losetup -d $loop" >/dev/null 2>&1
    remote_sudo "modprobe g_mass_storage file=$IMAGE stall=0 ro=0 removable=1" >/dev/null 2>&1
    [ "$ok" = "1" ]
}

verify_drive_file_matches() {  # $1 = filename $2 = local source file
    local fname="$1" src="$2" loop part
    remote_sudo "modprobe -r g_mass_storage" >/dev/null 2>&1
    sleep 1
    loop=$(remote_sudo "losetup -fP --show $IMAGE" | tr -d '\r\n') || return 1
    part="$loop"
    remote_sudo "test -b ${loop}p1" && part="${loop}p1"
    remote_sudo "mkdir -p $VMOUNT" >/dev/null
    if ! remote_sudo "mount $part $VMOUNT" >/dev/null 2>&1; then
        remote_sudo "losetup -d $loop" >/dev/null 2>&1
        remote_sudo "modprobe g_mass_storage file=$IMAGE stall=0 ro=0 removable=1" >/dev/null 2>&1
        return 1
    fi
    local ok=1 remote_hash local_hash listing
    listing=$(remote_sudo "ls -A $VMOUNT" | tr -d '\r')
    [ "$listing" = "$fname" ] || ok=0
    remote_hash=$(remote_sudo "sha256sum $VMOUNT/$fname 2>/dev/null" | tr -d '\r' | cut -d' ' -f1)
    local_hash=$(sha256sum < "$src" | cut -d' ' -f1)
    [ "$remote_hash" = "$local_hash" ] || ok=0
    remote_sudo "umount $VMOUNT" >/dev/null 2>&1
    remote_sudo "losetup -d $loop" >/dev/null 2>&1
    remote_sudo "modprobe g_mass_storage file=$IMAGE stall=0 ro=0 removable=1" >/dev/null 2>&1
    [ "$ok" = "1" ]
}

wait_for_refresh_done() {
    local timeout=${1:-20} waited=0
    sleep 0.5
    while remote_sudo "pgrep -f usb-refresh\\.sh" >/dev/null 2>&1; do
        sleep 0.5
        waited=$((waited + 1))
        [ "$waited" -gt "$((timeout * 2))" ] && { echo "WARN: usb-refresh.sh still running after ${timeout}s" >&2; break; }
    done
}

ensure_test_member() {
    local exists hash
    exists=$(remote_sudo_py "$BASE" <<PYEOF
import sqlite3
c = sqlite3.connect('$BASE/uploads.db')
print(c.execute("SELECT COUNT(*) FROM members WHERE email='$TEST_EMAIL'").fetchone()[0])
PYEOF
)
    exists=$(printf '%s' "$exists" | tr -d '\r' | tail -1)
    if [ "$exists" = "0" ]; then
        hash=$(remote_sudo_py "$BASE" <<PYEOF
from werkzeug.security import generate_password_hash
print(generate_password_hash('$TEST_PW'))
PYEOF
)
        hash=$(printf '%s' "$hash" | tr -d '\r' | tail -1)
        remote_sudo_py "$BASE" <<PYEOF
import sqlite3
from datetime import datetime
c = sqlite3.connect('$BASE/uploads.db')
c.execute("INSERT INTO members (first,last,email,password_hash,must_change_password,active,created_at,created_by) VALUES (?,?,?,?,0,1,?,?)",
    ('$TEST_FIRST','$TEST_LAST','$TEST_EMAIL',"""$hash""",datetime.now().strftime('%Y-%m-%d %H:%M:%S'),'hw-test'))
c.commit(); c.close()
PYEOF
    fi
    remote_sudo "mkdir -p $BASE/files/$FOLDER" >/dev/null
}

build_checklist_args() {
    local n
    n=$(remote_sudo_py "$BASE" <<PYEOF
import json
print(len(json.load(open('$BASE/settings.json'))['safety_checklist']))
PYEOF
)
    n=$(printf '%s' "$n" | tr -d '\r' | tail -1)
    CHECKLIST_ARGS=()
    local i
    for i in $(seq 0 $((n - 1))); do CHECKLIST_ARGS+=(-d "check_${i}=on"); done
}

get_end_reason() {  # $1 = filename
    remote_sudo_py "$BASE" <<PYEOF | tr -d '\r' | tail -1
import sqlite3
c = sqlite3.connect('$BASE/uploads.db')
row = c.execute("SELECT end_reason FROM print_jobs WHERE filename='$1' ORDER BY id DESC LIMIT 1").fetchone()
print(row[0] if row and row[0] else '')
PYEOF
}

get_printing_count() {
    remote_sudo_py "$BASE" <<PYEOF | tr -d '\r' | tail -1
import sqlite3
c = sqlite3.connect('$BASE/uploads.db')
print(c.execute("SELECT COUNT(*) FROM print_jobs WHERE status='printing'").fetchone()[0])
PYEOF
}

get_printing_filename() {
    remote_sudo_py "$BASE" <<PYEOF | tr -d '\r' | tail -1
import sqlite3
c = sqlite3.connect('$BASE/uploads.db')
row = c.execute("SELECT filename FROM print_jobs WHERE status='printing' ORDER BY id DESC LIMIT 1").fetchone()
print(row[0] if row else '')
PYEOF
}

member_login() {
    curl -s -c "$COOKIES" -b "$COOKIES" -X POST "$HOST_URL/" -d "email=$TEST_EMAIL" -d "password=$TEST_PW" -o /dev/null
}

upload_file() {  # $1 = filename
    local fname="$1" src="$LOCAL_SCRATCH/$fname"
    head -c 65536 /dev/urandom > "$src"
    curl -s -b "$COOKIES" -F "file=@$src;filename=$fname" \
        -H 'X-Requested-With: XMLHttpRequest' "$HOST_URL/my/$FOLDER/upload" -o /dev/null
}

start_print() {  # $1 = filename
    curl -s -b "$COOKIES" -X POST "$HOST_URL/my/$FOLDER/print" -d "filename=$1" "${CHECKLIST_ARGS[@]}" -o /dev/null
}

member_finish() {
    curl -s -b "$COOKIES" -X POST "$HOST_URL/my/$FOLDER/print/finish" -o /dev/null
}

admin_login() {
    local pw="${ADMIN_PASSWORD:-}"
    if [ -z "$pw" ]; then
        pw=$(remote_sudo "cat $BASE/.initial_admin_password" 2>/dev/null | tr -d '\r' | tail -1)
        [ -n "$pw" ] || {
            echo "No ADMIN_PASSWORD set and $BASE/.initial_admin_password is gone" \
                 "(already changed via Settings) -- set ADMIN_PASSWORD to run the admin sub-test." >&2
            return 1
        }
    fi
    curl -s -c "$ACOOKIES" -b "$ACOOKIES" -X POST "$HOST_URL/admin" -d "username=captain" -d "password=$pw" -o /dev/null
}

admin_finish() {
    curl -s -b "$ACOOKIES" -X POST "$HOST_URL/admin/print/finish" -o /dev/null
}

CHECKLIST_ARGS=()
ensure_test_member
build_checklist_args
member_login

# --- Test 1: upload -> print -> file is really on the drive, byte-for-byte
run_test_exposure() {
    upload_file model_a.goo
    start_print model_a.goo
    wait_for_refresh_done
    if verify_drive_file_matches model_a.goo "$LOCAL_SCRATCH/model_a.goo"; then
        pass "1: uploaded file is exposed on the real gadget drive, contents match byte-for-byte"
    else
        fail "1: file missing from the real gadget drive, or content mismatch, after starting a print"
    fi
}

# --- Test 2a: member self-reports finished -> drive clears -----------------
run_test_member_finish_clears_drive() {
    member_finish
    wait_for_refresh_done
    if verify_drive_contents ""; then
        pass "2a: member 'mark finished' clears the file from the real gadget drive"
    else
        fail "2a: drive still has content after member marked the print finished"
    fi
    local reason; reason=$(get_end_reason model_a.goo)
    [ "$reason" = "member_finished" ] \
        && pass "2a: end_reason recorded as member_finished" \
        || fail "2a: expected end_reason=member_finished, got '$reason'"
}

# --- Test 2b: supersede -- drive holds ONLY the new file --------------------
run_test_supersede() {
    upload_file model_b.goo
    upload_file model_c.goo
    start_print model_b.goo
    wait_for_refresh_done
    start_print model_c.goo
    wait_for_refresh_done
    if verify_drive_file_matches model_c.goo "$LOCAL_SCRATCH/model_c.goo"; then
        pass "2b: supersede -- drive holds only the new job's file, not the old one"
    else
        fail "2b: supersede -- drive did not end up holding exactly the new job's file"
    fi
    local reason; reason=$(get_end_reason model_b.goo)
    [ "$reason" = "superseded" ] \
        && pass "2b: superseded job tagged end_reason=superseded" \
        || fail "2b: expected end_reason=superseded for model_b.goo, got '$reason'"
}

# --- Test 2c: admin force-clear (the staff backstop) ------------------------
run_test_admin_finish() {
    if ! admin_login; then
        fail "2c: SKIPPED -- could not log in as admin (see message above)"
        return
    fi
    admin_finish
    wait_for_refresh_done
    if verify_drive_contents ""; then
        pass "2c: admin force-clear empties the drive"
    else
        fail "2c: drive still has content after admin force-clear"
    fi
    local reason; reason=$(get_end_reason model_c.goo)
    [ "$reason" = "admin_cleared" ] \
        && pass "2c: end_reason recorded as admin_cleared" \
        || fail "2c: expected end_reason=admin_cleared, got '$reason'"
}

# --- Test 3a: rapid concurrent print-starts (real network race, from two
# "browsers" at once) --------------------------------------------------------
run_test_race_safety() {
    upload_file race1.goo
    upload_file race2.goo
    upload_file race3.goo
    start_print race1.goo &
    start_print race2.goo &
    start_print race3.goo &
    wait
    wait_for_refresh_done 30

    local printing_count; printing_count=$(get_printing_count)
    [ "$printing_count" = "1" ] \
        && pass "3a: race -- exactly one job left marked 'printing' after concurrent starts" \
        || fail "3a: race -- expected exactly 1 'printing' job, found $printing_count"

    remote_sudo "lsmod" | grep -q '^g_mass_storage' \
        && pass "3a: race -- gadget module still loaded after concurrent refreshes" \
        || fail "3a: race -- gadget module was NOT loaded after concurrent refreshes"

    local leaked; leaked=$(remote_sudo "losetup -j $IMAGE" 2>/dev/null | wc -l)
    [ "$leaked" = "0" ] \
        && pass "3a: race -- no leaked loop device against $IMAGE" \
        || fail "3a: race -- $leaked leaked loop device(s) still attached to $IMAGE"

    local current_file; current_file=$(get_printing_filename)
    if [ -n "$current_file" ] && verify_drive_contents "$current_file"; then
        pass "3a: race -- drive contents match whichever job actually ended up 'printing'"
    else
        fail "3a: race -- drive contents don't match the DB's idea of the current job"
    fi
    admin_login && admin_finish && wait_for_refresh_done
}

# --- Test 3b: repeated cycles don't leak loop devices/mounts ----------------
run_test_no_resource_leak() {
    local i
    for i in 1 2 3 4 5; do
        upload_file "leak${i}.goo"
        start_print "leak${i}.goo"
        wait_for_refresh_done
        member_finish
        wait_for_refresh_done
    done
    local leaked mounted
    leaked=$(remote_sudo "losetup -j $IMAGE" 2>/dev/null | wc -l)
    mounted=0; remote_sudo "mountpoint -q $VMOUNT" 2>/dev/null && mounted=1
    if [ "$leaked" = "0" ] && [ "$mounted" = "0" ]; then
        pass "3b: 5 upload/print/finish cycles -- no leaked loop devices or mounts"
    else
        fail "3b: resource leak after repeated cycles (leaked loop devices: $leaked, still mounted: $mounted)"
    fi
}

run_test_exposure
run_test_member_finish_clears_drive
run_test_supersede
run_test_admin_finish
run_test_race_safety
run_test_no_resource_leak

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
