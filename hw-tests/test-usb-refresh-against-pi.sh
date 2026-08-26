#!/bin/bash
# Layer 1 of hw-tests/: re-validates usb-refresh.sh's mount-detection logic
# against the REAL g_mass_storage driver on a real Pi, using disposable
# scratch images -- but driven FROM THIS LAPTOP over SSH, not run on the Pi.
#
# This exists so running it never requires installing anything on the
# system under test. The two fixture images (bare FAT32, MBR-partitioned)
# need mkdosfs/parted/losetup to build -- those run HERE, on this machine,
# and only the resulting bytes get shipped to the Pi via scp. The Pi itself
# is only ever asked to do things usb-refresh.sh already needs in
# production: losetup/mount/umount/modprobe (via sudo), plus a throwaway
# sqlite3 DB via the Pi's system python3 (sqlite3 is stdlib -- no package
# install). See hw-tests/lib/remote.sh for the full rationale.
#
# What it checks, and why: usb-refresh.sh previously assumed /piusb.bin is
# always MBR-partitioned. Against a bare-FAT32 image (the layout
# second-pi-setup.md recommends for a fresh Pi), the mount would fail and
# `set -e` would kill the script AFTER it had already unloaded the gadget --
# permanently orphaning the printer's USB drive until someone noticed by
# hand. The fix (partition-node detection + an EXIT trap that always
# reloads the gadget) is covered off-hardware for the mount/detection logic
# itself in printer-upload/tests/test_usb_refresh_mount_logic.py, but that
# can't exercise the real g_mass_storage module. This script re-validates
# the same two layouts PLUS the actual failure/recovery path, against the
# real gadget driver.
#
# WARNING: running this unloads/reloads the g_mass_storage USB gadget on
# the target Pi several times over the run -- whatever's plugged into that
# Pi's USB port will briefly disconnect and reconnect, repeatedly. Run this
# BETWEEN prints on a DEDICATED test rig (hostname resin1), never on a
# printer actually in service.
#
# Usage:
#   bash hw-tests/test-usb-refresh-against-pi.sh                    # asks to confirm
#   bash hw-tests/test-usb-refresh-against-pi.sh --host resin1.lan --yes
#
# Env overrides (all optional):
#   SCRIPT_UNDER_TEST   local path to a working-tree usb-refresh.sh to test
#                       before running deploy.sh (gets copied up to scratch
#                       and run from there). Default: the already-deployed
#                       /usr/local/bin/usb-refresh.sh on the target Pi.
#   SCRATCH_DIR         remote scratch dir (default:
#                       /opt/printer-upload/hw-test-scratch -- real disk,
#                       not /tmp, which CLAUDE.md documents as a 213MB
#                       tmpfs that has silently truncated large writes
#                       before).
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

SCRATCH_DIR=${SCRATCH_DIR:-/opt/printer-upload/hw-test-scratch}
VMOUNT="$SCRATCH_DIR/verify-mnt"
LOCAL_SCRATCH=$(mktemp -d)

FAILURES=0
RESULTS=()
pass() { RESULTS+=("PASS: $1"); echo "PASS: $1"; }
fail() { RESULTS+=("FAIL: $1"); echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }

# --- Preflight (local tools -- this laptop, not the Pi) --------------------
MISSING=""
for tool in mkdosfs parted losetup ssh scp sha256sum; do
    command -v "$tool" >/dev/null 2>&1 || MISSING="$MISSING $tool"
done
[ -n "$MISSING" ] && { echo "Missing required LOCAL tools:$MISSING (install these on your laptop, not the Pi)" >&2; exit 2; }
[ "$(id -u)" = "0" ] || echo "NOTE: building the MBR test image locally needs losetup, which usually needs root on THIS machine -- expect a local sudo prompt for that step." >&2

remote_setup "$HOST" "$SSH_USER" || exit 2

ACTUAL_HOST=$(remote hostname | tr -d '\r\n')
echo "=================================================================="
echo " This disconnects/reconnects the REAL USB gadget on $ACTUAL_HOST ($HOST)"
echo " several times. Meant for a DEDICATED test rig (hostname resin1),"
echo " never a printer actually in service."
echo "=================================================================="
if [ "$ACTUAL_HOST" != "resin1" ] || [ "$YES" != "1" ]; then
    read -r -p "Type that Pi's hostname ($ACTUAL_HOST) to confirm you want to run this against it: " CONFIRM
    [ "$CONFIRM" = "$ACTUAL_HOST" ] || { echo "Aborted."; remote_teardown; exit 3; }
fi

