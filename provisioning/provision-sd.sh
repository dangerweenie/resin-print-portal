#!/bin/bash
# Two modes:
#
#   --device mode: give it a blank SD card's whole-disk device node and a
#   Raspberry Pi OS image file, and it flashes + partitions the card AND
#   provisions it — one command, nothing else to click through. Needs
#   sudo (writing a raw block device requires root).
#
#   --boot mode: give it the boot partition of an ALREADY-flashed card
#   (e.g. if you used Raspberry Pi Imager yourself) and it just does the
#   provisioning part.
#
# Either way: after this script, eject the card, boot the Pi, and walk
# away — hostname/user/SSH/Wi-Fi come up via cloud-init's native NoCloud
# datasource (user-data/network-config, written below), then cloud-init's
# own `runcmd` stage (which only runs once real networking is up) triggers
# provision-boot.sh for the USB gadget + pi-agent install. One
# automatic reboot at the end (see provision-boot.sh) to load the new
# config.txt dtoverlay.
#
# NOT YET HARDWARE-VERIFIED end-to-end — do one test card before trusting
# this for the whole fleet. See docs/second-pi-setup.md.
#
# The portal URL is the same on every card. Put it in provisioning/fleet.env
# once (see fleet.env.example) — then you only pass the per-card
# hostname/password/wifi. An enrollment token is optional (fleet.env or
# --enroll-token) and only needed if the portal requires one.
#
# Usage (device mode — flashes AND provisions):
#   sudo ./provision-sd.sh --device /dev/sdb --image ~/Downloads/raspios-lite.img.xz \
#     --hostname resin3 --password 'pi-login-password' \
#     --wifi-ssid 'SiteWifi' --wifi-password 'wifi-password'
#
# Usage (boot mode — card already flashed):
#   ./provision-sd.sh --boot /media/you/bootfs \
#     --hostname resin3 --password 'pi-login-password' \
#     --wifi-ssid 'SiteWifi' --wifi-password 'wifi-password'
#
# Optional: --user (default captain), --wifi-country (default US),
# --central-url / --enroll-token (override provisioning/fleet.env),
# --repo-root (default: parent directory of this script), --yes (device
# mode only — skip the "type the device path to confirm" prompt).
#
# After the Pi boots it self-registers with the portal; an admin approves it
# once under Printers → Pending. The resin-room volunteer does nothing.
set -euo pipefail

USER_NAME=captain
WIFI_COUNTRY=US
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOT=""
DEVICE=""
IMAGE=""
ASSUME_YES=0
HOSTNAME=""
PASSWORD=""
WIFI_SSID=""
WIFI_PASSWORD=""
CENTRAL_URL=""
ENROLL_TOKEN=""

# Fleet-wide portal settings live in provisioning/fleet.env (gitignored) so you
# set them once, not per card. --central-url / --enroll-token override.
if [ -f "$REPO_ROOT/provisioning/fleet.env" ]; then
    # shellcheck disable=SC1091
    . "$REPO_ROOT/provisioning/fleet.env"
fi

while [ $# -gt 0 ]; do
    case "$1" in
        --boot) BOOT="$2"; shift 2 ;;
        --device) DEVICE="$2"; shift 2 ;;
        --image) IMAGE="$2"; shift 2 ;;
        --yes) ASSUME_YES=1; shift ;;
        --hostname) HOSTNAME="$2"; shift 2 ;;
        --user) USER_NAME="$2"; shift 2 ;;
        --password) PASSWORD="$2"; shift 2 ;;
        --wifi-ssid) WIFI_SSID="$2"; shift 2 ;;
        --wifi-password) WIFI_PASSWORD="$2"; shift 2 ;;
        --wifi-country) WIFI_COUNTRY="$2"; shift 2 ;;
        --central-url) CENTRAL_URL="$2"; shift 2 ;;
        --enroll-token) ENROLL_TOKEN="$2"; shift 2 ;;
        --repo-root) REPO_ROOT="$2"; shift 2 ;;
        *) echo "Unknown argument: $1" >&2; exit 1 ;;
    esac
