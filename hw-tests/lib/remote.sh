# shellcheck shell=bash
# Shared SSH plumbing for hw-tests/*-against-pi.sh (sourced, never executed).
#
# Every script in this directory runs FROM the dev laptop/workstation and
# drives the Pi over SSH -- none of them run ON the Pi, and none of them
# ever apt-install (or otherwise modify the package set of) the system
# under test. Every remote command used anywhere in this suite is one of:
#   - losetup / mount / umount / modprobe / lsmod / dd / sha256sum / mkdir /
#     ls / cat: core util-linux + coreutils + kmod-tools, already required
#     for usb-refresh.sh and the gadget to work in production at all.
#   - systemctl: already required for piusb-gadget.service /
#     resin-pi-agent.service to run at all.
# Anything that needs tooling beyond that (building a FAT32 or
# MBR-partitioned test image needs mkdosfs/parted/losetup; driving the
# agent + central portal over HTTP needs curl) runs HERE, on the laptop,
# and ships results to/from the Pi as plain files or short shell snippets.
# Needing those tools on your OWN laptop is normal dev-machine setup, not
# the same thing as installing packages onto the printer's Pi.
#
# Privileged calls use `sudo -S` (password piped via stdin) rather than a
# pty (`ssh -tt`) so stdin stays a clean byte stream. The login password is
# asked for once, up front, and reused for every sudo call.
#
# Usage: source this, call remote_setup <host> [ssh_user], then use
# remote / remote_sudo / remote_put as needed, and call remote_teardown
# (a trap is fine) before exiting.
set -u

HWTEST_CTL="$(mktemp -u /tmp/hwtest-ssh-XXXXXX.sock)"

remote_setup() {
    HWTEST_HOST="$1"
    HWTEST_SSH_USER="${2:-captain}"
    echo "--- opening one SSH connection to $HWTEST_SSH_USER@$HWTEST_HOST (reused for the whole run) ---"
    ssh -MNf -o ControlPath="$HWTEST_CTL" -o ControlPersist=15m "$HWTEST_SSH_USER@$HWTEST_HOST" || {
        echo "ERROR: could not open SSH connection to $HWTEST_SSH_USER@$HWTEST_HOST" >&2
        return 1
    }
    read -r -s -p "sudo password for $HWTEST_SSH_USER@$HWTEST_HOST (same as the login password, per docs/second-pi-setup.md): " HWTEST_SUDO_PW
    echo
}

remote_teardown() {
    [ -S "$HWTEST_CTL" ] && ssh -O exit -o ControlPath="$HWTEST_CTL" "$HWTEST_SSH_USER@$HWTEST_HOST" 2>/dev/null
    return 0
}

# Unprivileged remote command.
remote() {
    ssh -o ControlPath="$HWTEST_CTL" "$HWTEST_SSH_USER@$HWTEST_HOST" "$@"
}

# Privileged remote command, via `sudo -S` (password piped, no pty).
remote_sudo() {
    printf '%s\n' "$HWTEST_SUDO_PW" | \
        ssh -o ControlPath="$HWTEST_CTL" "$HWTEST_SSH_USER@$HWTEST_HOST" "sudo -S -p '' $*"
}

remote_put() {  # $1 = local path, $2 = remote path
    scp -o ControlPath="$HWTEST_CTL" "$1" "$HWTEST_SSH_USER@$HWTEST_HOST:$2"
}
