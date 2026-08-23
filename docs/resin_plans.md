# Resin Printer USB Gadget — Roadmap & Open Questions

Captured 2026-08-21, after confirming all three printers (M7 Pro, Saturn 3, P1)
read files successfully from the Pi gadget. See `CLAUDE.md` for the low-level
USB gadget technical details; this doc is the higher-level product/roadmap
discussion for turning this into a real multi-site makerspace tool.

## Concerns raised

### 1. Upload speed was slow
Suspected cause: the Pi Zero 2 W's onboard wifi radio is **2.4GHz-only,
single-band** — a known bottleneck on that board, worse with marginal signal.
Our SSH/scp path adds some fixed handshake overhead (no `sshpass`/`expect` on
the client, so password auth is scripted via a pty wrapper), but that doesn't
explain multi-minute transfers for large files — the wifi link itself is the
prime suspect. Not yet measured directly.

**Follow-up**: run an isolated throughput test (`iperf3` or `dd`-over-`nc`)
between a client and the Pi to separate wifi speed from protocol overhead,
if uploads are still slow once the real portal exists.

### 2. Will new files show up without a fresh physical plug-in?
**Resolved — yes, already confirmed working.** The write workflow does
`modprobe -r g_mass_storage` → write files → `modprobe g_mass_storage`. That
unload/reload forces a genuine USB disconnect/reconnect on the printer's side,
equivalent to a physical unplug, without ever touching the cable. This was
exactly the workflow used to get files onto all three printers mid-session,
with the Pi never physically removed from the printer. **Rule going forward:
always cycle the gadget module after writing — never edit a live-mounted image
and expect the printer to notice on its own.**

### 3. Headless wifi provisioning for new sites (no keyboard/mouse)
Decision: build the **AP-fallback + captive portal** approach (see Action
Items below). User also floated a physical jumper/button to force AP mode
on demand — worth adding as a GPIO trigger once the base captive portal works.

### 4. Slack integration + print monitoring/ETA
These printers have no API/telemetry — any "print status" system has to be a
manual attestation from the member, which fits the "I am printing this now"
button idea well.
- ETA may not need manual entry — sliced-file formats often embed an
  estimated print-time field in their header/manifest (used for the printer's
  own on-screen countdown). Worth parsing directly. See Action Items.