done

for required in HOSTNAME PASSWORD WIFI_SSID WIFI_PASSWORD; do
    if [ -z "${!required}" ]; then
        echo "Missing required argument for: $required" >&2
        exit 1
    fi
done
if [ -z "$CENTRAL_URL" ]; then
    echo "Missing --central-url (or set CENTRAL_URL in provisioning/fleet.env)." >&2
    echo "It is the same on every card; the Pi self-registers with the portal on first boot." >&2
    exit 1
fi
# ENROLL_TOKEN is optional — only set it if the portal requires one.

if [ -n "$BOOT" ] && [ -n "$DEVICE" ]; then
    echo "ERROR: pass either --boot or --device, not both." >&2
    exit 1
fi
if [ -z "$BOOT" ] && [ -z "$DEVICE" ]; then
    echo "ERROR: pass --boot <mounted boot partition> or --device <SD card device> --image <path>." >&2
    exit 1
fi

CREATED_MOUNT=0

# ---------------------------------------------------------------------
# --device mode: flash the image onto the raw disk first, then mount the
# resulting boot partition and fall through into the same provisioning
# logic --boot mode uses.
# ---------------------------------------------------------------------
if [ -n "$DEVICE" ]; then
    if [ -z "$IMAGE" ]; then
        echo "ERROR: --device requires --image <path to a Raspberry Pi OS .img/.img.xz/.img.zst>." >&2
        exit 1
    fi
    if [ ! -f "$IMAGE" ]; then
        echo "ERROR: image file not found: $IMAGE" >&2
        exit 1
    fi
    if [ "$(id -u)" -ne 0 ]; then
        echo "ERROR: --device mode writes a raw disk — re-run this with sudo." >&2
        exit 1
    fi
    if [ ! -b "$DEVICE" ]; then
        echo "ERROR: $DEVICE is not a block device." >&2
        exit 1
    fi

    # Refuse anything that looks like it could be this machine's own disk.
    DEVICE_BASENAME=$(basename "$DEVICE")
    for mnt in / /boot /boot/firmware /home; do
        if [ -d "$mnt" ]; then
            SRC=$(findmnt -no SOURCE --target "$mnt" 2>/dev/null || true)
            if [ -n "$SRC" ]; then
                SRC_DISK=$(lsblk -no PKNAME "$SRC" 2>/dev/null || true)
                if [ "$SRC_DISK" = "$DEVICE_BASENAME" ] || [ "$SRC" = "$DEVICE" ]; then
                    echo "ERROR: $DEVICE backs this machine's own $mnt — refusing to touch it." >&2
                    exit 1
                fi
            fi
        fi
    done

    SIZE_BYTES=$(lsblk -bno SIZE "$DEVICE" | head -1)
    SIZE_GB=$((SIZE_BYTES / 1024 / 1024 / 1024))
    if [ "$SIZE_GB" -gt 128 ]; then
        echo "ERROR: $DEVICE is ${SIZE_GB}GB — bigger than any SD card this fleet uses." \
             "Refusing to write to it in case the wrong device was picked. If this really" \
             "is the right card, use --boot mode against it after flashing some other way." >&2
        exit 1
    fi

    echo "=== About to ERASE EVERYTHING on this device: ==="
    lsblk -o NAME,SIZE,MODEL,TRAN,MOUNTPOINT "$DEVICE"
    echo "=================================================="
    if [ "$ASSUME_YES" -ne 1 ]; then
        read -r -p "Type the device path exactly ($DEVICE) to confirm, anything else aborts: " CONFIRM
        if [ "$CONFIRM" != "$DEVICE" ]; then
            echo "Aborted — no changes made." >&2
            exit 1
        fi
    fi

    echo "--- unmounting any currently-mounted partitions of $DEVICE ---"
    for part in $(lsblk -lno NAME "$DEVICE" | tail -n +2); do
        umount "/dev/$part" 2>/dev/null || true
    done

    echo "--- writing $IMAGE to $DEVICE ---"
    case "$IMAGE" in
        *.img.xz|*.xz) xzcat "$IMAGE" | dd of="$DEVICE" bs=4M status=progress conv=fsync ;;
        *.img.zst|*.zst) zstd -dc "$IMAGE" | dd of="$DEVICE" bs=4M status=progress conv=fsync ;;
        *.img) dd if="$IMAGE" of="$DEVICE" bs=4M status=progress conv=fsync ;;
        *) echo "ERROR: unrecognized image extension (expected .img, .img.xz, or .img.zst): $IMAGE" >&2; exit 1 ;;
    esac
    sync
    partprobe "$DEVICE" 2>/dev/null || true
    udevadm settle 2>/dev/null || true

    if [[ "$DEVICE" =~ [0-9]$ ]]; then
        BOOT_PART="${DEVICE}p1"
    else
        BOOT_PART="${DEVICE}1"
    fi
    for _ in $(seq 1 10); do
        [ -b "$BOOT_PART" ] && break
        sleep 1
    done
    if [ ! -b "$BOOT_PART" ]; then
        echo "ERROR: expected boot partition $BOOT_PART never appeared after flashing." >&2
        exit 1
    fi

    BOOT=$(mktemp -d)
    mount "$BOOT_PART" "$BOOT"
    CREATED_MOUNT=1
    echo "--- flashed and mounted $BOOT_PART at $BOOT ---"
