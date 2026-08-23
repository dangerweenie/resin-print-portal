# On-hardware validation

`test-usb-refresh-on-pi.sh` re-validates `usb-refresh.sh` against the real
`g_mass_storage` USB gadget driver, on the actual Pi. It is **not** run as
part of the off-hardware test suite (`printer-upload/tests/`) — nothing in
this repo executes it automatically, and it isn't run in this session either.
**Run it yourself, on the Pi, and report back the PASS/FAIL summary.**

## Before running

- Must run **as root**, over SSH is fine: `sudo bash hw-tests/test-usb-refresh-on-pi.sh`
- Needs `dosfstools` and `parted`: `sudo apt install -y dosfstools parted` if
  the script's preflight check complains they're missing.
- **It will briefly disconnect and reconnect the printer's USB drive several
  times over ~30–60 seconds** — that's inherent to `g_mass_storage` (only one
  gadget instance at a time), not a bug in the test. Run it **between
  prints**, never while one is in progress. It prompts for confirmation
  before doing anything; pass `--yes` to skip that for scripted reruns.
- Scratch images/DBs live under `/opt/printer-upload/hw-test-scratch` (real
  disk, not `/tmp` — `CLAUDE.md` documents the Pi's `/tmp` as a 213MB tmpfs
  that has silently truncated large writes before). Cleaned up automatically
  on exit, including on failure or Ctrl-C.
- By default it tests the already-deployed `/usr/local/bin/usb-refresh.sh`.
  To test local changes before running `deploy.sh`, point it at your
  working-tree copy instead:
  `SCRIPT_UNDER_TEST=/path/to/usb-refresh.sh sudo -E bash hw-tests/test-usb-refresh-on-pi.sh`

## What it checks

| Sub-test | What it validates |
|---|---|
| **A — bare FAT32** | The layout `second-pi-setup.md` recommends for a fresh Pi (no partition table). This is the code path that used to be entirely broken. |
| **B — MBR-partitioned** | The layout the first Pi's existing image actually uses (confirmed working in `CLAUDE.md`). |
| **C — mount failure** | The most direct regression test for the original bug: point the script at an unmountable image, confirm it exits nonzero *and* the gadget module is still loaded afterward. Before the fix, a mount failure left the printer permanently disconnected until someone noticed by hand. |

Each sub-test prints `PASS: ...` or `FAIL: ...` as it runs, followed by a
full summary and a nonzero exit code if anything failed — paste that summary
back for review.

Expected total runtime: well under a minute.
