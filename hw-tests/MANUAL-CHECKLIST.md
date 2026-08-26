# Manual / human-in-the-loop hardware checklist

Layer 4 of `hw-tests/` (see `README.md` for the full picture). Everything
here needs either eyes on a printer's screen or a physical action (power
cycle, cable pull) that an SSH session can't script on itself. Run through
this on `resin1` after any change to `usb-refresh.sh`, the gadget config, or
the print-job lifecycle in `app.py` — and periodically regardless, since
firmware/behavior can drift per printer without any code change here.

Working style note (from `CLAUDE.md`): you're watching the printer screen,
Claude Code is driving the Pi — one step at a time, confirm before the next.

## 1. File actually shows up and opens, per printer model

The automated Layer 2 test (`test-full-flow-against-pi.sh`) only proves the file
is byte-correct *on the FAT32 filesystem the gadget presents*. It cannot
prove a given printer's firmware actually lists it, previews it, or opens it
— that's model-specific and has bitten this project before (see `CLAUDE.md`'s
whole M7 Pro saga). Repeat per printer currently on hand:

| Printer | Format | Shows in browser? | Opens/previews OK? | Notes |
|---|---|---|---|---|
| Anycubic Photon Mono M7 Pro | `.pwsz`/`.pm7`/`.pm7m` | ☐ | ☐ | The picky one — strictest USB firmware |
| Elegoo Saturn 3 | `.goo`/`.ctb` | ☐ | ☐ | Lenient — good default rig printer for frequent automated runs |
| Anycubic Photon (P1) | `.pp1` | ☐ | ☐ | Confirmed root-only file listing (no subfolders) |

## 2. Full print-to-completion, at least once per printer

Per `CLAUDE.md`, "shows in browser" and "opens" have been confirmed on all
three printers, but an actual **full print run to completion** has not.
Combine this with the new self-report flow:
1. Start a real print through the portal (real checklist, real file).
2. Let it run to completion on the physical printer.
3. Click "I've removed it — mark finished" in the portal.
4. Confirm the drive is now empty (unplug/replug a laptop into the Pi's port
   if you want to see it as a generic USB drive, or just trust
   `test-full-flow-against-pi.sh`'s own verification method).

## 3. Reboot recovery (hardware-verifies the still-unconfirmed provisioning path)

`docs/second-pi-setup.md` and `provisioning/provision-sd.sh` are explicitly
flagged **NOT YET HARDWARE-VERIFIED**. Running this on `resin1` is the
verification:
```bash
ssh captain@resin1.lan
sudo reboot
# wait ~30-60s
ssh captain@resin1.lan 'systemctl is-active piusb-gadget printer-upload; lsmod | grep g_mass_storage'
curl -s -o /dev/null -w '%{http_code}\n' http://resin1.lan/
```
Expect: both units `active`, `g_mass_storage` loaded, HTTP `200`/`302` —
all without touching anything by hand. If this passes, update the
"NOT YET HARDWARE-VERIFIED" notes in `docs/second-pi-setup.md` /
`provisioning/provision-sd.sh` to say so.

## 4. Real power-cycle resilience (this is not optional — it's normal operation)

Per `CLAUDE.md`: **the Pi draws its power from the printer's own USB port.**
There is no separate power switch and no graceful-shutdown path in normal
use — every time someone flips the printer off at the end of the day, the
Pi's SD card gets a hard power cut, same as yanking power on any Linux box.
This happens routinely, not just as an edge case, so it's worth verifying
deliberately rather than assuming it's fine:

1. **Idle power cut**: with nothing printing, turn the printer off, wait a
   few seconds, turn it back on.
   ```bash
   ssh captain@resin1.lan 'sudo fsck.fat -n /dev/<piusb-partition>'  # -n = check only, don't fix
   ssh captain@resin1.lan 'systemctl is-active piusb-gadget printer-upload'
   ```
   Expect: `fsck.fat` reports clean (or only trivial dirty-bit, per
   `CLAUDE.md`'s known-harmless note), both services active, drive
   reappears on the printer without any manual step.
2. **Mid-refresh power cut** (the risky case): start a print (so
   `usb-refresh.sh` is actively unmounting/copying/remounting), and cut
   power to the printer partway through that ~2-4 second window. Repeat the
   same checks above afterward. This is inherently timing-sensitive by
   hand — a few attempts to actually land inside the window is expected.
   Watch specifically for: FAT corruption requiring more than a trivial
   `fsck.fat` fix, the gadget failing to reload on boot, or the SD card's
   own root filesystem needing repair (`dmesg | grep -Ei 'ext4|remount-ro'`
   on the next successful boot).

If mid-refresh power cuts turn out to reliably corrupt something, that's a
real finding worth raising as a design question (e.g., shortening the
unmount/copy/remount window, or accepting the risk with periodic SD health
checks) — not something to silently work around here.

## 5. Physical USB replug (distinct from a power cycle)

With the printer left powered on, unplug the Pi's USB cable from the
printer's port and plug it back in (simulates a loose cable/connector issue
rather than a power event). Confirm the printer re-detects the drive without
needing `modprobe` run again by hand — dwc2 on some Pi/kernel combos has had
replug-detection quirks historically, worth a periodic sanity check.

## 6. Filename edge cases

Upload files with: spaces, unicode characters, a very long name (>50 chars),
and a double extension (`model.v2.goo`). Start a print with each and check
how the filename actually renders on each printer's on-screen file list —
`usb-refresh.sh`'s own revision history already notes real per-model
differences in how folder/attribution info survives (flattened+prefixed was
adopted specifically because of this). `secure_filename()` plus FAT32's
lossy long-filename handling is a second place things can mangle a name
that a byte-level content check can't see.
