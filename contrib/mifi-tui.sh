#!/usr/bin/env bash
# Launch the M7010 TUI in a fresh terminal window.
# Used as the on-click handler for the waybar custom/mifi module.

set -u

PASS_FILE="$HOME/.config/tplink-m7010/password"
BIN="$HOME/.local/bin/tplink-m7010"
TERM_BIN="${TPLINK_TERM:-ghostty}"

if [[ -r "$PASS_FILE" ]]; then
    export TPLINK_PASS
    TPLINK_PASS=$(<"$PASS_FILE")
fi

exec "$TERM_BIN" -e "$BIN"
