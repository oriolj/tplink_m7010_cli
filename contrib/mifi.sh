#!/usr/bin/env bash
# Waybar module: TP-Link M7010 status.
# When the modem is unreachable, outputs nothing so waybar hides the module.

set -u

ADDR="192.168.0.1"
PASS_FILE="$HOME/.config/tplink-m7010/password"
BIN="$HOME/.local/bin/tplink-m7010"

# Quick reachability check (TCP connect, 500ms timeout). If it fails, exit silently.
if ! timeout 0.5 bash -c "echo >/dev/tcp/${ADDR}/80" 2>/dev/null; then
    exit 0
fi

if [[ ! -r "$PASS_FILE" ]]; then
    exit 0
fi

PASS=$(<"$PASS_FILE")
TPLINK_PASS="$PASS" TPLINK_ADDR="$ADDR" "$BIN" --waybar 2>/dev/null
