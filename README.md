# Resin Print Portal

Members submit sliced files to resin 3D printers that have no networking of
their own. A Raspberry Pi bolted to each printer emulates a USB drive the
printer reads from; a central service decides who is allowed to print.

## Architecture

```
                                   ┌──────────────────────────────────────┐
   member's laptop/phone           │  central portal  (Go, in Kubernetes) │
        │ upload page              │                                      │
        ▼                          │  cmd/portal server  — Pi API + admin  │
┌───────────────┐   HTTPS          │  cmd/portal worker  — roster sync ────┼──▶ TinkerAccess
│  Pi (pi-agent)│ ───────────────▶ │  Postgres (Helm/PVC) — members,       │    get_users
│  USB gadget   │                  │    printers, certs, jobs, audit log   │    (A/I/S)
└──────┬────────┘                  └──────────────────────────────────────┘
       │ USB mass storage
       ▼
   the printer
```

- **The Pi is thin.** `cmd/pi-agent` serves one upload page on the makerspace
  LAN, forwards every submission to the central portal for the
  membership + certification + safety-checklist decision, and on approval
  writes the file onto the USB gadget via `usb-refresh.sh`. No local database,
  no auth logic, no web app.
- **The central portal owns the rules.** Resin-printer certification, print-job
  tracking, the per-printer safety checklist, the audit log, and Slack posting
  all live in Postgres and are managed in one admin UI.
- **TinkerAccess owns membership.** A worker polls the Leptos `get_users`
  endpoint (see [docs/GET_MEMBERS_ENDPOINT.md](docs/GET_MEMBERS_ENDPOINT.md))
  every few minutes and reconciles the roster. There are no expiry dates in
  TinkerAccess — "expired" means a member's status flipped to `I` or they
  dropped off the roster; either way the portal marks them inactive and denies
  prints until they come back.
- **Identity is trust-based.** A member types their Tinkermill Slack display
  name. The portal resolves it against an admin-maintained mapping, falling
  back to a unique exact full-name match. No password, no OAuth.

## Layout

```
cmd/portal/          the central binary — subcommands: server | worker | migrate
cmd/pi-agent/        the on-Pi agent (cross-compiled static armv6 binary)
internal/
  config/            env-var configuration for all three binaries
  store/             Postgres data layer (pgx) + goose migration runner
  tinkeraccess/      get_users client + Leptos-hash recovery helper
  sliced/            print-time / layer / machine-name parser (.goo .ctb .pwsz …)
  server/            chi router: Pi API, /api/v1/status, admin UI
  worker/            the roster-sync loop
  piagent/           upload page + central client + gadget write
  gadget/            wraps usb-refresh.sh
  slack/             best-effort Incoming Webhook poster
db/migrations/       goose SQL migrations (embedded in the binary)
web/                 admin-UI templates + static assets (embedded)
deploy/helm/resin-portal/   Helm chart (bundled Bitnami Postgres, PVC-backed)
build/Dockerfile     one image, subcommand-switched
pi/                  resin-pi-agent.service, install.sh, config.example.env
usb-refresh.sh       places one file (or nothing) on the gadget — `usb-refresh.sh <file>` / `--clear`
piusb-gadget.service (re)loads the USB gadget on every boot
hw-tests/            on-hardware validation, run by hand against a real Pi + printer
provisioning/        unattended flash-and-boot setup for a new Pi
CLAUDE.md            low-level USB gadget hardware notes and history
```

## Local development

Needs Go 1.24+ and Docker (for a throwaway Postgres).

### Run the portal locally (two commands)

```bash
make run-db                       # throwaway Postgres on :55432 (docker, --rm)
make run ADMIN_PASSWORD=letmein    # migrate + start the API server in the foreground
```

Then open **http://localhost:8080/admin/login** and sign in as `captain` with the
password you passed. `make run` migrates the database, seeds that first admin
account, and runs the server with `LOG_LEVEL=debug` and a fixed dev session
secret (so logins survive a restart). Ctrl-C stops the server; `make stop-db`
drops the database.

Useful overrides: `LOCAL_DSN=...` (point at a different database),
`STATUS_API_KEY=...` (the bearer key for `GET /api/v1/status`, default
`devstatuskey`), `ADMIN_USERNAME=...`.

### The roster-sync worker (optional, separate shell)

