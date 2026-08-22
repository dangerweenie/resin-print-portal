# Resin Print Portal

A Raspberry Pi–based USB mass-storage emulator plus a Flask web portal, so
members of a makerspace can submit sliced files to resin 3D printers that
have no networking of their own — and so staff can see who's printing what,
gated behind a pre-print safety checklist.

One Pi serves one printer (a USB gadget can't split to multiple hosts), so
each deployment is a Pi + printer pair. See `CLAUDE.md` for the low-level
USB gadget technical detail and `resin_plans.md` for the full narrative —
what broke, what got fixed, and the reasoning behind current design
decisions. `second-pi-setup.md` is a step-by-step runbook for standing up a
new Pi from scratch.

## Layout

```
printer-upload/     the Flask app — this is almost certainly what you want
  app.py
  sliced_file_info.py   parses print-time/layer-count out of sliced files
  pure_aes.py            pure-Python AES (no crypto lib was available on
                          the target device) — needed to decrypt one .ctb
                          variant's metadata block
  templates/*.html
  deploy.sh           copies everything into place on the Pi and restarts
                       the systemd service
usb-refresh.sh        syncs the current print job onto the printer's USB
                       gadget image — lives one level up from the Flask
                       app since it's more device-level than app-level
sliced_scenes/         (gitignored) real sample files used for hands-on
                       format testing — not included in this repo
```

Nothing in this repo is deployed automatically — `printer-upload/deploy.sh`
copies files into `/opt/printer-upload` on the target Pi and restarts
`printer-upload.service`. There's no CI/CD here yet; changes get scp'd
over and deploy.sh run manually.

## Current status (see `resin_plans.md` for the full detail)

Working and deployed on the first Pi: USB gadget emulation (verified
against 3 of 4 target printer models), automatic print-time parsing for
every supported sliced-file format, member login gating the upload/print
flow, an admin-configurable pre-print safety checklist, print-job tracking
with a JSON status endpoint, and optional Slack webhook notifications.

Two real production bugs were found and fixed this build: uploaded files
were silently never reaching the printer (a partition-mount mismatch), and
none of the four target printers actually browse USB subfolders (fixed by
syncing only the currently-active print job's file, not the full upload
history).

## Where we'd love a hand: member registration

The member system right now is deliberately simple — a `members` SQLite
table, email + password auth, admin manually certifies each person via
`/admin/members`. That's fine for the current scale, but there's real
appetite to grow it:

- **A self-service registration queue** — a person requests an account,
  it lands `pending`, an admin approves it — instead of an admin typing in
  every person by hand.
- **A transitional shared/open account** for gradual rollout, disabled once
  everyone has an individual login. (Known trade-off: while it's active,
  print-job attribution and the safety-checklist audit trail go blank for
  whoever's using it.)
- **Eventually**: integration with the space's existing keyfob access
  system, and maybe further out, Active Directory or roaming-profile-style
  identity down the line.

That last one is the reason worth designing for now rather than later:
right now, `app.py`'s member auth is a single hardcoded check (email +
`werkzeug.security.check_password_hash` against the `members` table,
inline in the `index()` route). Before building the keyfob/AD path, it's
worth pulling that check behind a small seam — something like a single
`authenticate(identifier) -> Member | None` entry point that the login
route calls, so a keyfob reader or an LDAP/AD bind can become a second
implementation of the same contract instead of a parallel, divergent auth
path. Nothing about that needs building today — just flagging it as the
shape worth aiming for before the password-table assumption gets baked in
deeper.

## Local dev

No test suite or local dev environment exists yet — everything so far has
been developed and tested directly against the live Pi over SSH. `flask`,
`werkzeug`, and `gunicorn` are the only real dependencies (see
`printer-upload/deploy.sh`). Secrets (`settings.json`, `.secret_key`,
`.api_key`, `uploads.db`) are generated at first run and are gitignored —
they exist only on deployed devices, never in this repo.
