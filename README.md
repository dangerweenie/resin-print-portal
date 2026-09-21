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

- **The Pi is thin.** `cmd/pi-agent` reads the fob, shows a "tap to load your
  queued print" page on the makerspace LAN, and on a tap pulls that member's
  staged file from the portal and writes it onto the USB gadget via
  `usb-refresh.sh`. No uploads, no local database, no auth logic, no web app.
- **The central portal owns the rules.** Resin-printer certification, print-job
  tracking, the per-printer safety checklist, the audit log, and Slack posting
  all live in Postgres and are managed in one admin UI.
- **TinkerAccess owns membership.** A worker polls the Leptos `get_users`
  endpoint (see [docs/GET_MEMBERS_ENDPOINT.md](docs/GET_MEMBERS_ENDPOINT.md))
  every few minutes and reconciles the roster. There are no expiry dates in
  TinkerAccess — "expired" means a member's status flipped to `I` or they
  dropped off the roster; either way the portal marks them inactive and denies
  prints until they come back.
- **Uploads go to the portal, releases happen at the printer.** A member
  uploads a sliced file on the portal's public `/upload` page (Slack display
  name + file + safety checklist); the portal checks membership + certification,
  parses the ETA, and holds the file. The member then walks to the printer and
  taps their **RFID fob** — that fob-release is what pulls the file onto the
  gadget. Physical presence is required to start a print; the big upload never
  touches the Pi's wifi or SD card.
- **Identity is trust-based.** Slack names are resolved against an
  admin-maintained mapping, falling back to a unique exact full-name match; fob
  UIDs against `members.code` from the synced roster. No password, no OAuth.

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
  rfid/              RDM6300 (UART) EM4100 fob reader — pure Go
  fobcode/           UID → all string forms, for format-agnostic matching
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

### Fleet version tracking + self-update

Every pi-agent binary is stamped with a version (`git describe`, shown as
`pi-agent starting version=…` in its logs) and reports it to the portal on
every check-in via an `X-Agent-Version` header. **Printers → Pending / Active**
shows each Pi's running version, when it last checked in, and a *behind* badge
when it isn't on the portal's own build.

The portal image carries the cross-compiled pi-agent it was built alongside and
can hand it out for an in-place self-update:

- **Automatic**: `--set agentUpdate.auto=true` (env `AGENT_AUTO_UPDATE=true`).
  Every Pi that isn't individually held converges to the portal's build on its
  next check-in (~5 min), **never mid-print**. Deploy a new portal image → the
  fleet follows.
- **Per-Pi**, from the Printers page, regardless of the global toggle:
  **Update now** pins a Pi to the current build; **Hold** freezes a Pi on what
  it's running; **Resume / Unpin** clears either.

The Pi downloads the binary (printer bearer auth), verifies its SHA-256,
swaps it in place keeping `pi-agent.prev`, and exits cleanly so systemd
(`Restart=always`) starts the new one. `agent-guard.sh` (an `ExecStartPre`)
rolls back to `pi-agent.prev` if a bad build crash-loops. All of this state
lives in `/var/lib/resin-pi-agent`, never on the boot partition. After
`maxUpdateAttempts` failed tries at the same version the Pi gives up and shows
as *behind* for a human to look at.

To push an agent update **out of band** (no portal, or a Pi with no route to
it):

```bash
make pi-agent
scp bin/pi-agent-armv6 usb-refresh.sh pi/agent-guard.sh captain@<pi>.lan:~/
ssh captain@<pi>.lan 'sudo install -m0755 ~/pi-agent-armv6 /usr/local/bin/pi-agent && \
    sudo install -m0755 ~/usb-refresh.sh /usr/local/bin/usb-refresh.sh && \
    sudo install -m0755 ~/agent-guard.sh /usr/local/bin/agent-guard.sh && \
    sudo systemctl restart resin-pi-agent'
```

### Identity: RFID fob only

Every Pi is fob-only. There is no name-entry mode and no switch for it.
TinkerMill's fobs are 125 kHz **EM4100** — an MFRC522 (13.56 MHz) can't read
those, so the fleet uses an **RDM6300**-class reader on the UART instead: wire
`5V→5V, GND→GND`, and `TX` **through a voltage divider** (1kΩ to the RXD node,
2kΩ from that node to GND — the reader's TX is 5V logic, the Pi's GPIO is not
5V-tolerant) `→ GPIO15/RXD (physical pin 10)`. That's all: the agent reads
`/dev/serial0` at 9600 baud on fixed pins, the Pi page is tap-to-load only, the
tag ID never touches the browser, and the **portal** matches it against
`members.code` in every format it could have been recorded as (hex, colon-hex,
decimal either endianness, and — since EM4100 tags carry a 5-byte ID whose
first byte is conventionally a site/version byte — the same forms of just the
last 4 bytes, the usual "card number"). The agent **won't start** if the
reader isn't working.

Every tap is checked against the portal once (`/check` → a `decision_log` row),
so who tapped what and when is queryable in the admin **Log** — the tap audit
trail lives centrally, not on the Pi. `sudo pi-agent -probe` is the wiring
diagnostic: it reports whether any bytes are arriving at all, whether they're
framing into valid checksummed reads, and decodes every tap it sees — enough
to tell "nothing wired" from "wired but the level shift/baud is off" from
"working." Note this reader is **125 kHz only** — a 13.56 MHz card/fob will
never show up.

**Certify by tap.** On the admin **Certifications** page, "Certify by tap" arms
a 2-minute window on a printer; the next fob tapped there certifies that member
for it (recorded in the log as `captured_certification`). No searching the
roster — the trainee just taps.

## Bringing a printer online

1. Deploy the portal; confirm the worker logs a roster sync (`members.code`
   comes from TinkerAccess — nothing to link for fobs).
2. Set `CENTRAL_URL` in `provisioning/fleet.env`, flash a card
   (`provisioning/provision-sd.sh`) — the image has an RDM6300 wired — and plug
   the Pi into the printer.
3. Admin UI → **Printers → Pending** — click **Approve** on the Pi that just
   showed up (it's keyed by hostname). This approval is the security gate.
4. **Certifications**: certify members for that printer (or use "Certify by
   tap" and have them tap at the Pi).
5. *(optional)* Edit the printer to set its model + allowed extensions — the
   default is "accept anything" with the standard safety checklist.
6. Give members the portal's `/upload` URL. They upload there, then tap their
   fob at the Pi to load the print.
7. Run `hw-tests/` to confirm the upload → tap → gadget path end-to-end.

## License

MIT — see [LICENSE](LICENSE).
