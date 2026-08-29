#!/bin/bash
# Place a single file (or nothing) onto the printer-facing USB gadget image and
# re-present it to the printer.
#
# Usage:
#   usb-refresh.sh <path-to-file>   copy exactly that file onto the gadget root
#   usb-refresh.sh --clear          leave the gadget empty
#
# History: this script used to look up "the current print job" from a local
# SQLite DB written by the on-Pi Flask app. That app is gone — the Pi now runs
# the thin `pi-agent`, which already knows which file was approved and passes it
# in as $1. The gadget-plumbing below (unload, losetup -fP, mount whichever
# partition node actually exists, copy, remount, and ALWAYS reload the gadget on
# the way out via a trap) is unchanged from the proven-working version; see
# CLAUDE.md for why each step is the way it is.
#
# Override env vars (all default to the production paths) let tests and the
# hardware smoke test point this at scratch fixtures:
#   PIUSB_IMAGE               gadget image file           (default /piusb.bin)
#   USB_REFRESH_MOUNT_POINT   loop mount point            (default /mnt/usbdrive)
set -u

IMAGE=${PIUSB_IMAGE:-/piusb.bin}
MOUNT_POINT=${USB_REFRESH_MOUNT_POINT:-/mnt/usbdrive}

MODE="file"
SRC=""
case "${1:-}" in
    "")        echo "usage: $0 <file> | --clear" >&2; exit 2 ;;
    --clear)   MODE="clear" ;;
    *)         SRC="$1"
               if [ ! -f "$SRC" ]; then
                   echo "ERROR: no such file: $SRC" >&2
                   exit 2
               fi ;;
esac

LOOPDEV=""
MOUNTED=0

# Whatever else goes wrong below, leave the printer with a working (if stale)
# drive on the way out.
cleanup_and_reload() {
    if [ "$MOUNTED" = "1" ]; then
        umount "$MOUNT_POINT" 2>/dev/null
    fi
    if [ -n "$LOOPDEV" ]; then
        losetup -d "$LOOPDEV" 2>/dev/null
    fi
    modprobe g_mass_storage file="$IMAGE" stall=0 ro=0 removable=1
}
trap cleanup_and_reload EXIT

# Disconnect from the printer (ignore "already unloaded").
modprobe -r g_mass_storage 2>/dev/null || true
sleep 1

LOOPDEV=$(losetup -fP --show "$IMAGE")
mkdir -p "$MOUNT_POINT"

# /piusb.bin may be bare FAT32 (no partition table) or MBR-partitioned — mount
# whichever node is actually present.
if [ -b "${LOOPDEV}p1" ]; then
    PART="${LOOPDEV}p1"
else
    PART="$LOOPDEV"
fi

if ! mount "$PART" "$MOUNT_POINT"; then
    echo "ERROR: failed to mount $PART — leaving drive as-is, still reloading gadget" >&2
    exit 1
fi
MOUNTED=1

# Clear the drive root, then place the new file (if any).
find "$MOUNT_POINT" -mindepth 1 -maxdepth 1 -exec rm -rf {} +

if [ "$MODE" = "file" ]; then
    cp "$SRC" "$MOUNT_POINT/$(basename "$SRC")"
fi

sync
if [ "$MODE" = "file" ]; then
    echo "USB drive refreshed with $(basename "$SRC")."
else
    echo "USB drive cleared."
fi
