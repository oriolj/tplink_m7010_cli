#!/usr/bin/env bash
# Launch the hotspot TUI in a fresh terminal window.
# Used as the on-click handler for the waybar custom/mifi module.
#
# No password handling here — the binary resolves its own password from
# the env vars or the per-device file under ~/.config (see README.md).

set -u

BIN="${TPLINK_M7010_BIN:-$HOME/.local/bin/tplink-m7010}"
TERM_BIN="${TPLINK_TERM:-ghostty}"

exec "$TERM_BIN" -e "$BIN"
