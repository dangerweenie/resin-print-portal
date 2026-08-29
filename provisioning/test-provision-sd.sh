#!/bin/bash
# Off-hardware test for the SD-card provisioning path. Runs entirely on your
# dev machine, no root, no real card: it drives provision-sd.sh in --boot
# mode against a fake boot partition (a temp dir) and checks that
#
#   1. cloud-init user-data / network-config / meta-data come out right,
#   2. provision-boot.sh is copied into place,
#   3. the pi-agent payload is staged with every file the installer needs,
#   4. pi/install.sh (the ONE installer, which provision-boot.sh calls on the
#      Pi) actually lays those files down at the expected paths — exercised
#      here via PI_INSTALL_ROOT + PI_INSTALL_SKIP_SYSTEMD,
#   5. provision-sd.sh fails loudly when the agent binary hasn't been built,
#   6. every script still parses (bash -n), and shellcheck is clean if present.
#
# It does NOT boot anything or talk to a Pi — that's hw-tests/ + MANUAL-CHECKLIST.
# What it DOES catch is the thing most likely to silently rot: the staging
# contract between provision-sd.sh, provision-boot.sh and pi/install.sh
# drifting out of sync.
#
# Usage: bash provisioning/test-provision-sd.sh
set -u

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1" >&2; FAILS=$((FAILS + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

# --- a throwaway agent binary so this test needs no cross-compile ----------
BIN="$REPO/bin/pi-agent-armv6"
MADE_STUB=0
if [ ! -f "$BIN" ]; then
    mkdir -p "$REPO/bin"
    printf '#!/bin/sh\necho stub pi-agent\n' > "$BIN"
    chmod +x "$BIN"
    MADE_STUB=1
    echo "note: using a stub $BIN (run 'make pi-agent' for the real thing)"
fi
cleanup_stub() { [ "$MADE_STUB" = "1" ] && rm -f "$BIN"; }
trap 'cleanup_stub; rm -rf "$TMP"' EXIT

# --- fake boot partition -------------------------------------------------
BOOT="$TMP/bootfs"
mkdir -p "$BOOT"
: > "$BOOT/config.txt"
: > "$BOOT/cmdline.txt"

# =======================================================================
echo "### 1. provision-sd.sh --boot (happy path)"
bash "$REPO/provisioning/provision-sd.sh" --boot "$BOOT" \
    --hostname test1 --password 'pw-123' \
    --wifi-ssid 'SiteNet' --wifi-password 'wpa-secret' \
    --wifi-country GB --user pilot --repo-root "$REPO" \
    --central-url 'https://portal.example.org' --enroll-token 'fleet-tok-xyz' >/dev/null 2>&1
RC=$?
check "provision-sd.sh exits 0" "[ $RC -eq 0 ]"

check "user-data has the hostname"        "grep -q '^hostname: test1' '$BOOT/user-data'"
check "user-data creates the pilot user"  "grep -q 'name: pilot' '$BOOT/user-data'"
check "user-data runcmd calls provision-boot.sh" \
      "grep -q 'provision-boot.sh' '$BOOT/user-data'"
check "network-config has the SSID"       "grep -q 'SiteNet' '$BOOT/network-config'"
check "network-config has the wifi password" "grep -q 'wpa-secret' '$BOOT/network-config'"
check "network-config has the reg domain" "grep -q 'regulatory-domain: GB' '$BOOT/network-config'"
check "meta-data written"                 "[ -s '$BOOT/meta-data' ]"
check "provision-boot.sh copied to boot"  "[ -f '$BOOT/provision-boot.sh' ]"

# =======================================================================
echo "### 2. staged pi-agent payload"
P="$BOOT/payload/agent"
for f in pi-agent-armv6 usb-refresh.sh resin-pi-agent.service install.sh config.example.env resin-pi-agent.env; do
    check "payload/agent/$f staged" "[ -f '$P/$f' ]"
done
check "piusb-gadget.service staged" "[ -f '$BOOT/payload/piusb-gadget.service' ]"
check "no leftover printer-upload payload" "[ ! -e '$BOOT/payload/printer-upload' ]"

check "staged env has the portal URL"    "grep -q '^CENTRAL_BASE_URL=https://portal.example.org' '$P/resin-pi-agent.env'"
check "staged env carries the enroll token when given" "grep -q '^ENROLL_TOKEN=fleet-tok-xyz' '$P/resin-pi-agent.env'"
check "staged env has NO per-Pi slug/key" "! grep -qE '^PRINTER_(SLUG|API_KEY)=' '$P/resin-pi-agent.env'"

# The enroll token is optional; --central-url is not.
BOOT_NT="$TMP/bootfs-notoken"; mkdir -p "$BOOT_NT"; : > "$BOOT_NT/config.txt"; : > "$BOOT_NT/cmdline.txt"
check "works with NO enroll token" \
      "bash '$REPO/provisioning/provision-sd.sh' --boot '$BOOT_NT' --hostname nt --password p --wifi-ssid s --wifi-password w --central-url 'https://p.example' --repo-root '$REPO' >/dev/null 2>&1 && grep -q '^ENROLL_TOKEN=$' '$BOOT_NT/payload/agent/resin-pi-agent.env'"
