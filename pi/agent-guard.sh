#!/bin/bash
# Crash-loop guard for pi-agent, run as ExecStartPre by resin-pi-agent.service.
#
# pi-agent replaces its own binary in place when the portal advertises a new
# build (see internal/piagent/selfupdate.go). If that new binary is bad and
# crash-loops, this guard notices the rapid restarts and rolls back to the
# previous binary the updater stashed as pi-agent.prev.
#
# A healthy pi-agent deletes the restart-track file once it reaches a listening
# state, so ordinary restarts never accumulate toward the threshold.
set -u

BIN=/usr/local/bin/pi-agent
PREV=$BIN.prev
STATE_DIR=/var/lib/resin-pi-agent
TRACK=$STATE_DIR/restart-track

WINDOW=600   # seconds to look back over
LIMIT=5      # this many starts within WINDOW -> roll back

mkdir -p "$STATE_DIR"
now=$(date +%s)

# Keep only timestamps inside the window, then append this start.
tmp=$(mktemp "$STATE_DIR/.restart-track.XXXXXX") || exit 0
if [ -f "$TRACK" ]; then
    awk -v n="$now" -v w="$WINDOW" 'NF && $1 ~ /^[0-9]+$/ && $1 > n - w' "$TRACK" > "$tmp"
fi
echo "$now" >> "$tmp"
mv -f "$tmp" "$TRACK"

count=$(wc -l < "$TRACK" 2>/dev/null || echo 0)
if [ "$count" -ge "$LIMIT" ] && [ -f "$PREV" ]; then
    logger -t resin-pi-agent "crash-loop guard: $count starts in ${WINDOW}s — rolling back to pi-agent.prev"
    mv -f "$PREV" "$BIN"
    : > "$TRACK"
fi

exit 0
