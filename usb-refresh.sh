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
set -e

BASE=/opt/printer-upload
FILES_SRC=$BASE/files
DB=$BASE/uploads.db
MOUNT_POINT=/mnt/usbdrive

CURRENT=$(python3 -c "
import sqlite3
c = sqlite3.connect('$DB')
row = c.execute(\"SELECT folder, filename FROM print_jobs WHERE status='printing' ORDER BY id DESC LIMIT 1\").fetchone()
print(f'{row[0]}\t{row[1]}' if row else '')
")

# Disconnect from printer
modprobe -r g_mass_storage 2>/dev/null
sleep 1

# Attach the image via losetup so its partition is exposed, then mount the
# actual filesystem partition (not the raw disk image).
LOOPDEV=$(losetup -fP --show /piusb.bin)
mkdir -p "$MOUNT_POINT"
mount "${LOOPDEV}p1" "$MOUNT_POINT"

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
umount "$MOUNT_POINT"
losetup -d "$LOOPDEV"
sleep 1

# Re-present to printer
modprobe g_mass_storage file=/piusb.bin stall=0 ro=0

echo "USB drive refreshed (current job: ${CURRENT:-none})."
