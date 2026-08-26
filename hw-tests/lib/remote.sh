# Shared SSH plumbing for hw-tests/*-against-pi.sh.
#
# Every script in this directory runs FROM the dev laptop/workstation and
# drives the Pi over SSH -- none of them run ON the Pi, and none of them
# ever apt-install (or otherwise modify the package set of) the system
# under test. Every remote command used anywhere in this suite is one of:
#   - losetup / mount / umount / modprobe / lsmod / dd / sha256sum / mkdir /
#     ls / cat: core util-linux + coreutils + kmod-tools, already required
#     for usb-refresh.sh and the gadget to work in production at all.
#   - systemctl: already required for piusb-gadget.service /
#     printer-upload.service to run at all.
#   - the DEPLOYED APP'S OWN venv python3 (put there by deploy.sh): used
#     for any sqlite3/json work instead of a standalone `sqlite3` CLI
#     package that wouldn't otherwise be on the Pi.
# Anything that needs tooling beyond that (building a FAT32 or
# MBR-partitioned test image needs mkdosfs/parted/losetup; driving the app
# over HTTP needs curl) runs HERE, on the laptop, and ships results to/from
# the Pi as plain files or short shell/python snippets. Needing those tools
# on your OWN laptop is normal dev-machine setup -- the same class of
# assumption this project already makes about running pytest/scp/ssh from
# here -- not the same thing as installing packages onto the printer's Pi.
#
# Privileged calls use `sudo -S` (password piped via stdin) rather than a
# pty (`ssh -tt`) specifically so stdin stays a clean byte stream -- needed
# because remote_sudo_py pipes actual Python source through the same
# channel, and pty line-ending translation is exactly the kind of thing
# that's fine for humans typing and flaky for programs piping structured
# input. This also means the login password is asked for by name once, up
# front, and reused for every sudo call rather than re-prompting per call.
#
# Usage: source this, call remote_setup <host> [ssh_user], then use
# remote / remote_sudo / remote_sudo_py / remote_put as needed, and call
# remote_teardown (a trap is fine) before exiting.
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

# Runs a python3 snippet (given on OUR stdin) as root, through the DEPLOYED
# APP'S OWN venv interpreter -- already installed as part of running the
# app at all, so this adds nothing to the Pi beyond what production already
# needs. Needs root because uploads.db / settings.json are root-owned
# (deploy.sh: chown -R root:root).
remote_sudo_py() {
    local base="${1:-/opt/printer-upload}"
    { printf '%s\n' "$HWTEST_SUDO_PW"; cat; } | \
        ssh -o ControlPath="$HWTEST_CTL" "$HWTEST_SSH_USER@$HWTEST_HOST" "sudo -S -p '' $base/venv/bin/python3 -"
}

# Same as remote_sudo_py but unprivileged -- for read-only queries where
# the caller already knows the files are readable without root.
remote_py() {
    local base="${1:-/opt/printer-upload}"
    ssh -o ControlPath="$HWTEST_CTL" "$HWTEST_SSH_USER@$HWTEST_HOST" "$base/venv/bin/python3 -"
}

remote_put() {  # $1 = local path, $2 = remote path
    scp -o ControlPath="$HWTEST_CTL" "$1" "$HWTEST_SSH_USER@$HWTEST_HOST:$2"
}
