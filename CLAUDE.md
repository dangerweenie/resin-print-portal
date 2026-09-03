# Resin Printer USB Gadget — Project Context

## Goal
A Raspberry Pi acts as a USB mass-storage gadget (a "USB drive") plugged into
resin 3D printers that have no networking. The printer reads sliced files off the
Pi's emulated drive.

**Architecture (2026-08-27 rewrite — see README.md):** the Pi now runs a thin Go
`pi-agent` (no local web app, no database) that serves one upload page on the
makerspace LAN and forwards every submission to a **central Go portal** running
in Kubernetes (Helm chart under `deploy/helm/`, Postgres on a PVC). The portal
owns resin-printer certification, print-job tracking, the safety checklist, the
audit log, and Slack posting; a background worker syncs membership status
(A/I/S) from TinkerAccess's `get_users` endpoint
(`docs/GET_MEMBERS_ENDPOINT.md`). Members identify themselves by **tapping their
RFID fob** — the UID is matched against `members.code` from the synced roster
(the portal also still accepts a Slack name from other callers, but the Pi is
fob-only). The old per-Pi Flask app (`printer-upload/`) has been removed.

A freshly-flashed Pi carries one fleet-wide constant — `CENTRAL_BASE_URL`, set
once in `provisioning/fleet.env` — and **self-registers** on first boot via
`POST /api/v1/enroll`: the portal creates a `printers` row keyed by the Pi's
hardware id, slug = hostname, and issues a per-Pi API key the Pi persists to
`/var/lib/resin-pi-agent/creds.env`. It starts unapproved; an admin clicks
Approve once under Printers → Pending — **that approval is the security gate**.
The agent re-checks its registration on start and every 2 min: if the portal
401/403/404s the stored key (deleted printer, rotated key, wiped DB) it
discards the creds and re-enrolls; transient errors never trigger that.
`ENROLL_TOKEN` (both sides) is optional extra hardening for a public-facing
portal; without it the enroll endpoint is open and only the approval gates
anything. Nothing is configured per Pi.

**Fleet version + self-update (2026-09-03):** every binary carries a
`git describe` version (`internal/buildinfo`, `-ldflags -X`), and the pi-agent
sends it to the portal on every call (`X-Agent-Version`) — surfaced on the
admin Printers page with a "behind" badge. The portal image bundles the
cross-compiled `pi-agent-armv6` it was built with (`/agent/pi-agent-armv6`,
`AGENT_BINARY_PATH`) and serves it at `GET /api/v1/printers/{slug}/agent-binary`
+ `/agent-update` (printer bearer auth). `AGENT_AUTO_UPDATE=true` converges the
fleet to the portal's build; per-Pi **Update now** / **Hold** override it
(`printers.agent_target_override` / `agent_update_hold`, migration `00002`).
The Pi's `SelfUpdater` (`internal/piagent/selfupdate.go`) checks every 5 min,
never swaps mid-print, verifies SHA-256, keeps `pi-agent.prev`, and exits clean
so systemd (`Restart=always`) relaunches; `pi/agent-guard.sh` (`ExecStartPre`)
rolls back on a crash-loop. All update state is in `/var/lib/resin-pi-agent`,
never the boot partition — still nothing configured per Pi.

The USB-gadget half of the system below is unchanged and still load-bearing —
`usb-refresh.sh`, `piusb-gadget.service`, the `config.txt` dtoverlay, and all
the printer-firmware findings still apply exactly. Only the "what decides who
can print, and where files come from" layer changed.

**Identity is the RFID fob and nothing else** — no name-entry mode, no switch.
Every Pi has an MFRC522 on fixed pins (SPI0.0 / GPIO25); the agent won't start
without it. `internal/rfid` is a pure-Go MFRC522 driver via `periph.io`; the Pi
sends the UID as canonical hex and the portal (`internal/fobcode` +
`store.ResolveRFIDCode`) matches it against `members.code` in every format.
Nothing about the fob is configurable on the Pi. Each tap hits `/check` once
(deduped for a held fob) and is recorded in `decision_log`. `pi-agent -probe`
is a wiring diagnostic only.

## Printers on hand (test targets, in priority order)
1. **Anycubic Photon Mono M7 Pro** — CURRENT TARGET. The picky one. Reads `.pwsz`
   (also `.pm7`/`.pm7m`, same ZIP container). Strict USB firmware.
2. **Elegoo Saturn 3** — lenient, reads `.goo`/`.ctb`. Works under almost anything.
3. **Anycubic Photon (P1 / older Anycubic)** — untested here yet.

