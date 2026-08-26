# On-hardware validation

Nothing in here runs as part of the off-hardware suite (`printer-upload/tests/`,
run via `.venv/bin/pytest printer-upload/tests/`) — that suite needs no Pi,
no root, and no real gadget. Everything below **does** need a real Pi
running the real `g_mass_storage` gadget.

## Runs from your laptop, not the Pi

Every script here (`*-against-pi.sh`) runs **on your dev machine** and
drives the Pi over SSH — you never `ssh` in and run them there yourself.
This is deliberate: earlier versions ran on the Pi and needed
`dosfstools`/`parted`/`curl`/`sqlite3` installed on it first, which means
changing the software makeup of the system under test just to test it —
exactly the kind of contamination you don't want. Now:

- Anything that needs extra tooling (building a FAT32 or MBR-partitioned
  fixture image needs `mkdosfs`/`parted`/`losetup`; driving the app over
  HTTP needs `curl`) runs **on your laptop**. Needing those installed on
  your own machine is normal dev setup — no different from already needing
  `ssh`/`scp`/`pytest` here — and has nothing to do with the Pi.
- Every command actually sent to the Pi uses only tools the production
  deploy already requires there: `losetup`/`mount`/`umount`/`modprobe`/
  `lsmod`/`dd`/`sha256sum` (core util-linux/coreutils/kmod — `usb-refresh.sh`
  itself already depends on all of these), `systemctl` (already needed for
  the two systemd units to run at all), and the **app's own already-installed
  venv python3** for any DB/JSON work (`sqlite3` is Python stdlib — no
  separate `sqlite3` CLI package needed on the Pi). See `lib/remote.sh` for
  the exact mechanics and the full reasoning.
- Laptop prerequisites: `ssh`, `scp`, `curl`, `sha256sum`, `python3` (all
  scripts), plus `mkdosfs`, `parted`, `losetup` (Layer 1 only, for building
  its two fixture images — this needs local root, expect a `sudo` prompt on
  your own machine for that step specifically).
- The Pi needs nothing added — if a script's preflight check complains
  something's missing or inactive there, that's a real gap in how the Pi
  was provisioned (see `docs/second-pi-setup.md`), not something to
  `apt install` your way past.

## The dedicated test rig: `resin1`

Point every script at a **Pi provisioned specifically as a test rig**,
hostname `resin1` — never at `resin` (serving the M7 Pro) or any other Pi
actually in service. These scripts disconnect/reconnect the USB gadget
repeatedly and write real rows into a real `uploads.db`; that's fine on a
rig nobody depends on, not fine on a printer someone's mid-print on. Each
script SSHes in, asks the Pi its own hostname, and makes you type it back
to confirm before doing anything — the same pattern
`provisioning/provision-sd.sh` uses before it erases an SD card. Don't
route around that check with `--yes` against the wrong host.

Set it up the normal way, following `docs/second-pi-setup.md` (Pi Zero W —
this is a new Pi, so it's fleet-standard, not the first Pi's Zero 2 W):
```bash
sudo ./provisioning/provision-sd.sh --device /dev/sdX --image ~/Downloads/raspios-lite.img.xz \
  --hostname resin1 --password '<pick a password>' \
  --wifi-ssid 'SiteWifi' --wifi-password '<wifi password>'
```
Worth knowing going in: that provisioning path is flagged
**NOT YET HARDWARE-VERIFIED** in `docs/second-pi-setup.md`. Bringing up
`resin1` this way doubles as the first real hardware verification of it —
see `MANUAL-CHECKLIST.md` §3, and update that doc's status note once it's
confirmed working.

Plug `resin1` into whichever printer is free — the Elegoo Saturn 3 (lenient,
per `CLAUDE.md`) is the best default for frequent automated runs, since it
tolerates being disconnected/reconnected rapidly without fuss. Periodically
re-run at least the manual checklist against the M7 Pro too (the picky one)
before trusting a change that touches the gadget path.

## Authentication

Each script opens one multiplexed SSH connection for its whole run (you're
prompted once for the login password, not once per remote command), then
uses `sudo -S` — password piped in, no pty — for anything needing root.
Because `sudo`'s cache is per-tty and no pty is allocated, expect a handful
of sudo-password prompts over a run, not just one; that's normal, not a
bug. `remote_setup` in `lib/remote.sh` asks for that password once up front
and reuses it for every `sudo` call after.

## The four layers

