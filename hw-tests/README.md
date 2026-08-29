# On-hardware validation

The Go unit/integration suite (`go test ./...`, plus the store/worker tests
against a throwaway Postgres) needs no Pi and no real gadget. Everything in
**this** directory does: a real Pi running `resin-pi-agent.service` and the
real `g_mass_storage` gadget, talking to a reachable central portal.

## Runs from your laptop, not the Pi

Every script here (`*-against-pi.sh`) runs **on your dev machine** and drives
the Pi over SSH — you never `ssh` in and run them there. This keeps the
system under test unmodified:

- Anything that needs extra tooling (building a FAT32 / MBR-partitioned
  fixture image needs `mkdosfs`/`parted`/`losetup`; driving the agent and
  portal over HTTP needs `curl`; the synthetic `.goo` fixture needs
  `python3`) runs **on your laptop**.
- Every command sent to the Pi uses only what the production deploy already
  requires there: `losetup`/`mount`/`umount`/`modprobe`/`lsmod`/`dd`/
  `sha256sum` (`usb-refresh.sh` already depends on all of these) and
  `systemctl` (needed for the two systemd units to run at all). No Python,
  no `sqlite3`, no app venv on the Pi any more — the Pi agent is a single
  static Go binary and holds no local state.
- Laptop prerequisites: `ssh`, `scp`, `curl`, `sha256sum`, `python3` (all
  scripts), plus `mkdosfs`, `parted`, `losetup` for the Layer 1 fixture
  images (needs local root — expect a `sudo` prompt on your own machine for
  that step).
- The Pi needs nothing added — a failing preflight check is a real gap in
  how the Pi was provisioned (`docs/second-pi-setup.md`), not something to
  `apt install` past.

## The dedicated test rig: `resin1`

Point every script at a Pi provisioned specifically as a test rig, hostname
`resin1` — never `resin` (serving the M7 Pro) or any Pi in service. These
scripts cycle the USB gadget repeatedly and create a real `print_jobs` row +
`decision_log` entries in the central database. Each script SSHes in, asks
the Pi its hostname, and makes you type it back before doing anything; don't
route around that with `--yes` against the wrong host.

Set it up per `docs/second-pi-setup.md` (Pi Zero W, fleet-standard):
```bash
sudo ./provisioning/provision-sd.sh --device /dev/sdX --image ~/Downloads/raspios-lite.img.xz \
  --hostname resin1 --password '<pick a password>' \
  --wifi-ssid 'SiteWifi' --wifi-password '<wifi password>'
```
That path is flagged **NOT YET HARDWARE-VERIFIED** in `docs/second-pi-setup.md`
— bringing up `resin1` this way doubles as its first real verification (see
`MANUAL-CHECKLIST.md` §3).

You also need, in the central portal (once):
- a **printer** whose `slug` matches `resin1`'s `PRINTER_SLUG`;
- a **member** with a linked Slack name, **certified** for that printer;
- that printer's **API key** (for the central-side assertions in Layer 2 —
  the agent already has its own copy in `/etc/resin-pi-agent.env`).

Plug `resin1` into whichever printer is free — the Elegoo Saturn 3 is the
best default for frequent runs (tolerant of rapid disconnect/reconnect).
Re-run the manual checklist against the M7 Pro periodically.

## Authentication

Each script opens one multiplexed SSH connection for its whole run (one
login-password prompt), then uses `sudo -S` — password piped, no pty — for
anything root. Expect a few sudo-password prompts over a run; `remote_setup`
in `lib/remote.sh` asks once up front and reuses it.

## The four layers

