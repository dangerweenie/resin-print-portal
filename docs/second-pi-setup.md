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
finish the rest of this runbook (gadget setup, pi-agent install)
unattended. See below.

**Status: NOT YET HARDWARE-VERIFIED.** The automated path was built from
current Raspberry Pi OS Trixie docs/source, not confirmed against a real
card yet. Run through it once, watch it actually come up, before relying on
it for a real site deploy. If it doesn't work, the old manual steps
(preserved further down under "Manual fallback") still get you there.

`make test-provision` (`provisioning/test-provision-sd.sh`) is an off-hardware
check: it runs `provision-sd.sh --boot` against a fake boot partition and
verifies the cloud-init files, the staged `pi-agent` payload, and that
`pi/install.sh` (the single installer `provision-boot.sh` now calls on the Pi)
lays every file down where it should. It catches the staging contract
drifting; it does **not** replace booting a real card.

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
`runcmd`, and handles the USB gadget + pi-agent install in one pass, then
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
   `network-config`), then its `runcmd` stage runs
   `provisioning/provision-boot.sh` for the USB gadget + pi-agent install
   (the agent is a single static Go binary — no `apt`/`pip` involved).
   That script reboots itself once more at the end so the new `config.txt`
   dtoverlay loads. A few minutes total.

   **Before flashing** (one-time fleet setup):
   ```bash
   make pi-agent                                  # -> bin/pi-agent-armv6
   cp provisioning/fleet.env.example provisioning/fleet.env
   $EDITOR provisioning/fleet.env                 # CENTRAL_URL (ENROLL_TOKEN optional)
   ```
   `provision-sd.sh` bakes `CENTRAL_URL` (identical on every card) into the Pi's
   `/etc/resin-pi-agent.env`. It errors out if the binary or `CENTRAL_URL` is
   missing. `ENROLL_TOKEN` is optional — set it only if the portal requires one
   (a public-facing deployment); otherwise enrollment is open and the admin
   approving the Pi is the security gate.
3. Verify:
   ```bash
   ssh captain@<hostname>.lan   # NOT .local — mDNS doesn't resolve from
                                 # this WSL2 client; use .lan (or the IP)
   cat /var/log/provision-boot.log   # confirm the gadget + pi-agent step finished
   systemctl status resin-pi-agent piusb-gadget
   journalctl -u resin-pi-agent | tail   # look for "enrolled" then "waiting for ... approve"
   ```
   If SSH never comes up at all, pull the card and check `/boot-progress.log`
   and `/var/log/cloud-init-output.log` from a card reader.

From here, the Pi has: hostname/user/SSH set, Wi-Fi connected, Bluetooth/
avahi/triggerhappy disabled, `dtoverlay=dwc2,dr_mode=peripheral` +
`gpu_mem=16` in `config.txt`, a bare-FAT32 `/piusb.bin` with a
`piusb-gadget` systemd unit that (re)loads the gadget on **every** boot,
and `resin-pi-agent.service` running — it has already self-registered with
the portal and is waiting to be approved.

## 2. Approve the printer

The Pi registered itself on first boot. In the admin UI → **Printers →
Pending**, find the row (it's keyed by the Pi's hostname) and click
**Approve**. That's it — nothing to configure on the Pi.

Optionally, click into the printer and set its **model** (enables the
sliced-for cross-check) and **allowed extensions** for that machine's real
format(s). The default is "accept anything" plus the standard safety
checklist, so it works without this.

Then link Slack names and certify members under **Members** /
**Certifications**.

```bash
# on the Pi, confirm it went live after approval:
curl -s -o /dev/null -w '%{http_code}\n' http://localhost/   # expect 200
```

## 3. Certification is central now

Unlike the old per-Pi Flask app, there is **one** members roster and one
certification list, in the central portal's Postgres. Certifying someone
for this printer in the admin UI is all that's needed — no per-Pi account
creation. Membership active/inactive comes from TinkerAccess automatically
via the sync worker.

## Redeploying the agent (not a fresh flash)
Once a Pi is provisioned, pushing an agent update:
```bash
make pi-agent
scp bin/pi-agent-armv6 usb-refresh.sh captain@<hostname>.lan:~/
ssh captain@<hostname>.lan 'sudo install -m0755 ~/pi-agent-armv6 /usr/local/bin/pi-agent && \
    sudo install -m0755 ~/usb-refresh.sh /usr/local/bin/usb-refresh.sh && \
    sudo systemctl restart resin-pi-agent'
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

**Install the pi-agent** (build it on your dev machine first with
`make pi-agent`):
```bash
scp bin/pi-agent-armv6 usb-refresh.sh pi/resin-pi-agent.service pi/install.sh \
    pi/config.example.env captain@<hostname>.lan:~/agent-install/
ssh captain@<hostname>.lan 'sudo bash ~/agent-install/install.sh ~/agent-install'
# install.sh seeds /etc/resin-pi-agent.env from config.example.env — set
# CENTRAL_BASE_URL in it (ENROLL_TOKEN only if the portal requires one), then:
ssh captain@<hostname>.lan 'sudo nano /etc/resin-pi-agent.env && sudo systemctl restart resin-pi-agent'
```
The Pi then self-registers; approve it in the admin UI under **Printers →
Pending**.
`install.sh` drops the binary at `/usr/local/bin/pi-agent`, installs the
systemd unit, seeds `/etc/resin-pi-agent.env`, and removes the old Flask
`printer-upload.service` / `/opt/printer-upload` if it finds them. No Python
on the Pi at all.

## Not yet started
- Captive-portal wifi provisioning — not built anywhere yet. Wi-Fi still
  has to be known at flash time (baked in by `provision-sd.sh`) or you're
  plugging in a keyboard/monitor once to configure it manually.