```bash
make run-worker \
  TINKERACCESS_BASE_URL=http://<tinker-access-host>:3000 \
  TINKERACCESS_GET_USERS_PATH=/api/get_users11102523982452806591
```

Polls TinkerAccess every minute and fills the `members` table. Without a
reachable TinkerAccess it just logs fetch errors and keeps an empty roster —
the server still runs; you just can't approve anyone until members sync.

### The Pi agent, pointed at your local server

`make run` leaves enrollment open (no token), so the agent needs only the portal
URL. It self-enrolls and persists its issued creds under `/tmp`:

```bash
CENTRAL_BASE_URL=http://localhost:8080 \
CREDS_PATH=/tmp/pi-agent-creds.env GADGET_IMAGE=/tmp/piusb.bin LISTEN_ADDR=:8081 \
go run ./cmd/pi-agent
```

Then approve it in the admin UI under **Printers → Pending**. (Or skip enrollment
entirely with `PRINTER_SLUG=... PRINTER_API_KEY=...` for a hand-made printer. If
you ran `make run ENROLL_TOKEN=foo`, pass `ENROLL_TOKEN=foo` here too.)

### Tests

```bash
make test                # unit tests, no DB
make test-integration    # + store/worker tests against run-db
make test-provision      # off-hardware check of the SD-card provisioning path
make build               # -> bin/portal
```

### Manual server invocation

`make run` is a thin wrapper; the underlying commands are:

```bash
export DATABASE_URL=postgres://postgres:test@localhost:55432/portal?sslmode=disable
bin/portal migrate up
SESSION_SECRET=$(head -c32 /dev/urandom | base64) ADMIN_PASSWORD=letmein \
  bin/portal server        # admin UI at http://localhost:8080/admin/login
```

## Deploying the central portal (Kubernetes + Helm)

```bash
make helm-deps                                  # fetch the Bitnami postgresql subchart
helm install portal deploy/helm/resin-portal \
  --set app.adminPassword='...' \
  --set app.sessionSecret='<32+ random chars>' \
  --set postgresql.auth.password='...' \
  --set tinkerAccess.baseURL='http://tinker-access.default.svc:3000' \
  --set tinkerAccess.getUsersPath='/api/get_users<hash>' \
  --set ingress.enabled=true --set ingress.host=portal.tinkermill.org --set ingress.tls=true
```

Pi self-enrollment is on by default; the admin approving a Pi is the gate. Add
`--set app.enrollToken='<long random string>'` (and the same value as
`ENROLL_TOKEN` in `provisioning/fleet.env`) only if the portal is reachable from
the public internet and you want to stop strangers from creating pending rows.

`values.yaml` documents every knob. Set `postgresql.enabled=false` +
`externalDatabase.dsn` to use a database you manage instead of the bundled one.
Migrations run automatically as a `post-install,pre-upgrade` Helm hook.

## Deploying the Pi agent

A freshly-flashed Pi self-registers — no per-Pi config. `provisioning/` +
[docs/second-pi-setup.md](docs/second-pi-setup.md) do first-time flash-and-boot
setup unattended; set `CENTRAL_URL` + `ENROLL_TOKEN` once in
`provisioning/fleet.env`.

To push an **agent update** to an already-running Pi:

```bash
make pi-agent
scp bin/pi-agent-armv6 usb-refresh.sh captain@<pi>.lan:~/
ssh captain@<pi>.lan 'sudo install -m0755 ~/pi-agent-armv6 /usr/local/bin/pi-agent && \
    sudo install -m0755 ~/usb-refresh.sh /usr/local/bin/usb-refresh.sh && \
    sudo systemctl restart resin-pi-agent'
```

## Bringing a printer online

1. Deploy the portal; confirm the worker logs a roster sync.
2. Set `CENTRAL_URL` in `provisioning/fleet.env`, flash a card
   (`provisioning/provision-sd.sh`), plug the Pi into the printer.
3. Admin UI → **Printers → Pending** — click **Approve** on the Pi that just
   showed up (it's keyed by hostname). This approval is the security gate.
4. **Members**: link Slack display names. **Certifications**: certify them for
   that printer.
5. *(optional)* Edit the printer to set its model + allowed extensions — the
   default is "accept anything" with the standard safety checklist.
6. Run `hw-tests/` against the Pi + printer to confirm end-to-end.