## Hardware notes
- **2026-08-23 fleet decision**: every *new* Pi is a **Zero W** (original,
  ARMv6, single micro-USB port), not Zero 2 W — cost savings. The first Pi
  (`resin`, serving the M7 Pro, everything documented below) stays a Zero 2
  W; that history isn't affected. See `docs/second-pi-setup.md` for the
  Zero W runbook, now driven by `provisioning/provision-sd.sh` for
  unattended flash-and-boot setup (NOT YET hardware-verified — see that
  doc's status note).
- First Pi (`resin`) is a Zero 2 W (RP3A0 chipset). Use the INNER micro-USB
  port labelled `USB` (OTG/data), NOT the outer `PWR` port (no data lines on
  any Zero).
- Pi draws power from the printer's USB port; printer must be ON for the Pi to run.
- One Pi serves ONE printer (a USB gadget can't split to multiple hosts).
- SSH: `captain@resin.lan` (hostname `resin`, user `captain`). NOTE: `resin.local`
  (mDNS) does NOT resolve from a WSL2 client — use `resin.lan` instead. Same
  password for both SSH login and `sudo` on the Pi.

## Build, test, and deploy

The central portal is Go (`cmd/portal`, `cmd/pi-agent`). Needs Go 1.24+ and
Docker for a throwaway Postgres. See README.md for the full walkthrough; the
short version, from the repo root:

```bash
make run-db              # local Postgres on :55432
make test               # unit tests, no DB
make test-integration   # + store/worker tests against run-db (skip without TEST_DATABASE_URL)
make build              # -> bin/portal   (server | worker | migrate subcommands)
make pi-agent           # -> bin/pi-agent-armv6   (static, cross-compiled for the Zero W)
make helm-lint          # fetch the Bitnami postgres subchart, lint + render the chart
```

The store/worker tests self-skip unless `TEST_DATABASE_URL` points at a
disposable database (same pattern the old loop-device test used for root).

### Deploy — central portal
Kubernetes + Helm: `helm install portal deploy/helm/resin-portal ...`.
`values.yaml` documents every setting; migrations run as a
`post-install,pre-upgrade` hook. Bundled Bitnami Postgres with a PVC by
default; `postgresql.enabled=false` + `externalDatabase.dsn` to bring your own.

### Deploy — Pi agent
Fresh SD card is the normal path: `make pi-agent`, set `CENTRAL_URL` once in
`provisioning/fleet.env` (an `ENROLL_TOKEN` there is optional), then
`provisioning/provision-sd.sh` (see `docs/second-pi-setup.md`). The Pi
self-registers on first boot; approve it once in the admin UI under
Printers → Pending. Nothing is configured per Pi.

Manual/scp path (existing Pi, or no card reader):
```bash
make pi-agent
scp bin/pi-agent-armv6 usb-refresh.sh pi/resin-pi-agent.service pi/install.sh \
    pi/config.example.env captain@<pi>.lan:~/agent-install/
ssh captain@<pi>.lan 'sudo bash ~/agent-install/install.sh ~/agent-install'
# edit /etc/resin-pi-agent.env — set CENTRAL_BASE_URL (ENROLL_TOKEN optional) — then:
# sudo systemctl restart resin-pi-agent
```
`install.sh` also retires the old Flask `printer-upload.service` if present.
`hw-tests/README.md` covers on-hardware validation.

## ⚠️ UPDATE 2026-08-21 — central hypothesis below is FALSIFIED, see "CONFIRMED
## WORKING" section further down before acting on anything in this block.

## THE CENTRAL KNOWN ISSUE (ORIGINAL HYPOTHESIS — SUPERSEDED, kept for history)
The M7 Pro shows a USB icon but WILL NOT READ FILES when the Pi runs Raspberry Pi
OS **Bookworm** (kernel 6.x). Root cause traced by elimination: it is NOT the disk
image format or the gadget parameters — those are byte-for-byte identical to a
known-working community project (adamoutler/Pi-Zero-W-Smart-USB-Flash-Drive),
which is CONFIRMED working on the M7 Pro using a Pi Zero 1 W on old Raspbian.

The only remaining variable is the KERNEL:
- Bookworm kernel 6.x  -> changed dwc2 gadget enumeration -> M7 Pro REJECTS it.
- Bullseye Legacy 5.x  -> original enumeration           -> M7 Pro ACCEPTS it.
- Elegoo Saturn tolerates either kernel (that's why Saturn "works" and masks this).

### Implication (SUPERSEDED — do not act on this, see update above)
If `uname -r` shows a 6.x kernel (Bookworm), the M7 Pro will very likely never
read the drive no matter what gadget tweaks we try. The fix is one of:
  (a) reflash the Pi to Raspberry Pi OS Lite **Legacy (Bullseye, 32-bit)**, OR
  (b) stay on Bookworm and downgrade the kernel to 5.x via `rpi-update <hash>`.
Check OS/kernel FIRST before spending time on gadget debugging.
**This did not hold up: see CONFIRMED WORKING below — all three printers read
files fine on kernel 6.12 (newer than Bookworm even). Do not reflash/downgrade
the kernel based on this section.**

## The working gadget recipe (verified approach)
```bash
# Bare FAT32 image, NO partition table:
sudo dd if=/dev/zero of=/piusb.bin bs=1M count=8192 status=progress
sudo mkdosfs /piusb.bin -F 32 -I -n RESINUSB
sudo fsck.fat -y /piusb.bin

# config.txt (Bookworm path /boot/firmware/config.txt; Bullseye /boot/config.txt)
# needs, under an [all] section:
dtoverlay=dwc2,dr_mode=peripheral        # dr_mode=peripheral matters on Zero 2 W
                                          # (it can default to USB host mode otherwise)

# Load the gadget:
sudo modprobe g_mass_storage file=/piusb.bin stall=0 ro=0 removable=1
```
To add/refresh files: `modprobe -r g_mass_storage`, mount the image on a loop,
copy files in, unmount, `fsck.fat -y`, then modprobe again. (Can't write to the
image while the printer has it mounted — same as ejecting before writing.)

## Things ALREADY TRIED that did NOT fix the M7 Pro (do not repeat)
- ~~MBR partition table + loop device instead of bare FAT32~~ **RETRACTED
  2026-08-21**: an MBR-partitioned image (with `g_mass_storage` pointed at the
  whole disk, partition auto-detected via `losetup -fP`) worked fine on all
  three printers in the confirmed run below. Whatever blocked this before, it
  wasn't the partition table by itself.
- Pointing g_mass_storage at a partition vs the whole disk
- `removable=1`, custom `iSerialNumber`
- libcomposite/configfs with SanDisk vendor IDs (0x0781 / 0x5571)
- `dr_mode=peripheral` alone (needed on Zero 2 W, but NOT sufficient on Bookworm)
The FAT "dirty bit" was a red herring — clear it with `fsck.fat -y`. NOTE:
`fsck.fat` must be run against the *partition* (e.g. `/dev/loop1p1`), not the
raw disk image, when the image has an MBR partition table — running it against
the whole `/piusb.bin` misreads the MBR as a bogus FAT boot sector and errors
out ("Currently, only 1 or 2 FATs are supported, not 251" or similar garbage).

## ✅ CONFIRMED WORKING (2026-08-21) — all three printers read files fine

**Result:** All three target printers showed their respective sliced file in the
on-screen file browser after being plugged into this Pi:
- Anycubic Photon Mono M7 Pro — showed `.pwsz`, user also confirmed it **loads**
  (opens/previews correctly, not just a filename in the list)
- Elegoo Saturn 3 — showed `.goo`
- Anycubic Photon P1 — showed `.pp1`

Scope note: confirmed = file appears and (for the M7 Pro) opens/loads OK. NOT
yet confirmed = an actual full print run to completion on any of the three.
Treat "prints successfully end-to-end" as still open.

### Exact environment this worked on
- OS: Raspbian GNU/Linux **13 (trixie)**, Debian base 13.4
- Kernel: **6.12.75+rpt-rpi-v7** — i.e. newer than the "Bookworm 6.x" the old
  hypothesis blamed, not older. The kernel-version theory is dead.
- Pi: Zero 2 W, inner micro-USB (OTG/data) port, powered from the printer.
- SSH reachable at `resin.lan` (not `resin.local` — mDNS doesn't resolve from
  this WSL2 client).

### Exact config that worked (NOT the "verified recipe" bare-FAT32 approach —
### this was the pre-existing image already on the Pi, MBR partition and all)
`/boot/firmware/config.txt` — still had the leftover/conflicting lines, never
cleaned up, and it worked anyway:
```
# This line should be removed if the legacy DWC2 controller is required
dtoverlay=dwc2,dr_mode=host
dtoverlay=dwc2,dr_mode=peripheral
```
(Both `host` and `peripheral` dr_mode lines present simultaneously. Untouched
for this whole test. Worth cleaning up eventually, but it is NOT what was
blocking the M7 Pro before — whatever that was, it's apparently not present
in this current setup/printer-firmware combination.)

`/piusb.bin`: 8 GiB, **MBR partition table** (partition type `0xc`, FAT32 LBA,
starting sector 2048) — i.e. exactly the format previously listed as "already
tried, did not work." `g_mass_storage` was pointed at the whole raw image, and
the partition inside it was accessed via `losetup -fP --show /piusb.bin` (which
auto-detects the partition and exposes it as e.g. `/dev/loop1p1`).

Gadget load command (unchanged from the original recipe):
```bash
sudo modprobe g_mass_storage file=/piusb.bin stall=0 ro=0 removable=1
```

Files present on the image simultaneously (all three coexisted on one FAT32
partition without issue — no need for separate images per printer):
```
3x_DancingRocky_v1_M7_SADG.pwsz   91,958,594 bytes
Roky_5in_P1_SUNLU_ABSDG.pp1       85,402,568 bytes
saturn_figure_1.goo              126,842,548 bytes
```

### Write workflow used (image was live/mounted by a printer at the time)
```bash
sudo modprobe -r g_mass_storage                 # unload — drive disappears from printer
sudo losetup -fP --show /piusb.bin              # -> e.g. /dev/loop1, exposes /dev/loop1p1
sudo mount /dev/loop1p1 /mnt/piusb_ro            # (mount point name is a leftover, it's RW here)
sudo cp <file(s)> /mnt/piusb_ro/
sync
sudo umount /mnt/piusb_ro
sudo losetup -d /dev/loop1
sudo modprobe g_mass_storage file=/piusb.bin stall=0 ro=0 removable=1   # reload — drive reappears
```
Skip `fsck.fat` on the raw `/piusb.bin` when it's MBR-partitioned — point it at
the partition device instead if you want to run it, otherwise it's optional.

### Practical operational notes learned this session
- Pi's `/tmp` is a **213 MB tmpfs** — filled up and silently truncated/failed
  scp transfers when staging multiple large sliced files there at once. Stage
  large files under `/home/captain/staging/` (real disk, ~16 GB free) instead.
- No `sshpass`/`expect`/`pexpect` available locally, and no root to install
  them. Password auth was scripted via a `pty.fork()`-driven Python wrapper for
  interactive SSH commands, and via `SSH_ASKPASS` + `SSH_ASKPASS_REQUIRE=force`
  (with `SSH_AUTH_SOCK` unset) for `scp`. Don't background/`setsid` an scp this
  way mid-transfer — it detaches before completion and races if reissued,
  corrupting the destination file. Let it block to completion (or run truly in
  background and poll/wait properly, not `setsid`).
- Always verify large transfers with `sha256sum` on both ends before trusting
  them — silent truncation happened twice this session (once from a
  setsid/backgrounding race, once from the tmpfs filling up mid-write).

## Immediate plan
~~1. SSH in; check `cat /etc/os-release` and `uname -r`.~~ DONE — Raspbian 13
   trixie, kernel 6.12.75+rpt-rpi-v7.
~~2-5. Get a file showing on all three printers.~~ **DONE 2026-08-21** — see
   "CONFIRMED WORKING" above. All three printers show/load their sliced file
   from the same Pi, same image, same (imperfect/leftover) config.

### Next up
1. Confirm an actual **full print to completion** on at least one printer
   (M7 Pro first, since it was the picky one) — "shows in browser" and "opens/
   loads" are confirmed, "prints" is not yet.
2. Decide whether to clean up the leftover `config.txt` (duplicate/conflicting
   `dwc2` dr_mode lines) and rebuild `/piusb.bin` as bare FAT32 per the old
   "verified recipe" — it may be unnecessary now given the confirmed run above,
   but it's still sloppy config worth resolving once things are stable.
3. **Central portal rewrite landed 2026-08-27** (Go service + Postgres + Helm +
   thin `pi-agent`). Not yet hardware-verified end-to-end: stand up the portal,
   create the M7 Pro printer + link Slack names + certify the known members,
   install `pi-agent` on `resin`, and run `hw-tests/` against the Pi + M7 Pro.

## Working style the user wants
- ONE action/instruction at a time; wait for confirmation before the next step.
- The user watches the printer screen; Claude Code drives the Pi.
