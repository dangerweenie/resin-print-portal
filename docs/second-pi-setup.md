# Second Pi Setup Runbook (Pi Zero 1 W)

Everything learned building the first Pi (`resin`, Zero 2 W, serving the M7
Pro), condensed into a repeatable checklist for the second Pi. See
`CLAUDE.md` for the low-level gadget details and `resin_plans.md` for the
full narrative/decision history — this doc is just the "do these steps"
version.

## 1. Flash the OS
- **Raspberry Pi Imager**, board = **Raspberry Pi Zero** (not Zero 2 W —
  Imager filters image compatibility by board choice, and picking the wrong
  one risks an image that won't boot on ARMv6 hardware).
- OS: **Raspberry Pi OS Lite (32-bit)**, Trixie. Confirmed this build still
  supports the original Zero W's ARMv6 chip — verified against current
  sources, not just assumption, since getting this wrong means a Pi that
  won't boot at all. (Debian is expected to drop ARMv6 after Trixie, so this
  is likely the last generation that supports this board — not a concern
  now, just don't expect to OS-upgrade this Pi indefinitely later.)
- In Imager's advanced options (the gear icon): set hostname, enable SSH,
  set the `captain` user + password, and — if the target site's wifi is
  already known — preload that SSID/password too. If the site's wifi is
  *not* known ahead of time, see the captive-portal note under "Not yet
  started" below; that isn't built yet, so for now you'll need to know the
  network in advance or plug in a keyboard/monitor once.

## 2. First boot / verify
```bash
ssh captain@<hostname>.lan   # NOT .local — mDNS doesn't resolve from this
                              # WSL2 client; use .lan (or the Pi's IP)
uname -m && cat /etc/os-release   # expect armv6l or armv7l, Raspbian 13 trixie
```

## 3. USB gadget setup
Hardware: use the Zero W's **only** micro-USB port carefully — unlike the
Zero 2 W, the original Zero/Zero W has a single data-capable port doing
double duty for power *and* OTG data (check silkscreen; some Zero W boards
label it differently than the Zero 2 W's dual-port layout). Confirm which
port is which on the actual board before connecting to the printer.

`/boot/firmware/config.txt`, under `[all]`:
```
dtoverlay=dwc2,dr_mode=peripheral
```
(On the original Zero W this may matter less than on the Zero 2 W — the
Zero W doesn't default to host mode the way the Zero 2 W can — but set it
explicitly anyway for consistency.)

Build the gadget image — **recommend bare FAT32, no partition table** for a
fresh build (simpler; avoids the `losetup -fP` partition-handling dance the
first Pi's pre-existing image needed). The current `usb-refresh.sh` handles
either format correctly, so this isn't a hard requirement, just the
simpler starting point:
```bash
sudo dd if=/dev/zero of=/piusb.bin bs=1M count=8192 status=progress
sudo mkdosfs /piusb.bin -F 32 -I -n RESINUSB
sudo modprobe g_mass_storage file=/piusb.bin stall=0 ro=0 removable=1
```

## 4. Deploy the Flask portal
No git repo yet — copy the app directory over directly (adjust the
hostname):
```bash
# From this machine:
scp -r printer-upload captain@<hostname>.lan:/tmp/deploy
scp usb-refresh.sh captain@<hostname>.lan:/tmp/deploy/

# On the Pi:
sudo mkdir -p /opt/printer-upload/{templates,files,tmp}
sudo cp /tmp/deploy/app.py /tmp/deploy/sliced_file_info.py /tmp/deploy/pure_aes.py /tmp/deploy/requirements.txt /opt/printer-upload/
sudo cp /tmp/deploy/templates/*.html /opt/printer-upload/templates/
sudo cp /tmp/deploy/usb-refresh.sh /usr/local/bin/usb-refresh.sh
sudo chmod 755 /usr/local/bin/usb-refresh.sh
sudo chown -R root:root /opt/printer-upload

# Dedicated venv, not system Python + --break-system-packages — apt install
# python3-venv first if this errors, it's not preinstalled on Lite images:
sudo python3 -m venv /opt/printer-upload/venv
sudo /opt/printer-upload/venv/bin/pip install -q -r /opt/printer-upload/requirements.txt
```
Pins in `requirements.txt` are provisional (real, current, mutually-compatible
versions, but not yet verified on ARMv6) — once this install succeeds, run
`sudo /opt/printer-upload/venv/bin/pip freeze` and commit the fully-resolved
output back into `requirements.txt` as the hardware-verified source of truth.

Systemd unit (`/etc/systemd/system/printer-upload.service`):
```ini
[Unit]
Description=Resin Printer Upload Portal
After=network.target

[Service]
WorkingDirectory=/opt/printer-upload
ExecStart=/opt/printer-upload/venv/bin/gunicorn --bind 0.0.0.0:80 --workers 1 --worker-class gthread --threads 4 --timeout 300 app:app
Restart=always
User=root

[Install]
WantedBy=multi-user.target
```
`gthread` + a few threads (not just a bare sync worker) matters more on this
board than it would on a beefier one: the Zero W is single-core, so one
member's large upload over a slow wifi link would otherwise fully block
every other request — including the printer's own polling and the admin
dashboard — for the whole transfer. Threads cost little extra RAM (thread
stacks, not a second Python process) and the app's request handling is
already safe for it (each request opens/closes its own short-lived sqlite
connection, nothing shared across requests).
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now printer-upload
curl -s -o /dev/null -w '%{http_code}\n' http://localhost/   # expect 200
sudo journalctl -u printer-upload -n 20 --no-pager   # confirm INFO-level app logs show up
```

## 5. Configure for the target printer
Log into `/admin` (username `captain`, password from
`/opt/printer-upload/.initial_admin_password` — generated fresh on first
run, not a fixed default; change it via Settings and delete that file),
then in Settings:
- **Printer Name** — whatever this Pi's printer actually is.
- **File Type Filter** — enable it, set to *only* that printer's actual
  supported extensions. Don't skip this — the first Pi's default filter
  didn't match the M7 Pro's real formats and it took a confusing "empty
  screen" moment to notice.
- **Supports folders** — leave unchecked. Currently unused/moot anyway
  (the drive only ever holds the current job's single file — see
  `resin_plans.md`), kept only in case multi-file browsing comes back later.
- **Safety Checklist** — review/adjust the default items if this printer
  has different physical-check needs (different vat/plate mechanism, etc).
- **Slack webhook** — optional, same as the first Pi.

## 6. Certify members
Each Pi has its own separate `members` table — **certifying someone on the
first Pi does NOT give them access on this one.** If Hunter/Ed/Matt (or
anyone else) need access here too, they need a *separate* account created
via this Pi's own `/admin/members`, even though it's the same physical
person. This is a deliberate consequence of "one Pi = one printer = an
account here means certified on this specific printer" — not a bug, but
worth knowing about so it doesn't look like a broken sync.

## Not yet started (same as the first Pi)
- Captive-portal wifi provisioning — not built anywhere yet. If this Pi is
  going to a site with unknown wifi at deploy time, you'll need to either
  know the network in advance (set it in step 1) or plug in a keyboard/
  monitor once to configure it manually until that's built.