fi

# ---------------------------------------------------------------------
# From here on: $BOOT is a mounted boot partition, however it got that
# way. Same provisioning steps regardless of --boot vs --device mode.
# ---------------------------------------------------------------------
if [ ! -f "$BOOT/config.txt" ] || [ ! -f "$BOOT/cmdline.txt" ]; then
    echo "ERROR: $BOOT doesn't look like a Raspberry Pi OS boot partition" \
         "(no config.txt/cmdline.txt found there). Check the mount path." >&2
    exit 1
fi

AVAIL_KB=$(df -Pk "$BOOT" | awk 'NR==2 {print $4}')
if [ "$AVAIL_KB" -lt 51200 ]; then
    echo "WARNING: only ${AVAIL_KB}KB free on $BOOT — the app payload may" \
         "not fit. Continuing anyway." >&2
fi

echo "--- enabling SSH (belt-and-suspenders; ssh_pwauth in user-data below is"
echo "    the real mechanism) ---"
touch "$BOOT/ssh"

echo "--- writing cloud-init user-data / network-config / meta-data ---"
# Trixie's Raspberry Pi OS ships cloud-init as its NATIVE first-boot
# provisioning path now — this image's own network-config template says so
# in its comments, and Raspberry Pi's own announcement calls the old
# systemd.run=firstrun.sh kernel-cmdline trick (what this script used to do)
# "the legacy first-boot customisation system" it's moving away from. That
# old approach also fought cloud-init (disabling it outright) instead of
# using it, and needed a fragile stage1-reboot-stage2 dance to get app
# deployment after real networking came up. cloud-init already sequences
# that correctly on its own (network-config applied before the interface
# comes up; `runcmd` only runs once real networking is up) — see
# docs/second-pi-setup.md for the history of why this changed.
yq() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

cat > "$BOOT/user-data" <<EOF
#cloud-config
hostname: $(yq "$HOSTNAME")
manage_etc_hosts: true

users:
  - name: $(yq "$USER_NAME")
    groups: [sudo, adm, dialout, cdrom, audio, video, plugdev, games, users, input, render, netdev, gpio, i2c, spi]
    shell: /bin/bash
    lock_passwd: false

chpasswd:
  users:
    - name: $(yq "$USER_NAME")
      password: "$(yq "$PASSWORD")"
      type: text
  expire: false

ssh_pwauth: true

runcmd:
  - [ bash, /boot/firmware/provision-boot.sh ]
EOF

