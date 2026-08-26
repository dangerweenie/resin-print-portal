# New Pi Setup Runbook (Pi Zero W — fleet standard)

## ⚠️ 2026-08-23 — fleet hardware decision changed
The project is standardizing on the original **Pi Zero W** (not Zero 2 W) for
every printer going forward, for cost savings. The first Pi (`resin`, serving
the M7 Pro) is a Zero 2 W and stays that way — see `CLAUDE.md` for its whole
debugging history — but every *new* Pi from here on is a Zero W. This doc
used to be framed as "the second Pi" (a one-off); it's now the standard
runbook for any new Pi.

## ⚠️ 2026-08-23 — flashing is now automated, not a manual Imager-wizard walk
Everything in "1. Flash the OS" used to mean clicking through Raspberry Pi
Imager's Advanced Options each time. That's replaced by
`provisioning/provision-sd.sh` (repo root), which can flash + partition
the SD card itself (`--device` mode) or just provision an
already-flashed one (`--boot` mode), then hands off to cloud-init to
finish the rest of this whole runbook (gadget setup, app deploy)
unattended. See below.

**Status: NOT YET HARDWARE-VERIFIED.** The automated path was built from
current Raspberry Pi OS Trixie docs/source, not confirmed against a real
card yet. Run through it once, watch it actually come up, before relying on
it for a real site deploy. If it doesn't work, the old manual steps
(preserved further down under "Manual fallback") still get you there.

