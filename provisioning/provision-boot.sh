#!/bin/bash
# Runs once via cloud-init's `runcmd:` (see user-data, written by
# provision-sd.sh). By this point cloud-init has already handled hostname,
# user creation, SSH, and Wi-Fi (network-config's netplan wifis: block,
# applied before the interface ever comes up) — this script no longer does
# any of that itself. It only handles what cloud-init doesn't: the USB
# gadget setup and the pi-agent install.
#
# ⚠️ 2026-08-25 — do NOT assume `runcmd` firing means real networking is up.
# That was this script's original assumption (cloud-init's final stage is
# supposed to gate on network-online.target) but a real test card disproved
# it: cloud-init's own "Net device info" log showed `wlan0 | Up: False` and
# then walked straight through config→final→runcmd in ~2 minutes anyway.
# Whatever network-online.target considers "online" here, it isn't "wlan0
# actually associated" — so this script now verifies connectivity itself
# (see the retry loop below) instead of trusting the stage it was called from.
set -eu

BOOT=/boot/firmware
LOG=/var/log/provision-boot.log
exec > >(tee -a "$LOG") 2>&1

# Single running checkpoint log on the FAT32 boot partition — readable with
# just a card reader, no root/network/serial access needed, same as before.
checkpoint() { printf '%s CHECKPOINT: %s\n' "$(date -Is)" "$1" >> "$BOOT/boot-progress.log"; sync; }

checkpoint "provision-boot-started"
echo "=== provision-boot.sh starting: $(date -Is) ==="

echo "--- enabling persistent journal ---"
# The stock journald.conf here leaves /var/log/journal empty even across a
# full boot (confirmed on two separate test cards) — meaning wpa_supplicant/
# NetworkManager association failures are otherwise unrecoverable after the
# fact from a card reader. This takes effect starting the reboot at the end
# of this script (journald reads its config at its own very-early startup,
# before cloud-init/runcmd ever runs), so it won't help diagnose THIS boot,
# but will capture the next one.
mkdir -p /etc/systemd/journald.conf.d
cat > /etc/systemd/journald.conf.d/persistent.conf <<'EOF'
[Journal]
Storage=persistent
EOF

echo "--- trimming background services (best-effort, single-core Zero W) ---"
# avahi-daemon (mDNS/.local) deliberately NOT disabled here — it's unrelated
# to whether <hostname>.lan resolves (that's the router registering the
# DHCP-supplied hostname into its own local DNS, a separate mechanism this
# Pi doesn't control) but it's a real, working fallback for any client that
# isn't WSL2 (which famously can't resolve .local at all — see CLAUDE.md).
for svc in bluetooth hciuart triggerhappy; do
    systemctl disable --now "$svc" 2>/dev/null || true
done

echo "--- config.txt ---"
CONFIG="$BOOT/config.txt"
grep -q '^dtoverlay=dwc2,dr_mode=peripheral' "$CONFIG" || \
    printf 'dtoverlay=dwc2,dr_mode=peripheral\n' >> "$CONFIG"
grep -q '^dtoverlay=disable-bt' "$CONFIG" || \
    printf 'dtoverlay=disable-bt\n' >> "$CONFIG"
grep -q '^gpu_mem=' "$CONFIG" || \
    printf 'gpu_mem=16\n' >> "$CONFIG"

echo "--- USB gadget image ---"
if [ ! -f /piusb.bin ]; then
    dd if=/dev/zero of=/piusb.bin bs=1M count=8192 status=progress
    mkdosfs /piusb.bin -F 32 -I -n RESINUSB
fi
cp "$BOOT/payload/piusb-gadget.service" /etc/systemd/system/piusb-gadget.service
systemctl daemon-reload
systemctl enable --now piusb-gadget

checkpoint "gadget-configured"

echo "--- waiting for real network (not trusting runcmd's own gating — see" \
     "the note at the top of this file) ---"
NETWORK_OK=0
for _ in $(seq 1 36); do   # ~6 minutes, 10s per attempt
    if getent hosts deb.debian.org >/dev/null 2>&1; then
        NETWORK_OK=1
        break
    fi
    sleep 10
done

# The pi-agent is a single static Go binary — no venv, no pip, no apt. It
# also doesn't strictly need the network at install time (it just can't reach
# the central portal until one exists / the config is filled in), so this
# runs regardless of NETWORK_OK. The actual install is pi/install.sh — the
# same script the manual/scp deploy uses — staged into the payload by
# provision-sd.sh, so there's exactly one install implementation.
install_agent() {
    bash "$BOOT/payload/agent/install.sh" "$BOOT/payload/agent"
}

echo "--- installing pi-agent ---"
if install_agent; then
    checkpoint "pi-agent-installed"
    rm -rf "$BOOT/payload/agent"
    if [ "$NETWORK_OK" -ne 1 ]; then
        echo "!!! No working network — pi-agent is installed but can't reach" \
             "the central portal yet. Fix networking, then edit" \
             "/etc/resin-pi-agent.env and 'systemctl restart resin-pi-agent'." >&2
    else
        echo "pi-agent installed. Edit /etc/resin-pi-agent.env with this" \
             "printer's CENTRAL_BASE_URL / PRINTER_SLUG / PRINTER_API_KEY," \
             "then 'systemctl restart resin-pi-agent'."
    fi
else
    checkpoint "pi-agent-install-failed"
    echo "!!! pi-agent install failed — see $LOG above. Payload left at" \
         "$BOOT/payload for inspection. The gadget is already configured and" \
         "will still come up after the reboot below." >&2
fi

checkpoint "provision-boot-finished"
echo "=== provision-boot.sh done: $(date -Is) ==="

# config.txt's new dtoverlay only takes effect on the next boot (device tree
# overlays are applied by the firmware bootloader, not hot-reloadable) — so
# this always ends with one reboot, same as the old firstrun.sh did. Confirmed
# on real hardware that a plain `reboot` can hang here if issued from a unit
# systemd then tries to tear down as part of its own shutdown sequence; this
# is a completely normal fully-booted system (not the old early
# kernel-command-line.target stage where that was root-caused), so the risk
# is much lower, but backgrounding a short-delayed sysrq reboot is free
# insurance and this trick is already proven reliable on this hardware.
(sleep 2; echo b > /proc/sysrq-trigger) &
disown