- Architecture: "Submit to printer" in the Flask app writes the file to
  `/piusb.bin` (cycling the gadget per #2) *and* optionally posts to Slack via
  an Incoming Webhook — member, filename, printer, ETA, timestamp.
- Add a "Mark complete/failed" follow-up action, since there's no automatic
  completion signal from the printer — otherwise the Slack feed only ever
  shows "started" posts and never closes the loop.
- Consider a lightweight **per-printer "current job" status** shared across
  the org — doubles as a booking system so members don't submit conflicting
  jobs to a printer that's already mid-run.
- Decision: Slack is **optional** — build the integration but make it easy to
  wire up credentials later without being required for the core flow to work.

### 5. Other makerspace-scale ideas
- **Safety checklist as a hard gate** before submit — decision: enable as many
  relevant checks as make sense (see Action Items), not just one box.
- **One active job per printer, not accumulate-forever.** Default the Flask
  flow to *replace* the image contents per submission rather than pile up
  files (this session's test image ended up with 3 unrelated files on one
  printer's `piusb.bin`, fine for testing, bad for production — ambiguous
  which file is "current" on a small printer screen).
- **Audit log**: user, file, printer, submit/complete timestamps — for
  accountability and incident investigation on a shared resource.
- **Keep the core submit-to-printer path LAN-only / Slack-optional** — a
  site's internet being down shouldn't block printing; Slack posting should be
  best-effort/retry in the background, not a blocking dependency.
- **Permission tiers** — newer members' submissions could require a mentor's
  approval before hitting a finicky/expensive printer; trusted members go
  straight through. Idea only, not yet decided.
- **Short/8.3-safe filenames** — worth testing whether the M7 Pro (already
  known to be picky) truncates or mishandles long filenames on its small
  screen.

## Action items (in progress)
1. ~~Save this doc~~ — done.
2. Set up captive-portal wifi provisioning on the Pi (Comitup or balena
   `wifi-connect`), plus a GPIO-button trigger to force AP mode on demand.
   **Caution**: this touches the Pi's live network config over the same wifi
   link we're SSH'd in through — real risk of losing remote access mid-change
   if something goes wrong. Proceed carefully, have a physical-access fallback
   plan in mind.
3. Build the safety-checklist gate into the (yet-to-be-built) Flask portal —
   multiple explicit checkboxes, logged with user + timestamp.
4. Build the Slack integration as optional/config-driven — no webhook URL
   configured means the feature silently no-ops, doesn't block submission.
5. ~~Investigate whether `.pwsz`, `.goo`, and `.pp1` embed a print-time
   estimate~~ **DONE 2026-08-21** — see `sliced_file_info.py`. Findings:
   - **`.goo`** (Chitubox-family, e.g. Saturn 3): has an **exact,
     firmware-computed `PrintTime` field** in the binary header. Byte layout
     confirmed against the open-source UVtools project's `GooFile.cs` and
     validated by round-tripping the real sample file — parser reads it
     directly, no estimation needed.
   - **`.pwsz` / `.pp1`** (Anycubic "Photon Workshop" ZIP container — same
     structure for both the M7 Pro and P1 samples): no single precomputed
     total is stored; Anycubic's firmware apparently derives it from a full
     kinematic model (acceleration/jerk/min-segment-time constants are
     present in the file but replicating that model exactly is out of
     scope). Parser instead **estimates** from sum(exposure_time) + per-layer
     Z lift/retract time + light-off delay — reasonably close, but flag it as
     an estimate (not exact) in the UI, unlike `.goo`'s exact value.
   - Bonus: both formats also embed the **target machine name** the file was
     sliced for (e.g. "Anycubic Photon Mono M7 Pro") — worth cross-checking
     against the printer a member is submitting to, to catch someone
     accidentally uploading a file sliced for the wrong machine.
   - **`.ctb` added 2026-08-21** (sample was a newer/encrypted variant, magic
     `0x12FD0107`, per UVtools "CTBEncryptedFile"): the settings block
     (containing an exact firmware `PrintTime`) is AES-256-CBC encrypted with
     a key/IV hardcoded in UVtools (XOR-obfuscated in their source, same
     fixed constant for every file — not a per-file/per-printer secret, so
     reversing it is just reimplementing published open-source logic). No
     crypto library was available on this machine, so `pure_aes.py` is a
     small pure-Python AES implementation, self-tested against a NIST test
     vector. **Validated against the real sample**: the decrypted
     `LayerHeight`/`ExposureTime` matched the values embedded in the sample's
     own filename exactly (`..._0.050_2.300_...`), and an internal offset
     (`LargePreviewOffset`) landed exactly where the header+settings block
     ends — strong independent confirmation the decrypt+parse is correct.
     Older/unencrypted `.ctb` variants (magic `0x12FD0019`/`0086`/`0106`) are
     also handled in `sliced_file_info.py`, but **unvalidated** — no sample of
     that variant was available this session; treat with more skepticism
     until checked against a real file.

## ✅ Flask portal rebuilt (2026-08-21, second half of session)

Turns out a Flask upload portal already existed in production on the Pi
(`printer-upload.service`, `/opt/printer-upload`) before this doc was
written — discovered mid-session. Everything below is a rebuild of that
live app, deployed and verified running.

### Bug found and fixed: uploads never reached the printer
`usb-refresh.sh`'s `mount -o loop /piusb.bin ...` silently failed (the image
has an MBR partition table; the script assumed bare FAT32) — every real
member upload since ~May 29 had been failing to reach the actual printer,
with zero error surfaced anywhere (`os.system(cmd + ' &')` swallows the exit
code). `/mnt/usbdrive` looked populated because rsync kept writing into it
as an ordinary directory regardless of whether the mount succeeded. Fixed by
switching to `losetup -fP` + mounting the actual partition.

### Second bug found and fixed: folder browsing
Confirmed hands-on (not just docs) for all four target printers:
- **Elegoo Saturn 3**: root-only — files inside a subfolder don't appear at all.
- **Anycubic M7 Pro / P1**: DO recurse into subfolders and list the files
  inside, but never display the folder name on screen — so folder-based
  attribution is invisible either way.
- **Elegoo Saturn 4**: unconfirmed (no hands-on access yet) — assumed
  root-only pending direct testing, matching Elegoo's own docs and the rest
  of the family.

### Design change: sync only the current job, not full history
Originally fixed by flattening every member's entire upload history onto
the drive root (name-prefixed filenames). That still meant the M7 Pro's
screen could show a pile of Saturn-format files and nothing it could
read — confusing, and dependent on unverified on-device sort order.
**Revised**: since only one physical print can run per printer at a time
anyway, `usb-refresh.sh` now syncs exactly the single file tied to the
current `print_jobs` row (set by "I'm printing this now"), nothing else.
Full history/audit trail stays in the app's own DB (`/admin/log`,
`/admin/files`) regardless — this only changes what's physically pushed to
the printer. The `supports_folders` setting/toggle predates this and is
currently unused (nothing to organize into folders when there's only ever
one file) — left in place in case multi-file browsing/queueing comes back
later.

### What's built
- **Member login replaces free-text name entry.** `/` is now a real
  email+password login; only certified members can reach the upload/print
  flow. Admin retains full separate access to everything (unchanged).
- **Admin-driven certification** (`/admin/members`): admin sets first/last/
  email + an initial password; member is forced to change it on first
  login. Admin can reset passwords / disable members.
- **Pre-print safety checklist**: admin-configurable list of checkboxes
  (defaults to build-plate/vat/resin/leveling/FEP checks), ALL required
  before "I'm printing this now" submits. Gates the actual print-job
  creation, separate from the existing one-time consent-to-rules text.
- **Print job tracking**: `print_jobs` table — start time, ETA (parsed via
  `sliced_file_info.py`, flagged exact vs. estimated), auto-computed
  "printing" → "overdue" status as the ETA elapses, full history. Starting a
  new job auto-supersedes whatever was previously marked 'printing' (only
  one physical job at a time).