## ⚠️ 2026-08-25 — provisioning rebuilt on cloud-init, not the old firstrun.sh hack
The first version of this automated path disabled cloud-init outright and
drove hostname/user/Wi-Fi setup through the legacy
`systemd.run=/boot/firmware/firstrun.sh systemd.unit=kernel-command-line.target`
kernel-cmdline trick instead (same mechanism Raspberry Pi Imager's "Advanced
Options" customization uses). That's explicitly the path Raspberry Pi's own
Trixie announcement calls "the legacy first-boot customisation system" —
Trixie ships cloud-init as the native replacement, with its own NoCloud
datasource (`user-data`/`network-config`/`meta-data` on the boot partition)
already present on every image, just unfilled by default. Fighting it
instead of using it needed a fragile two-boot dance: stage one
(`firstrun.sh`) ran during an early, network-less systemd target, then had
to force a hard reboot (`echo b > /proc/sysrq-trigger` — a plain `reboot`
was confirmed to hang there) into a *second* boot before stage two
(`provision-stage2.sh`, gated on `network-online.target`) could do anything
needing real networking. It also had a live bug: the Wi-Fi regulatory
domain was written into `cmdline.txt` by `raspi-config nonint
do_wifi_country`, which `firstrun.sh`'s own cleanup step then silently
clobbered by restoring `cmdline.txt` from an earlier snapshot.

`provision-sd.sh` now writes real `user-data`/`network-config` instead of
blank templates: hostname/user/password/SSH via `user-data`, and Wi-Fi
(SSID/password/**regulatory domain**) via `network-config`'s netplan
`wifis:` block — the same format this image's own template already
documented in its comments. cloud-init sequences things correctly on its
own (network-config is applied before the interface ever comes up;
`runcmd` only fires once real networking is up), so the two-stage
reboot-and-wait dance is gone — `provisioning/provision-boot.sh` (which
replaces both `firstrun.sh` and `provision-stage2.sh`) runs once, via
`runcmd`, and handles the USB gadget + app deploy in one pass, then
reboots itself once at the end so the new `config.txt` dtoverlay (device
tree overlays only load at firmware/bootloader time, not hot-reloadable)
takes effect.

## 1. Flash + provision (automated)

One command, card to fully-provisioned, including partitioning the SD
card itself. Download a **Raspberry Pi OS Lite (32-bit), Trixie** image
(`.img`, `.img.xz`, or `.img.zst`) first, then:
```bash
sudo ./provisioning/provision-sd.sh --device /dev/sdX --image ~/Downloads/raspios-lite.img.xz \
  --hostname resin3 --password '<pi login password>' \
  --wifi-ssid 'SiteWifi' --wifi-password '<wifi password>'
```
`--device` takes the SD card's **whole-disk** device node (e.g. `/dev/sdb`,
not `/dev/sdb1`) — check `lsblk` first to be sure which one is the card,
not your own machine's disk. The script refuses to run if the device
looks like it backs your own `/`, `/boot`, or `/home`, or if it's over
128GB (bigger than any card this fleet uses), and prints the device's
size/model and requires you to type the device path back to confirm
before it writes anything — this step erases the entire card, so that
confirmation is deliberately not skippable by default (`--yes` exists for
scripted use, not recommended when running it by hand).

Already flashed the card yourself (Imager, `dd`, etc.) and just want the
provisioning part? Mount its boot partition and use `--boot` instead of
`--device`/`--image`:
```bash
./provisioning/provision-sd.sh --boot /path/to/mounted/bootfs \
  --hostname resin3 --password '<pi login password>' \
  --wifi-ssid 'SiteWifi' --wifi-password '<wifi password>'
```

Either way: (`--user` defaults to `captain`, `--wifi-country` defaults to
`US`.) The site's Wi-Fi has to be known at flash time — there's still no
captive-portal/in-field provisioning (see "Not yet started" below).

2. Eject the card, put it in the Pi, power it on, and wait — cloud-init
   brings up hostname/user/SSH/Wi-Fi on the first boot (from `user-data`/
   `network-config`), then its `runcmd` stage (which only fires once real
   networking is up, so `apt`/`pip install` just works — no separate
   network-wait stage needed) runs `provisioning/provision-boot.sh` for
   the USB gadget + app deploy. That script reboots itself once more at
   the end so the new `config.txt` dtoverlay loads. A few minutes total.
3. Verify:
   ```bash
   ssh captain@<hostname>.lan   # NOT .local — mDNS doesn't resolve from
                                 # this WSL2 client; use .lan (or the IP)
   cat /var/log/provision-boot.log   # confirm the gadget+app deploy step finished
   curl -s -o /dev/null -w '%{http_code}\n' http://localhost/   # expect 200
   ```
   If SSH never comes up at all, pull the card and check
   `/boot-progress.log` and `/var/log/cloud-init-output.log` (root needed
   for the latter) on the boot/root partitions from a card reader —
   between them they show how far a stuck boot actually got.

From here, the Pi has: hostname/user/SSH set, Wi-Fi connected, Bluetooth/
avahi/triggerhappy disabled, `dtoverlay=dwc2,dr_mode=peripheral` +
`gpu_mem=16` in `config.txt`, a bare-FAT32 `/piusb.bin` with a
`piusb-gadget` systemd unit that (re)loads the gadget on **every** boot —
not just the first, which the old manual recipe never covered — and the
Flask portal deployed and running under systemd.

## 2. Configure for the target printer (still manual)
Log into `/admin` (username `captain`, password from
`/opt/printer-upload/.initial_admin_password` — generated fresh on first
run, not a fixed default; change it via Settings and delete that file),
then in Settings:
- **Printer Name** — whatever this Pi's printer actually is.
- **File Type Filter** — enable it, set to *only* that printer's actual
  supported extensions. Don't skip this — the first Pi's default filter
  didn't match the M7 Pro's real formats and it took a confusing "empty
  screen" moment to notice.
- **Supports folders** — leave unchecked. Currently unused/moot anyway
  (the drive only ever holds the current job's single file — see
  `resin_plans.md`), kept only in case multi-file browsing comes back later.
- **Safety Checklist** — review/adjust the default items if this printer
  has different physical-check needs (different vat/plate mechanism, etc).
- **Slack webhook** — optional, same as the first Pi.

This step is inherently printer-specific and manual — not something the
provisioning script attempts.

## 3. Certify members
Each Pi has its own separate `members` table — **certifying someone on the
first Pi does NOT give them access on this one.** If Hunter/Ed/Matt (or
anyone else) need access here too, they need a *separate* account created
via this Pi's own `/admin/members`, even though it's the same physical
person. This is a deliberate consequence of "one Pi = one printer = an
account here means certified on this specific printer" — not a bug, but
worth knowing about so it doesn't look like a broken sync.

## Redeploying app code changes (not a fresh flash)
Once a Pi is already provisioned, pushing a code change doesn't need any of
the above — use the normal deploy path from `CLAUDE.md`:
```bash
scp -r printer-upload usb-refresh.sh captain@<hostname>.lan:~/resin-print-portal/
ssh captain@<hostname>.lan 'cd ~/resin-print-portal/printer-upload && sudo bash deploy.sh'
```

## Manual fallback (if the automated flash-and-provision path fails)
Everything below is what `provisioning/provision-sd.sh` +
`provisioning/provision-boot.sh` do for you. Fall back to it by hand if the
automated path doesn't come up on a real card — worth filing as a bug
either way, since the goal is for this section to become unnecessary.

**Flash + first boot**: Raspberry Pi Imager, board = **Raspberry Pi Zero**.
OS: Raspberry Pi OS Lite (32-bit), Trixie — confirmed this build still
supports the original Zero W's ARMv6 chip (verified against current
sources, not just assumption — Debian is expected to drop ARMv6 support
after Trixie, so this is likely the last generation that supports this
board). In Imager's Advanced Options (gear icon): set hostname, enable
SSH, set the `captain` user + password, and preload the site's Wi-Fi
SSID/password.
```bash
ssh captain@<hostname>.lan
uname -m && cat /etc/os-release   # expect armv6l, Raspbian 13 trixie
```

**USB gadget** (hardware note: the Zero W's single micro-USB port does
double duty for power *and* OTG data — confirm on the actual board before
connecting to a printer):

`/boot/firmware/config.txt`, under `[all]`:
```
dtoverlay=dwc2,dr_mode=peripheral
```
```bash
sudo dd if=/dev/zero of=/piusb.bin bs=1M count=8192 status=progress
sudo mkdosfs /piusb.bin -F 32 -I -n RESINUSB
sudo modprobe g_mass_storage file=/piusb.bin stall=0 ro=0 removable=1
```
Then install `piusb-gadget.service` (repo root) so the gadget reloads on
every boot, not just this one:
```bash
sudo cp piusb-gadget.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now piusb-gadget
```

**Deploy the Flask portal**:
```bash
scp -r printer-upload usb-refresh.sh captain@<hostname>.lan:~/resin-print-portal/
ssh captain@<hostname>.lan 'cd ~/resin-print-portal/printer-upload && sudo bash deploy.sh'
```
(`deploy.sh` installs its own systemd unit now — no separate manual unit-file
step needed.) `apt install python3-venv` first if the venv step errors —
it's not preinstalled on Lite images.

## Not yet started
- Captive-portal wifi provisioning — not built anywhere yet. Wi-Fi still
  has to be known at flash time (baked in by `provision-sd.sh`) or you're
  plugging in a keyboard/monitor once to configure it manually.
