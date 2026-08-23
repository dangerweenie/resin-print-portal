#!/bin/bash
# On-hardware validation for usb-refresh.sh -- run this ON the Pi itself
# (over SSH is fine), as root. It cannot be run from a dev machine: it needs
# the real losetup/mount/modprobe stack and, for sub-test C specifically,
# the real g_mass_storage module.
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
# real gadget driver, using disposable scratch images -- it never touches
# real member data.
#
# WARNING: running this will unload/reload the g_mass_storage USB gadget
# several times over the next ~30-60 seconds -- the printer's drive will
# briefly disconnect and reconnect (inherent to g_mass_storage: only one
# gadget instance at a time). Run this BETWEEN prints, not while one is in
# progress, and not as part of routine operation.
#
# Usage:
#   sudo bash hw-tests/test-usb-refresh-on-pi.sh          # asks to confirm
#   sudo bash hw-tests/test-usb-refresh-on-pi.sh --yes    # skips the prompt
#
# Env overrides (all optional):
#   SCRIPT_UNDER_TEST   path to usb-refresh.sh to test (default: the
#                       deployed copy at /usr/local/bin/usb-refresh.sh --
#                       point this at an unstaged working-tree copy to test
#                       changes before running deploy.sh)
#   SCRATCH_DIR         where scratch images/DBs live (default:
#                       /opt/printer-upload/hw-test-scratch -- deliberately
#                       NOT /tmp, which CLAUDE.md documents as a 213MB tmpfs
#                       that has silently truncated large writes before)
set -u

SCRIPT_UNDER_TEST=${SCRIPT_UNDER_TEST:-/usr/local/bin/usb-refresh.sh}
SCRATCH_DIR=${SCRATCH_DIR:-/opt/printer-upload/hw-test-scratch}
YES=0
[ "${1:-}" = "--yes" ] && YES=1

FAILURES=0
RESULTS=()
pass() { RESULTS+=("PASS: $1"); echo "PASS: $1"; }
fail() { RESULTS+=("FAIL: $1"); echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }

# --- Preflight --------------------------------------------------------
if [ "$(id -u)" != "0" ]; then
    echo "Must run as root (needs modprobe/losetup/mount). Try: sudo bash $0" >&2
    exit 2
fi

MISSING=""
for tool in mkdosfs losetup mount parted lsmod cmp modprobe python3; do
    command -v "$tool" >/dev/null 2>&1 || MISSING="$MISSING $tool"
done
if [ -n "$MISSING" ]; then
    echo "Missing required tools:$MISSING" >&2
    echo "Try: apt install -y dosfstools parted util-linux" >&2
    exit 2
fi

if [ ! -e "$SCRIPT_UNDER_TEST" ]; then
    echo "usb-refresh.sh not found at $SCRIPT_UNDER_TEST (set SCRIPT_UNDER_TEST=... to override)" >&2
    exit 2
fi

echo "=================================================================="
echo " This will unload/reload the g_mass_storage USB gadget several"
echo " times over the next ~30-60 seconds -- the printer's drive will"
echo " BRIEFLY DISCONNECT AND RECONNECT repeatedly."
echo " Run this BETWEEN prints, not while one is in progress."
echo "=================================================================="
if [ "$YES" != "1" ]; then
    read -r -p "Continue? [y/N] " REPLY
    case "$REPLY" in
        [yY]*) ;;
        *) echo "Aborted."; exit 3 ;;
    esac
fi

rm -rf "$SCRATCH_DIR"
mkdir -p "$SCRATCH_DIR"

# --- Cleanup: unconditionally leave the REAL production gadget loaded --
# Mirrors the exact lesson usb-refresh.sh itself already learned: no
# blanket `set -e`, explicit per-step checks, and a trap that always runs
# regardless of pass/fail/interrupt.
cleanup() {
    modprobe -r g_mass_storage 2>/dev/null || true
    modprobe g_mass_storage file=/piusb.bin stall=0 ro=0 removable=1
    rm -rf "$SCRATCH_DIR"
}
trap cleanup EXIT