# netplan v2 format, straight from this image's own (previously unused)
# network-config template — including regulatory-domain, which is the
# native replacement for the old raspi-config-nonint-do_wifi_country dance
# (that wrote the country into cmdline.txt, which firstrun.sh's own cleanup
# step then clobbered on every run — a real bug in the old mechanism that
# this sidesteps entirely rather than fixing).
cat > "$BOOT/network-config" <<EOF
network:
  version: 2
  wifis:
    wlan0:
      dhcp4: true
      optional: false
      regulatory-domain: $(yq "$WIFI_COUNTRY")
      access-points:
        "$(yq "$WIFI_SSID")":
          password: "$(yq "$WIFI_PASSWORD")"
EOF

cat > "$BOOT/meta-data" <<EOF
instance_id: $(yq "$HOSTNAME")-$(date +%s)
dsmode: local
EOF

echo "--- copying provision-boot.sh (runs once via cloud-init's runcmd," \
     "after real networking is up) ---"
cp "$REPO_ROOT/provisioning/provision-boot.sh" "$BOOT/provision-boot.sh"

echo "--- staging pi-agent payload ---"
# The Pi no longer runs a local web app — it runs the thin Go `pi-agent`,
# which talks to the central portal. Cross-compile it first on this machine:
#   make pi-agent          # -> bin/pi-agent-armv6 (static, armv6)
AGENT_BIN="$REPO_ROOT/bin/pi-agent-armv6"
if [ ! -f "$AGENT_BIN" ]; then
    echo "ERROR: $AGENT_BIN not found — run 'make pi-agent' first." >&2
    exit 1
fi
rm -rf "$BOOT/payload"
mkdir -p "$BOOT/payload/agent"
cp "$AGENT_BIN"                              "$BOOT/payload/agent/pi-agent-armv6"
cp "$REPO_ROOT/usb-refresh.sh"              "$BOOT/payload/agent/usb-refresh.sh"
cp "$REPO_ROOT/pi/resin-pi-agent.service"   "$BOOT/payload/agent/resin-pi-agent.service"
cp "$REPO_ROOT/pi/install.sh"               "$BOOT/payload/agent/install.sh"
cp "$REPO_ROOT/pi/config.example.env"       "$BOOT/payload/agent/config.example.env"
cp "$REPO_ROOT/piusb-gadget.service"        "$BOOT/payload/"

# Stage a FILLED env with the fleet constant(s). Everything printer-specific
# (slug, API key) the Pi gets by self-registering with the portal on first
# boot — nobody edits anything per card. install.sh installs this verbatim.
cat > "$BOOT/payload/agent/resin-pi-agent.env" <<EOF
CENTRAL_BASE_URL=$CENTRAL_URL
ENROLL_TOKEN=$ENROLL_TOKEN
GADGET_IMAGE=/piusb.bin
LISTEN_ADDR=:80
USB_REFRESH_SCRIPT=/usr/local/bin/usb-refresh.sh
CREDS_PATH=/var/lib/resin-pi-agent/creds.env
LOG_LEVEL=info
EOF
chmod 600 "$BOOT/payload/agent/resin-pi-agent.env"
[ -n "$ENROLL_TOKEN" ] || echo "  note: no enroll token — enrollment is open, admin approval is the gate"

sync

if [ "$CREATED_MOUNT" -eq 1 ]; then
    umount "$BOOT"
    rmdir "$BOOT"
    echo "=== Done. Card fully flashed and provisioned — safe to remove it now. ==="
else
    echo "=== Done. Eject the card, boot the Pi, and wait a few minutes. ==="
fi
echo "cloud-init brings up Wi-Fi/hostname/user on the first boot, then runs"
echo "provision-boot.sh once networking is up; that script reboots once more"
echo "itself at the end (needed for the new config.txt dtoverlay to take"
echo "effect). Then:"
echo "  ssh $USER_NAME@$HOSTNAME.lan"
echo "  ssh $USER_NAME@$HOSTNAME.lan 'cat /var/log/provision-boot.log'"
echo "to confirm the pi-agent install finished."