| Layer | What | Printer attached? | Script |
|---|---|---|---|
| 1 | `usb-refresh.sh`'s `<file>` / `--clear` contract + mount-detection against the real gadget, using disposable scratch images built locally | No | `test-usb-refresh-against-pi.sh` |
| 2 | The real member flow end-to-end: upload via the agent → central approves → **file actually exposed on the gadget** (byte-checked) → central shows the job → finish → **file actually comes off** | No (verified via loopback mount, not a printer screen) | `test-full-flow-against-pi.sh` |
| 3 | `resin-pi-agent.service` crash/recovery on real systemd; confirms the gadget is untouched by an agent crash | No | `test-service-resilience-against-pi.sh` |
| 4 | Everything needing eyes on a printer screen or a physical power cycle/replug | **Yes** | `MANUAL-CHECKLIST.md` |

Run order 1 → 2 → 3 (quick); Layer 4 after gadget/lifecycle changes and
periodically regardless.

### Layer 1 — `test-usb-refresh-against-pi.sh`
Re-validates `usb-refresh.sh` against the real driver: bare-FAT32 vs
MBR-partitioned mount handling, the regression test for the original bug (a
mount failure must never orphan the gadget), and the `--clear` path. Fixture
images are built locally (`mkdosfs`/`parted`/`losetup`, local `sudo` prompt)
and shipped up via `scp`.

```bash
bash hw-tests/test-usb-refresh-against-pi.sh --host resin1.lan --yes
```
Env override: `SCRIPT_UNDER_TEST` — a local path to a working-tree
`usb-refresh.sh` to test instead of the one installed on the Pi.

### Layer 2 — `test-full-flow-against-pi.sh` (the important one)
The direct answer to "does a file actually reach the printer when a member
uploads it, and actually leave when they're done." It:

1. checks the agent's upload page responds and the central `/config` is
   reachable for the slug;
2. builds a tiny synthetic `.goo` locally (valid `V3.0` header + a nonzero
   `print_time`);
3. `POST`s it to the agent's `/submit` with the checklist ticked, as the
   certified test member's Slack name;
4. asserts the central `/current-job` now shows `hwtest.goo` and parsed its
   ETA as exact;
5. unloads the real `/piusb.bin` over SSH, mounts it, and asserts it holds
   exactly `hwtest.goo`, byte-identical (sha256) to what was uploaded;
6. `POST`s `/finish` and asserts the gadget is cleared.

```bash
bash hw-tests/test-full-flow-against-pi.sh \
  --host resin1.lan --central https://portal.tinkermill.org \
  --slug resin --api-key <printer key> --slack-name 'test.member' --yes
```

Leaves one real `print_jobs` row (ended) + `decision_log` entries under the
test member — expected, and the kind of trail `/admin/log` is meant to show.

### Layer 3 — `test-service-resilience-against-pi.sh`
SIGKILLs the real `resin-pi-agent.service` and confirms systemd's
`Restart=on-failure` brings it back and the upload page answers HTTP again —
and, the part that matters most, that the **USB gadget never flinches**: the
printer keeps whatever drive it has regardless of whether the agent or the
central portal is up. Doesn't reboot the Pi; full boot-recovery is Layer 4.

```bash
bash hw-tests/test-service-resilience-against-pi.sh --host resin1.lan --yes
```

### Layer 4 — `MANUAL-CHECKLIST.md`
Printer-screen confirmation per model, full print-to-completion, reboot
recovery, power-cycle resilience, physical USB replug, filename edge cases.

## Before running any of this

- Run from a normal (non-root) shell on your laptop — only Layer 1's
  `mkdosfs`/`losetup` step needs local root, and it prompts for `sudo`
  itself.
- Layers 1–2 briefly disconnect/reconnect whatever's plugged into the Pi's
  USB port several times — inherent to `g_mass_storage`. Never run them
  during a real print anyone relies on (which should only ever be `resin1`).
- Layer 1 scratch images live under `/home/<user>/hw-test-scratch` on the Pi
  (real disk, not `/tmp` — a 213MB tmpfs, per `CLAUDE.md`). Cleaned up on
  exit, including on failure/Ctrl-C.
- All scripts accept `--host` (default `resin1.lan`) and `--user` (default
  `captain`).