SCRIPT_UNDER_TEST_REMOTE=/usr/local/bin/usb-refresh.sh
if [ -n "${SCRIPT_UNDER_TEST:-}" ] && [ -f "${SCRIPT_UNDER_TEST:-}" ]; then
    SCRIPT_UNDER_TEST_REMOTE="$SCRATCH_DIR/usb-refresh-under-test.sh"
    echo "--- copying local $SCRIPT_UNDER_TEST to $ACTUAL_HOST:$SCRIPT_UNDER_TEST_REMOTE ---"
fi

cleanup() {
    remote_sudo "rm -rf $SCRATCH_DIR" >/dev/null 2>&1
    # Unconditionally leave the REAL production gadget loaded, regardless
    # of which scratch image the sub-tests below left it pointed at.
    remote_sudo "modprobe -r g_mass_storage" >/dev/null 2>&1
    remote_sudo "modprobe g_mass_storage file=/piusb.bin stall=0 ro=0 removable=1" >/dev/null 2>&1
    rm -rf "$LOCAL_SCRATCH"
    remote_teardown
}
trap cleanup EXIT

remote_sudo "rm -rf $SCRATCH_DIR" >/dev/null 2>&1
remote_sudo "mkdir -p $SCRATCH_DIR" >/dev/null
remote_sudo "chown $SSH_USER:$SSH_USER $SCRATCH_DIR" >/dev/null
if [ "$SCRIPT_UNDER_TEST_REMOTE" != "/usr/local/bin/usb-refresh.sh" ]; then
    remote_put "$SCRIPT_UNDER_TEST" "$SCRIPT_UNDER_TEST_REMOTE"
fi

# --- Helpers ---------------------------------------------------------------

seed_db_row() {  # $1=remote subdir under SCRATCH_DIR (e.g. a-base) $2=folder $3=filename
    remote "mkdir -p $SCRATCH_DIR/$1/files/$2 && python3 -" <<PYEOF
import sqlite3
c = sqlite3.connect('$SCRATCH_DIR/$1/uploads.db')
c.execute('CREATE TABLE IF NOT EXISTS print_jobs(id INTEGER PRIMARY KEY, folder TEXT, filename TEXT, status TEXT)')
c.execute("INSERT INTO print_jobs VALUES (1, '$2', '$3', 'printing')")
c.commit(); c.close()
PYEOF
}

# Mounts $1 (already loaded into g_mass_storage by usb-refresh.sh's own
# trap) via an independent loop device to verify contents, then releases
# that loop device -- mirrors the exact by-hand sequence CLAUDE.md
# documents, all via sudo so FAT32's lack of real permission bits never
# gets in the way of the read-back check.
verify_scratch_image() {  # $1=remote image path $2=partitioned(0/1) $3=fname $4=local_src
    local image=$1 partitioned=$2 fname=$3 local_src=$4
    local loop
    loop=$(remote_sudo "losetup -fP --show $image" | tr -d '\r\n') || return 1
    [ -n "$loop" ] || return 1
    local part=$loop
    [ "$partitioned" = "1" ] && part="${loop}p1"
    remote_sudo "mkdir -p $VMOUNT" >/dev/null
    if ! remote_sudo "mount $part $VMOUNT" >/dev/null 2>&1; then
        remote_sudo "losetup -d $loop" >/dev/null 2>&1
        return 1
    fi
    local listing remote_hash local_hash ok=1
    listing=$(remote_sudo "ls -A $VMOUNT" | tr -d '\r')
    [ "$listing" = "$fname" ] || ok=0
    remote_hash=$(remote_sudo "sha256sum $VMOUNT/$fname 2>/dev/null" | tr -d '\r' | cut -d' ' -f1)
    local_hash=$(sha256sum < "$local_src" | cut -d' ' -f1)
    [ "$remote_hash" = "$local_hash" ] || ok=0
    remote_sudo "umount $VMOUNT" >/dev/null 2>&1
    remote_sudo "losetup -d $loop" >/dev/null 2>&1
    [ "$ok" = "1" ]
}

run_usb_refresh() {  # $1=image $2=base $3=mountpoint
    remote_sudo "env PIUSB_IMAGE=$1 PRINTER_UPLOAD_BASE=$2 USB_REFRESH_MOUNT_POINT=$3 bash $SCRIPT_UNDER_TEST_REMOTE"
}