| Layer | What | Needs a printer physically attached? | Script |
|---|---|---|---|
| 1 | `usb-refresh.sh`'s mount-detection logic against the real gadget, using disposable scratch images built locally | No | `test-usb-refresh-against-pi.sh` |
| 2 | The real member-facing flow end-to-end: upload → print → **file is actually exposed** → finish → **file actually comes off** → supersede → admin force-clear → concurrency/leak checks | No (drive contents are verified via loopback mount, not by reading a printer's screen) | `test-full-flow-against-pi.sh` |
| 3 | `printer-upload.service` crash/recovery on real systemd; confirms the gadget is untouched by an app crash | No | `test-service-resilience-against-pi.sh` |
| 4 | Everything that needs eyes on a printer screen or a physical power cycle/replug | **Yes** | `MANUAL-CHECKLIST.md` |

Run order: 1 → 2 → 3 are quick (well under a few minutes combined) and
worth running after any change touching the gadget, the print-job lifecycle,
or the deploy path. Layer 4 is slower and printer-dependent — run it after
gadget/lifecycle changes and periodically regardless.

### Layer 1 — `test-usb-refresh-against-pi.sh`
Re-validates `usb-refresh.sh`'s bare-FAT32 vs MBR-partitioned mount handling,
plus the specific regression test for the original bug (a mount failure must
never orphan the gadget), against the real driver. The two fixture images
are built locally (needs `mkdosfs`/`parted`/`losetup` on your laptop, with a
local `sudo` prompt for the loop-device step) and shipped up via `scp`;
doesn't touch the deployed app.

```bash
bash hw-tests/test-usb-refresh-against-pi.sh --host resin1.lan --yes
```
Env override: `SCRIPT_UNDER_TEST` — a **local** path to a working-tree
`usb-refresh.sh` to test before running `deploy.sh` (copied up to scratch
and run from there). Defaults to the already-deployed
`/usr/local/bin/usb-refresh.sh` on the target Pi.

### Layer 2 — `test-full-flow-against-pi.sh` (the important one)

This is the direct answer to "does a file actually get exposed to the
printer when a member uploads and prints it" — and its mirror, "does it
actually stop being exposed once the print is done." It drives the real
deployed app over HTTP **from your laptop** (curl against `http://resin1.lan/`,
cookie-jar login) as a dedicated `hwtest@resin1.invalid` member account,
then verifies the real `/piusb.bin` over SSH by briefly unloading the real
gadget, mounting it, checking contents, and reloading — the same by-hand
sequence `CLAUDE.md` documents, just scripted and always followed by a
reload.

What it covers:
1. **Exposure** — upload a file, start a print, confirm it's on the drive
   the gadget presents, byte-for-byte (sha256, compared locally against the
   file this script generated before uploading it).
2. **Completion handling** — three distinct ways a print stops being
   "printing," each tagged with its own `end_reason` in `print_jobs` (see
   the admin log's "Ended by" column):
   - member clicks "mark finished" themselves → drive clears,
     `end_reason='member_finished'`
   - someone starts a *different* print without ever finishing the first →
     drive ends up holding only the new file, old one tagged
     `end_reason='superseded'`
   - staff force-clears from the admin dashboard → drive clears,
     `end_reason='admin_cleared'`
3. **Concurrency/race safety** — three near-simultaneous print-start
   requests fired from the laptop (a real network race between "browsers,"
   not a loopback loop), checking the system settles into one consistent
   state (one `printing` row, gadget loaded, no leaked loop devices, drive
   contents match the DB). **This one may legitimately fail** —
   `trigger_usb_refresh()` launches `usb-refresh.sh` with no locking around
   overlapping invocations, so a double-click or two people posting at once
   is a real, currently-unguarded race. A failure here is a finding (add a
   lock around the refresh), not a broken test.
4. **Resource leak check** — 5 repeated upload/print/finish cycles, then
   confirms no dangling loop devices or stale mounts accumulated.

```bash
bash hw-tests/test-full-flow-against-pi.sh --host resin1.lan --yes
```
Env overrides: `PRINTER_UPLOAD_BASE` (default `/opt/printer-upload`),
`ADMIN_PASSWORD` (falls back to reading `$BASE/.initial_admin_password` on
the Pi if that file still exists).

Test data note: this leaves real rows in `resin1`'s `uploads.db` under an
"Automated Hwtest" member/folder — expected, and even useful, since it's
exactly the kind of member-confirmed/superseded/staff-cleared trail
`admin/log` is meant to surface (just synthetic instead of from a real
member). Nothing here needs a printer plugged in to pass — it validates the
filesystem the gadget presents, not what a printer's firmware does with it.

### Layer 3 — `test-service-resilience-against-pi.sh`
SIGKILLs the real `printer-upload.service` (over SSH) and, from your laptop,
confirms systemd's `Restart=always` actually brings it back and the app
answers HTTP again — and, the part that matters most here, confirms the
**USB gadget never flinches**: the printer must keep whatever drive it
already has regardless of whether the web portal is up. Doesn't reboot the
Pi (an SSH session can't reliably survive triggering its own host's
reboot); full boot-recovery is Layer 4 §3.

```bash
bash hw-tests/test-service-resilience-against-pi.sh --host resin1.lan --yes
```

### Layer 4 — `MANUAL-CHECKLIST.md`
Printer-screen confirmation per model, full print-to-completion, reboot
recovery, real power-cycle resilience (the Pi has no graceful shutdown in
normal use — see that file's §4 for why this is routine, not an edge case),
physical USB replug, and filename edge cases. Needs a human watching a
printer, per `CLAUDE.md`'s established working style.

## Before running any of this

- Run these from a normal (non-root) shell on your laptop — none of them
  need local root except the specific `mkdosfs`/`losetup` step inside
  Layer 1, which prompts for `sudo` itself when it gets there.
- Layers 1–2 **will briefly disconnect and reconnect whatever's plugged into
  the Pi's USB port** several times — inherent to `g_mass_storage` (only one
  gadget instance at a time), not a bug. Never run these while a real print
  is in progress on a printer anyone's relying on — which, again, should
  only ever be `resin1` in the first place.
- Layer 2 defaults to testing the **deployed** app/gadget on `resin1`. To
  test local changes before running `deploy.sh`, deploy to `resin1` first
  (it's a test rig — that's what it's for), or point Layer 1 specifically
  at a working-tree copy via `SCRIPT_UNDER_TEST=/path/to/usb-refresh.sh`.
- Scratch images/DBs for Layer 1 live under
  `/opt/printer-upload/hw-test-scratch` on the Pi (real disk, not `/tmp` —
  `CLAUDE.md` documents the Pi's `/tmp` as a 213MB tmpfs that has silently
  truncated large writes before). Cleaned up automatically on exit,
  including on failure or Ctrl-C.
- All three scripts accept `--host <hostname or IP>` (default `resin1.lan`)
  and `--user <ssh user>` (default `captain`).