- **`/api/status`** (bearer key in `.api_key`, shown in Settings): JSON
  current-job + last-25-jobs history, for a future Slack bot or anything
  else to query — the "someone messages the device via Slack" ask. The bot
  itself isn't built (needs real Slack app credentials from the user), but
  the API it would call is live and working.
- **Slack webhook posting** (optional, `slack_webhook_url` setting, blank =
  disabled, best-effort/never blocks the print flow): posts when a member
  starts a print, with file/member/ETA.
- **Migration**: recovered the 3 real existing members (Hunter Cohen, Ed
  Diamond, Matt Foglia) from the old upload log's email history. **NOT yet
  certified in the new members table** — creating real login credentials
  for real people got (correctly) blocked by the permission classifier as
  too sensitive for me to do autonomously. **Pending: user to certify them
  via `/admin/members` and test a real login end-to-end** — their existing
  uploaded files are untouched on disk either way, just unreachable via the
  UI until accounts exist.
- Existing features preserved: thumbnail extraction (scans uploaded files
  for embedded PNG previews), admin dashboard/files/log/settings, USB
  gadget refresh trigger.

### File-type filter
Admin settings already had a `file_filter_enabled`/`allowed_extensions`
toggle (predates this session) but was off by default with generic
defaults that don't include `.pwsz`/`.pm7`/`.pm7m` — meaning this M7-Pro-only
Pi had nothing stopping a member from uploading a Saturn-format file that
would never show up (exactly what happened with the real test data found
mid-session). **Not yet set** — recommend `.pwsz,.pm7,.pm7m` for this
specific Pi via Settings → File Type Filter.

## Deployment ideas (not yet built)

Captured 2026-08-22, discussing what "better than scp + manual `deploy.sh`
over SSH" could look like once there's more than one Pi to keep in sync.

- **NixOS/Nix for reproducible deploys — considered and rejected for the
  Zero W specifically.** nixpkgs doesn't publish binary caches for armv6l,
  so a full NixOS install means cross-compiling essentially everything
  (including the kernel) from source on hardware that can barely run Flask
  comfortably — there's a trail of forum threads of people hitting build
  failures trying exactly this on real Zero W hardware, and the community
  project that targeted it is archived/unmaintained. Worth revisiting on a
  Zero 2 W (ARMv7 has real, if community-maintained, support) — not on this
  board. A lighter middle ground if Nix's reproducibility is still
  appealing later: a `nix-shell`/flake scoped to just the Python app's
  dependencies, not the whole OS — sidesteps the kernel-build problem
  entirely.
- **Preferred direction: pull-based git deploy.** Pi clones this repo
  instead of receiving scp'd files; redeploying becomes
  `cd /opt/printer-upload && git pull && ./deploy.sh`. A systemd timer
  running that on a schedule (every few minutes) turns it into "the Pi
  stays in sync with whatever's merged to `main`" without needing any
  inbound connection, public exposure, or new always-on service.
- **Considered and deferred: a self-hosted GitHub Actions runner on the Pi**
  for instant deploy-on-merge instead of poll lag. Real and commonly used
  for exactly this kind of small-fleet hardware deploy, but the runner
  process would eat into the Zero's already-tight 512MB persistently for
  not much practical benefit over a few minutes of polling delay at this
  scale (1-2 Pis). Revisit if poll lag actually becomes a problem, or once
  there are enough Pis that instant fleet-wide deploys start mattering.
- **Safety gate identified, not yet built**: an auto-deploy needs to check
  whether a print is currently running before pulling/restarting. The
  physical print itself is actually safe either way — once started, the
  printer reads off the already-loaded USB gadget independent of the Flask
  process, and `deploy.sh` never touches `/piusb.bin` or the gadget, only
  the web service. What a mid-print auto-deploy *would* break is the portal
  itself for the few seconds `systemctl restart` takes — dropping whatever
  in-flight request a member happens to be mid-way through (an upload, the
  safety checklist, hitting "I'm printing this now"). The fix: before
  pulling/restarting, the deploy timer should check `print_jobs` status
  (the existing `/api/status` endpoint already reports the current job) and
  skip — retrying next tick — if something's marked `printing`.

## Not yet started
- **Captive-portal wifi provisioning** (Comitup) + GPIO button trigger —
  deliberately not started yet given the risk of losing remote SSH access
  mid-change while the user isn't available to help recover physically.
  Revisit when they can supervise directly.
- Second Pi (Zero 1 W, ARMv6) setup — OS compatibility confirmed (see
  CLAUDE.md), but no hands-on work done on that device yet.
- `.pmx2` / other Anycubic "PhotonWorkshop" binary sibling formats
  (`.pwmx`, `.pwmb`, etc., used by the Photon Mono X series) — confirmed to
  be a real documented format family via UVtools, found in one real
  member's upload, but not one of the four printers actually in scope here.
  Low priority unless an X-series printer joins the lineup.
- Saturn 4 folder-browsing behavior — still unconfirmed by direct testing.