seed_job() {  # $1=base dir  $2=folder  $3=filename
    mkdir -p "$1/files/$2"
    head -c 65536 /dev/urandom > "$1/files/$2/$3"
    python3 -c "
import sqlite3
c = sqlite3.connect('$1/uploads.db')
c.execute('CREATE TABLE print_jobs(id INTEGER PRIMARY KEY, folder TEXT, filename TEXT, status TEXT)')
c.execute(\"INSERT INTO print_jobs VALUES (1, '$2', '$3', 'printing')\")
c.commit(); c.close()
"
}

read_back_and_compare() {  # $1=image $2=partitioned(0/1) $3=fname $4=src $5=verify mount point
    local image=$1 partitioned=$2 fname=$3 src=$4 vmount=$5 loop part ok=1
    loop=$(losetup -fP --show "$image") || return 1
    part="$loop"; [ "$partitioned" = "1" ] && part="${loop}p1"
    mkdir -p "$vmount"
    if ! mount "$part" "$vmount"; then
        losetup -d "$loop"
        return 1
    fi
    [ "$(ls -A "$vmount")" = "$fname" ] || ok=0
    cmp -s "$vmount/$fname" "$src" || ok=0
    umount "$vmount"
    losetup -d "$loop"
    [ "$ok" = "1" ]
}

# --- Sub-test A: bare FAT32 (no partition table) -----------------------
run_subtest_a() {
    local img="$SCRATCH_DIR/a-bare.bin" base="$SCRATCH_DIR/a-base"
    dd if=/dev/zero of="$img" bs=1M count=64 status=none
    mkdosfs -F 32 "$img" >/dev/null
    seed_job "$base" testuser model.goo

    if PIUSB_IMAGE="$img" PRINTER_UPLOAD_BASE="$base" USB_REFRESH_MOUNT_POINT="$SCRATCH_DIR/a-mnt" \
        bash "$SCRIPT_UNDER_TEST"; then
        if read_back_and_compare "$img" 0 model.goo "$base/files/testuser/model.goo" "$SCRATCH_DIR/a-verify"; then
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
    local img="$SCRATCH_DIR/b-mbr.bin" base="$SCRATCH_DIR/b-base" loop
    dd if=/dev/zero of="$img" bs=1M count=64 status=none
    parted -s "$img" mklabel msdos
    parted -s "$img" mkpart primary fat32 1MiB 100%
    loop=$(losetup -fP --show "$img")
    mkdosfs -F 32 "${loop}p1" >/dev/null
    losetup -d "$loop"
    seed_job "$base" testuser model.goo

    if PIUSB_IMAGE="$img" PRINTER_UPLOAD_BASE="$base" USB_REFRESH_MOUNT_POINT="$SCRATCH_DIR/b-mnt" \
        bash "$SCRIPT_UNDER_TEST"; then
        if read_back_and_compare "$img" 1 model.goo "$base/files/testuser/model.goo" "$SCRATCH_DIR/b-verify"; then
            pass "B: MBR-partitioned image -- file landed correctly"
        else
            fail "B: MBR-partitioned image -- file missing or content mismatch after refresh"
        fi
    else
        fail "B: MBR-partitioned image -- usb-refresh.sh exited nonzero"
    fi
}

# --- Sub-test C: unmountable image must not orphan the gadget ----------
# This is the test that most directly re-validates the original bug: a
# mount failure used to leave the printer permanently disconnected.
run_subtest_c() {
    local img="$SCRATCH_DIR/c-corrupt.bin" base="$SCRATCH_DIR/c-base"
    # A valid backing file for the gadget (nonzero size) with no recognizable
    # filesystem -- mountable=no, loadable-as-gadget-backing=yes. A truly
    # empty (0-byte) file would confound the test: g_mass_storage itself may
    # refuse to load against it, which would fail for the wrong reason.
    dd if=/dev/urandom of="$img" bs=1M count=4 status=none
    seed_job "$base" testuser model.goo

    modprobe -r g_mass_storage 2>/dev/null || true
    modprobe g_mass_storage file=/piusb.bin stall=0 ro=0 removable=1

    if PIUSB_IMAGE="$img" PRINTER_UPLOAD_BASE="$base" USB_REFRESH_MOUNT_POINT="$SCRATCH_DIR/c-mnt" \
        bash "$SCRIPT_UNDER_TEST"; then
        fail "C: expected usb-refresh.sh to fail against an unmountable image, but it exited 0"
    elif lsmod | grep -q '^g_mass_storage'; then
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
