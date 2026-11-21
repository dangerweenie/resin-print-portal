# Development guide

How to build everything in this repo, run the whole system on your own machine
with no access to TinkerMill's network, and run the test suites.

## Platform

Development is supported on **Linux only**. The build, the test scripts, and
SD-card provisioning all assume a Linux userland (util-linux, udev, loop
devices). If you're on something else, use a Linux VM or a spare Linux box.

## Prerequisites

| Tool | Needed for |
|---|---|
| **Go**, at the version in `go.mod` (currently 1.26.7) | Everything. Install from [go.dev/dl](https://go.dev/dl/); distro packages usually lag. |
| **Docker**, with your user able to reach the daemon | `make run-db` (a throwaway Postgres) and `make docker` (the portal image) |
| **git** | Version stamping (`git describe`) |
| **Helm** | Deploying, and `make helm-lint` |
| `xz`, `zstd`, `parted`, `util-linux`, `udev` | Flashing SD cards with `provision-sd.sh` (see [pi-provisioning.md](pi-provisioning.md)) |
| `ssh`, `scp`, `curl`, `python3` | `scripts/simulate-tap.sh` and the on-hardware tests in `hw-tests/` |

Check what you have:

```bash
make deps            # report only
make deps-install    # offer to install whatever's missing via apt/dnf/pacman/apk
```

`make deps` exits non-zero only if a core tool (Go, Docker, git) is missing.
Everything else is reported against the workflow that needs it. Go and Docker
are never installed for you. Go's version is pinned, and Docker's daemon setup
is too system-specific to automate.

## What gets built

| Binary | Source | Runs on | Purpose |
|---|---|---|---|
| `portal` | `cmd/portal` | Kubernetes | Central service. Subcommands: `server` (API + admin UI + upload page), `worker` (TinkerAccess roster sync), `migrate` (database migrations). |
| `pi-agent` | `cmd/pi-agent` | Each printer's Pi | Reads fobs, asks the portal whether to load a print, writes it onto the USB drive. |
| `fake-tinkeraccess` | `cmd/fake-tinkeraccess` | Your machine | Stand-in for TinkerAccess's member roster, for local development. |

```bash
make build       # -> bin/portal              (native)
make pi-agent    # -> bin/pi-agent-armv6      (static, cross-compiled for the Pi Zero W)
make docker      # -> ghcr.io/dangerweenie/resin-portal:dev   (portal + bundled pi-agent)
```

Every binary is stamped with `git describe --tags --always --dirty`. The portal
image bundles the pi-agent built from the same commit and hands it out to Pis
for self-update, so the two always carry the same version string.

## Running the whole system locally

You don't need TinkerMill's network, a real TinkerAccess, a cluster, or a Pi.
Locally you run:

- **Postgres** in a throwaway container,
- **fake-tinkeraccess**, serving a roster you choose,
- the **real portal server** and **real roster-sync worker**, pointed at those,
- **`scripts/simulate-tap.sh`** in place of the Pi. The real agent needs the
  RFID reader on `/dev/serial0` and the USB gadget, so it can't run on a dev
  box. The script makes the same API calls the agent makes on a tap.

### 1. Start the backing services

```bash
make run-db                                           # Postgres on localhost:55432
make fake-tinkeraccess CODE=CAFE1234 NAME="Test Member"   # roster on :3000 (leave running)
```

`CODE` is the fob code for your fake member; any hex string works. For more
than one member, write a roster file and run the fake directly:

```bash
cat > /tmp/roster.json <<'EOF'
[
  {"id": 1, "name": "Test Member",   "code": "CAFE1234", "status": "A"},
  {"id": 2, "name": "Lapsed Member", "code": "BEEF0001", "status": "I"}
]
EOF
go run ./cmd/fake-tinkeraccess -roster /tmp/roster.json
```

Status `A` is active, `I` inactive, and `S` active (24h).

### 2. Start the portal and the worker

In separate terminals:

```bash
make run ADMIN_PASSWORD=letmein [SLACK_BOT_TOKEN=xob-...] [SLACK_CHANNEL=resin-status    # migrates, seeds admin "captain", serves :8080
```
```bash
make run-worker                    # syncs the roster from :3000 now, then every minute
```

The worker's defaults already point at the fake. Within a second you should
see `roster synced received=1` in its output.

### 3. Set up a printer in the admin UI

Open <http://localhost:8080/admin/login> and sign in as `captain` / `letmein`.

1. **Members** — your fake member(s) are listed, synced from the roster.
2. **Printers → Add a printer by hand** — slug `dev-printer`, then **Create
   printer**. Copy the API key from the confirmation banner; it's shown once.
   A printer added by hand is approved immediately and gets whatever safety
   checklist you type (blank means none). A real Pi registers itself, gets the
   default checklist, and waits under **Pending approval**.
3. **Certifications** — pick `dev-printer` and click **Certify** next to
   your member.

### 4. Upload a print as a member

Open <http://localhost:8080/upload>, choose `dev-printer`, enter the member's
name (`Test Member`), attach any file, tick the checklist, and submit. Any
file is fine; a real `.goo`/`.ctb`/`.pwsz` also gets its print time parsed. The
job is now **staged**: it shows under **Jobs**, waiting for a tap.

### 5. Tap a fob at the "printer"

```bash
scripts/simulate-tap.sh --slug dev-printer --key <API key> --fob CAFE1234
```

```text
--> tap: CAFE1234 on dev-printer
    check: {"allowed":true,"member_name":"Test Member","reason":"","staged_filename":"test.goo"}
    claim: {"claimed":true,"filename":"test.goo","job_id":1,"sha256":"5c70…","size":19,…}
loaded job 1: /tmp/simulated-gadget/test.goo
```

`/tmp/simulated-gadget` stands in for the USB drive: it holds exactly the one
file the printer would see. The job is now **printing**. End it as the member
would at the printer:

```bash
scripts/simulate-tap.sh --slug dev-printer --key <API key> --finish 1
```

Every tap also lands in the admin **Log**.

### Things worth trying

| Scenario | How |
|---|---|
| Unknown fob | `--fob DEADBEEF` → `DENIED: unknown_fob`. The admin Log records it as `fob:DEADBEEF`. |
| Uncertified member | **Revoke** the certification, tap again → `not_certified` |
| Inactive member | Restart the fake as `go run ./cmd/fake-tinkeraccess -code CAFE1234 -name "Test Member" -status I`; after the next sync (≤ 1 min) → `membership_inactive` |
| Certify by tap | Revoke, then **Certifications → Certify by tap**, then tap → `certified Test Member for dev-printer by tap` |
| Nothing staged | Tap with no upload pending → allowed, nothing to load |
| Wrong extension | Set the printer's allowed extensions, upload something else → rejected at upload |

### Tearing down

`Ctrl-C` the portal, worker, and fake, then `make stop-db`. The database
container is started with `--rm`, so stopping it discards all data. The next
`make run-db` starts empty.

Handy overrides for `make run`: `LOCAL_DSN=…` (a different database),
`STATUS_API_KEY=…` (default `devstatuskey`), `ENROLL_TOKEN=…` (require an
enrollment token). See [configuration.md](configuration.md) for every variable.

## Testing against a real Pi

When you need the real agent (reader, LEDs, USB gadget, a real printer), point
a test Pi at the portal on your machine instead of production:

1. Run the local stack above. Make sure the Pi can reach your machine on port
   8080 (same LAN, firewall open).
2. Flash a card with `--central-url http://<your-LAN-IP>:8080`, overriding
   `fleet.env` for that card only. See [pi-provisioning.md](pi-provisioning.md).
3. Boot it. It appears under **Printers → Pending approval** in your local
   admin UI; approve it.
4. Tap a real fob. It's denied as `unknown_fob`, and the Log shows its code
   (`fob:…`). Restart the fake with `CODE=<that code>`, wait for the next sync,
   certify, and you're testing with real hardware against a fully local
   backend.

The dedicated test rig is `resin1`. Never point development work at a Pi that's
in service.

## Tests

| Command | Needs | Covers |
|---|---|---|
| `make test` | nothing | Unit tests for every package, including the agent's tap/load logic against fake hardware and a fake portal |
| `make test-integration` | `make run-db` | The above, plus the Postgres store and the roster-sync worker against a real database. Store/worker tests skip themselves without `TEST_DATABASE_URL`, which this target sets. |
| `make test-provision` | nothing (no root, no card) | Runs `provision-sd.sh` against a fake boot partition and checks the cloud-init files and the staged agent payload, and that `pi/install.sh` lays every file down correctly |
| `make vet` | nothing | `go vet` |
| `hw-tests/` | a test Pi + printer | On-hardware validation, run by hand. See [`hw-tests/README.md`](../hw-tests/README.md). |

Run `make test` before every commit. Run `make test-integration` for anything
touching `internal/store`, `internal/worker`, or `db/migrations`, and
`make test-provision` for anything under `provisioning/` or `pi/`.

## Database migrations

Migrations live in `db/migrations/` as numbered [goose](https://github.com/pressly/goose)
SQL files (`00004_whatever.sql`, each with `-- +goose Up` and `-- +goose Down`
sections). They're embedded in the `portal` binary. `make run` applies them on
start, and in Kubernetes a Helm hook runs `portal migrate up` before every
upgrade. Run them by hand with:

```bash
DATABASE_URL=postgres://postgres:test@localhost:55432/portal?sslmode=disable \
  bin/portal migrate [up|down|status]
```

Never edit a migration that has been deployed. Add a new one.

## Make targets

`make help` lists everything. The ones you'll use:

| Target | Does |
|---|---|
| `deps` / `deps-install` | Check for, or install, the tools above |
| `build` / `pi-agent` / `docker` | Build the portal, the Pi agent, or the image |
| `run-db` / `stop-db` | Throwaway Postgres on `:55432` |
| `run` / `run-worker` | Portal server / roster worker against the local database |
| `fake-tinkeraccess` | Fake roster on `:3000` (`CODE=`, `NAME=`) |
| `test` / `test-integration` / `test-provision` / `vet` | Tests and checks |
| `helm-deps` / `helm-lint` | Fetch the chart's Postgres dependency; lint and render the chart |
| `clean` | Remove build output |
