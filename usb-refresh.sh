#!/bin/bash
# Sync the CURRENTLY ACTIVE print job's file — and only that file — onto the
# printer-facing USB gadget image, then re-present it to the printer.
#
# Revision history:
#   2026-08-21 (first fix): switched from a naive `mount -o loop` (which
#   silently failed against the MBR-partitioned /piusb.bin, so uploads never
#   reached the printer at all) to `losetup -fP` + mounting the partition.
#   Also started flattening every member's entire upload history onto the
#   drive root (member name prefixed onto each filename), since folder
#   browsing turned out to be unreliable across the printer lineup: Elegoo
#   Saturn 3 confirmed root-only (subfolder files invisible entirely);
#   Anycubic M7 Pro / P1 confirmed to recurse into subfolders but never show
#   the folder name on screen, so flat+prefixed was still the only way to
#   get visible attribution there too. Saturn 4 unconfirmed either way.
#
#   2026-08-21 (this revision): flattening *everything* still meant the
#   drive accumulated every file every member had ever uploaded, in every
#   format, regardless of which printer they were meant for — which is
#   exactly what produced an empty, confusing M7 Pro screen (six real files
#   on the drive, zero in a format the M7 Pro reads). Since only one
#   physical print can run on a given printer at a time anyway, the drive
#   now mirrors that reality directly: it holds exactly the single file
#   tied to the current `print_jobs` row (set by the "I'm printing this
#   now" action in app.py), nothing else. No filename collisions, no sort-
#   order guessing, no leftover clutter. Full upload history and the
#   member-attribution audit trail remain in the app's own database and
#   admin panel (/admin/files, /admin/log) regardless — this only changes
#   what's physically pushed to the printer's USB root.
#
#   NOTE: the "supports_folders" settings.json key (and its admin-settings
#   toggle) predates this revision and is currently unused — there's only
#   ever one file on the drive now, so folder support is moot. Left in
#   place rather than ripped out, in case multi-file browsing comes back
#   later (e.g. a real per-printer queue) and the toggle becomes relevant
#   again.
#
#   2026-08-22 (this revision): the script previously assumed /piusb.bin is
#   ALWAYS MBR-partitioned and hardcoded mounting "${LOOPDEV}p1" — but
#   second-pi-setup.md recommends building fresh images as bare FAT32 (no
#   partition table), which this script couldn't actually handle despite
#   docs claiming otherwise. Against a bare-FAT32 image, "${LOOPDEV}p1"
#   never exists, `mount` failed, and `set -e` killed the script AFTER
#   `modprobe -r` had already unloaded the gadget — leaving the printer
#   permanently disconnected until someone noticed and fixed it by hand.
#   Now: detect whether a partition node exists and mount whichever is
#   actually present, and unconditionally reload the gadget on the way out
#   (success or failure) via a trap, so a mount problem degrades to "drive
#   didn't get refreshed" instead of "printer loses its drive entirely."
#
#   Also added override env vars (PRINTER_UPLOAD_BASE, USB_REFRESH_MOUNT_POINT,
#   PIUSB_IMAGE) — all default to the exact production paths below, so this
#   is a no-op for the real systemd-invoked path. Lets tests and the
#   hardware smoke test point this script at disposable scratch fixtures
#   instead of the real device file.
set -u

BASE=${PRINTER_UPLOAD_BASE:-/opt/printer-upload}
FILES_SRC=$BASE/files
DB=$BASE/uploads.db
MOUNT_POINT=${USB_REFRESH_MOUNT_POINT:-/mnt/usbdrive}
IMAGE=${PIUSB_IMAGE:-/piusb.bin}

CURRENT=$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
row = c.execute(\"SELECT folder, filename FROM print_jobs WHERE status='printing' ORDER BY id DESC LIMIT 1\").fetchone()
print(f'{row[0]}\t{row[1]}' if row else '')
")

LOOPDEV=""
MOUNTED=0

# Always leave the printer with a working (even if stale/unrefreshed) drive
# on the way out, whatever else went wrong above.
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

# Disconnect from printer (ignore "already unloaded" — not an error here)
modprobe -r g_mass_storage 2>/dev/null || true
sleep 1

LOOPDEV=$(losetup -fP --show "$IMAGE")
mkdir -p "$MOUNT_POINT"

# /piusb.bin may be a bare FAT32 filesystem (no partition table — the
# simpler layout recommended for freshly-built images) or MBR-partitioned
# (the first Pi's existing image) — detect which and mount accordingly.
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

# Clear the drive root, then place the current job's file (if any).
find "$MOUNT_POINT" -mindepth 1 -maxdepth 1 -exec rm -rf {} +

if [ -n "$CURRENT" ]; then
    FOLDER=$(printf '%s' "$CURRENT" | cut -f1)
    FNAME=$(printf '%s' "$CURRENT" | cut -f2)
    SRC="$FILES_SRC/$FOLDER/$FNAME"
    if [ -f "$SRC" ]; then
        cp "$SRC" "$MOUNT_POINT/$FNAME"
    fi
fi

sync
echo "USB drive refreshed (current job: ${CURRENT:-none})."