if [ -f "$REPO/provisioning/fleet.env" ]; then
    echo "skip: fleet.env present, can't test the missing-central-url error"
else
    check "requires --central-url" \
      "! bash '$REPO/provisioning/provision-sd.sh' --boot '$BOOT' --hostname x --password p --wifi-ssid s --wifi-password w --repo-root '$REPO' >/dev/null 2>&1"
fi

# =======================================================================
echo "### 3. staging contract: everything install.sh / provision-boot.sh reads is present"
# every \$SRC/<file> or \$P/<file> referenced by the installer scripts must
# exist in the staged payload dir.
MISSING=""
for ref in $(grep -hoE '\$(SRC|P)/[A-Za-z0-9._-]+' \
                "$REPO/pi/install.sh" "$REPO/provisioning/provision-boot.sh" \
             | sed -E 's#\$(SRC|P)/##' | sort -u); do
    # pi-agent / pi-agent-armv6 are interchangeable names for the binary
    # (install.sh accepts either); section 2 already checks one is staged.
    case "$ref" in pi-agent|pi-agent-armv6) continue ;; esac
    [ -f "$P/$ref" ] || MISSING="$MISSING $ref"
done
check "install scripts reference only staged files (missing:$MISSING )" "[ -z '$MISSING' ]"

# =======================================================================
echo "### 4. pi/install.sh actually installs those files (dry run)"
DEST="$TMP/dest"
mkdir -p "$DEST/opt/printer-upload" "$DEST/etc/systemd/system"
: > "$DEST/etc/systemd/system/printer-upload.service"
PI_INSTALL_ROOT="$DEST" PI_INSTALL_SKIP_SYSTEMD=1 bash "$P/install.sh" "$P" >/dev/null 2>&1
IRC=$?
check "install.sh dry run exits 0" "[ $IRC -eq 0 ]"
check "  -> /usr/local/bin/pi-agent"       "[ -x '$DEST/usr/local/bin/pi-agent' ]"
check "  -> /usr/local/bin/usb-refresh.sh" "[ -x '$DEST/usr/local/bin/usb-refresh.sh' ]"
check "  -> /etc/systemd/system/resin-pi-agent.service" \
      "[ -f '$DEST/etc/systemd/system/resin-pi-agent.service' ]"
check "  -> /etc/resin-pi-agent.env seeded" "[ -f '$DEST/etc/resin-pi-agent.env' ]"
check "  -> old printer-upload.service removed" \
      "[ ! -e '$DEST/etc/systemd/system/printer-upload.service' ]"
check "  -> old /opt/printer-upload removed" "[ ! -e '$DEST/opt/printer-upload' ]"
check "  -> staged fleet env installed verbatim" \
      "grep -q '^ENROLL_TOKEN=fleet-tok-xyz' '$DEST/etc/resin-pi-agent.env'"
echo 'ADMIN_EDITED_MARKER=1' >> "$DEST/etc/resin-pi-agent.env"
check "install.sh doesn't clobber an existing env" \
      "PI_INSTALL_ROOT='$DEST' PI_INSTALL_SKIP_SYSTEMD=1 bash '$P/install.sh' '$P' >/dev/null 2>&1 && grep -q 'ADMIN_EDITED_MARKER=1' '$DEST/etc/resin-pi-agent.env'"

# =======================================================================
echo "### 5. provision-sd.sh fails cleanly with no agent binary"
BOOT2="$TMP/bootfs2"; mkdir -p "$BOOT2"; : > "$BOOT2/config.txt"; : > "$BOOT2/cmdline.txt"
[ -f "$BIN" ] && mv "$BIN" "$TMP/agent-bin.stash"   # hide it, restore after
# shellcheck disable=SC2034  # NB_OUT is used inside a `check` eval string below
NB_OUT="$(bash "$REPO/provisioning/provision-sd.sh" --boot "$BOOT2" \
          --hostname x --password p --wifi-ssid s --wifi-password w \
          --central-url 'https://p.example' --enroll-token 'tok' \
          --repo-root "$REPO" 2>&1)"
NB_RC=$?
[ -f "$TMP/agent-bin.stash" ] && mv "$TMP/agent-bin.stash" "$BIN"
check "exits non-zero without bin/pi-agent-armv6" "[ $NB_RC -ne 0 ]"
check "explains how to fix it" "printf '%s' \"\$NB_OUT\" | grep -q 'make pi-agent'"
check "did not half-stage a payload" "[ ! -d '$BOOT2/payload/agent' ]"

# =======================================================================
echo "### 6. static checks"
for s in provisioning/provision-sd.sh provisioning/provision-boot.sh pi/install.sh usb-refresh.sh; do
    check "bash -n $s" "bash -n '$REPO/$s'"
done
if command -v shellcheck >/dev/null 2>&1; then
    check "shellcheck (provisioning + install)" \
      "shellcheck -S warning '$REPO/provisioning/provision-sd.sh' '$REPO/provisioning/provision-boot.sh' '$REPO/pi/install.sh' '$REPO/usb-refresh.sh'"
else
    echo "skip: shellcheck not installed"
fi

echo
if [ "$FAILS" -eq 0 ]; then
    echo "All provisioning checks passed."
    exit 0
fi
echo "$FAILS provisioning check(s) failed."
exit 1
