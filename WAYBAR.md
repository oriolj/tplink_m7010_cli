# Waybar integration — this machine

## Files installed outside this repo

| Path                                     | Contents                                    | Perms |
| ---------------------------------------- | ------------------------------------------- | ----- |
| `~/.local/bin/tplink-m7010`              | Compiled binary                             | 755   |
| `~/.config/tplink-m7010/password`        | Plain password, one line, no trailing space | 600   |
| `~/.config/waybar/scripts/mifi.sh`       | Wrapper used by the module                  | 755   |
| `~/.config/waybar/scripts/mifi-tui.sh`   | on-click handler — opens TUI in `$TPLINK_TERM` (default `ghostty`) | 755 |

The wrapper script is the important piece — it short-circuits with no output
when the modem isn't on the LAN (TCP connect check with 500ms timeout), which
makes waybar hide the module entirely instead of showing an error state.
Without this, disconnecting from the M7010 would leave a stale/failing tile on
the bar.

## waybar config changes

`~/.config/waybar/config.jsonc`:

- Added `"custom/mifi"` to `modules-right` (between `custom/memory` and
  `network`).
- Added a `custom/mifi` module block:

  ```jsonc
  "custom/mifi": {
      "exec": "~/.config/waybar/scripts/mifi.sh",
      "return-type": "json",
      "interval": 30,
      "format": "{}",
      "tooltip": true,
      "on-click": "~/.config/waybar/scripts/mifi-tui.sh"
  }
  ```

`~/.config/waybar/style.css`:

- Added `#custom-mifi` to the shared padding/color rule.
- Added `.warning`, `.critical`, `.disconnected` classes matching the colors
  used for the battery module (yellow / red / dim).

## Reload

```sh
pkill -SIGUSR2 waybar     # waybar reloads config on SIGUSR2
```

## Troubleshooting

- **Module silently missing when connected** — run the wrapper directly:
  `~/.config/waybar/scripts/mifi.sh`. If it prints nothing, the reachability
  check is failing; confirm you're actually on the M7010's Wi-Fi and that
  `192.168.0.1:80` is reachable.
- **"login failed (code 1)"** — wrong password. `result: 1` on step 2 is
  `DontMatch`. Re-check `~/.config/tplink-m7010/password` (no trailing
  newline issues: the wrapper uses `$(<file)` which trims the final newline).
- **Garbled output / decrypt errors after firmware update** — TP-Link could
  change the transport. Re-run with `--debug --raw` and compare against the
  examples in `PROTOCOL.md`.