# --- Sub-test A: bare FAT32 (no partition table) ---------------------------
run_subtest_a() {
    local local_src="$LOCAL_SCRATCH/model-a.goo" local_img="$LOCAL_SCRATCH/a-bare.bin"
    head -c 65536 /dev/urandom > "$local_src"
    dd if=/dev/zero of="$local_img" bs=1M count=64 status=none
    mkdosfs -F 32 "$local_img" >/dev/null

    remote_put "$local_src" "$SCRATCH_DIR/a-model.goo" >/dev/null
    remote_put "$local_img" "$SCRATCH_DIR/a-bare.bin" >/dev/null
    remote "mkdir -p $SCRATCH_DIR/a-base/files/testuser && cp $SCRATCH_DIR/a-model.goo $SCRATCH_DIR/a-base/files/testuser/model.goo"
    seed_db_row a-base testuser model.goo

    if run_usb_refresh "$SCRATCH_DIR/a-bare.bin" "$SCRATCH_DIR/a-base" "$SCRATCH_DIR/a-mnt" >/dev/null; then
        if verify_scratch_image "$SCRATCH_DIR/a-bare.bin" 0 model.goo "$local_src"; then
            pass "A: bare FAT32 image -- file landed correctly"
        else
            fail "A: bare FAT32 image -- file missing or content mismatch after refresh"
        fi
    else
        fail "A: bare FAT32 image -- usb-refresh.sh exited nonzero"
    fi
}

# --- Sub-test B: MBR-partitioned (matches the CLAUDE.md-confirmed layout) --
run_subtest_b() {
    local local_src="$LOCAL_SCRATCH/model-b.goo" local_img="$LOCAL_SCRATCH/b-mbr.bin" loop
    head -c 65536 /dev/urandom > "$local_src"
    dd if=/dev/zero of="$local_img" bs=1M count=64 status=none
    sudo parted -s "$local_img" mklabel msdos
    sudo parted -s "$local_img" mkpart primary fat32 1MiB 100%
    loop=$(sudo losetup -fP --show "$local_img")
    sudo mkdosfs -F 32 "${loop}p1" >/dev/null
    sudo losetup -d "$loop"
    sudo chmod 644 "$local_img"

    remote_put "$local_src" "$SCRATCH_DIR/b-model.goo" >/dev/null
    remote_put "$local_img" "$SCRATCH_DIR/b-mbr.bin" >/dev/null
    remote "mkdir -p $SCRATCH_DIR/b-base/files/testuser && cp $SCRATCH_DIR/b-model.goo $SCRATCH_DIR/b-base/files/testuser/model.goo"
    seed_db_row b-base testuser model.goo

    if run_usb_refresh "$SCRATCH_DIR/b-mbr.bin" "$SCRATCH_DIR/b-base" "$SCRATCH_DIR/b-mnt" >/dev/null; then
        if verify_scratch_image "$SCRATCH_DIR/b-mbr.bin" 1 model.goo "$local_src"; then
            pass "B: MBR-partitioned image -- file landed correctly"
        else
            fail "B: MBR-partitioned image -- file missing or content mismatch after refresh"
        fi
    else
        fail "B: MBR-partitioned image -- usb-refresh.sh exited nonzero"
    fi
}

# --- Sub-test C: unmountable image must not orphan the gadget --------------
# The most direct regression test for the original bug: point the script at
# an unmountable image, confirm it exits nonzero AND the gadget module is
# still loaded afterward. Built with `dd` directly on the Pi -- no fixture
# transfer needed, `dd` is core coreutils either way.
run_subtest_c() {
    remote "mkdir -p $SCRATCH_DIR/c-base/files/testuser"
    remote_sudo "dd if=/dev/urandom of=$SCRATCH_DIR/c-corrupt.bin bs=1M count=4" >/dev/null
    remote_sudo "chown $SSH_USER:$SSH_USER $SCRATCH_DIR/c-corrupt.bin" >/dev/null
    remote "head -c 65536 /dev/urandom > $SCRATCH_DIR/c-base/files/testuser/model.goo"
    seed_db_row c-base testuser model.goo

    remote_sudo "modprobe -r g_mass_storage" >/dev/null 2>&1
    remote_sudo "modprobe g_mass_storage file=/piusb.bin stall=0 ro=0 removable=1" >/dev/null 2>&1

    if run_usb_refresh "$SCRATCH_DIR/c-corrupt.bin" "$SCRATCH_DIR/c-base" "$SCRATCH_DIR/c-mnt" >/dev/null 2>&1; then
        fail "C: expected usb-refresh.sh to fail against an unmountable image, but it exited 0"
    elif remote_sudo "lsmod" | grep -q '^g_mass_storage'; then
        pass "C: mount failure -- script exited nonzero AND gadget module still loaded (fix confirmed)"
    else
        fail "C: mount failure -- gadget module was left unloaded (this is the original bug)"
    fi
}

run_subtest_a
run_subtest_b
run_subtest_c

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
