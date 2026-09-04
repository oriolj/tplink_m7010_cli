#!/usr/bin/env bash
# Waybar module: hotspot status (TP-Link M7010/M7450 or GL.iNet Mudi GL-E5800).
#
# The binary autodetects which router is on the LAN and emits an empty JSON
# object ({"text":"", …}) when nothing is reachable, so waybar can hide the
# module without us needing a TCP probe here. Passwords come from
# $HOME/.config/tplink-m7010/password (M7010),
# $HOME/.config/tplink-m7450/password (M7450), or
# $HOME/.config/gl-e5800/password (Mudi) — the binary picks the right one
# based on which device it detects.

set -u

BIN="${TPLINK_M7010_BIN:-$HOME/.local/bin/tplink-m7010}"

exec "$BIN" --waybar 2>/dev/null
