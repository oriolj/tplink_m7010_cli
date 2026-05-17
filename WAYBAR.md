# Waybar integration — this machine

## Files installed outside this repo

| Path                                     | Contents                                    | Perms |
| ---------------------------------------- | ------------------------------------------- | ----- |
| `~/.local/bin/tplink-m7010`              | Compiled binary (autodetects M7010 / Mudi)  | 755   |
| `~/.config/tplink-m7010/password`        | M7010 admin password, one line              | 600   |
| `~/.config/gl-e5800/password`            | Mudi (GL-E5800) admin password, one line    | 600   |
| `~/.config/waybar/scripts/mifi.sh`       | Wrapper used by the module                  | 755   |
| `~/.config/waybar/scripts/mifi-tui.sh`   | on-click handler — opens TUI in `$TPLINK_TERM` (default `ghostty`) | 755 |

You only need whichever password file matches the device(s) you actually
use. The binary autodetects which router is on the LAN — preferring the
default-gateway match — and reads the matching file.

The wrapper script itself is now a one-line `exec` of the binary in
`--waybar` mode; the silence-when-unreachable behaviour and the device
selection both live inside the Go binary. (Older versions of the
wrapper did a TCP probe in bash and only worked for the M7010 — the
new wrapper has no device-specific knowledge.)

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

- **Module silently missing when connected** — run the binary directly:
  `~/.local/bin/tplink-m7010 --waybar`. An empty JSON object means
  autodetect didn't find a supported router. Check `ip route show default`
  — autodetect prefers a default gateway that matches a known device IP
  (`192.168.0.1` for M7010, `192.168.8.1` for Mudi).
- **"login failed" / "Access denied"** — wrong password. For the M7010,
  `result: 1` on step 2 is `DontMatch`. Re-check the password file
  (`~/.config/tplink-m7010/password` or `~/.config/gl-e5800/password` —
  no trailing newline issues; readers trim them).
- **Garbled output / decrypt errors after firmware update** — TP-Link or
  GL.iNet may have changed the transport. Re-run with `--debug --raw` and
  compare against the examples in `PROTOCOL.md` (M7010) or
  `PROTOCOL_GLINET.md` (Mudi).
- **Wrong device picked by autodetect** — force a specific device with
  `--device m7010` / `--device mudi`. If autodetect is wrong because two
  routers' default IPs are both accepted on the LAN, the default-gateway
  signal should be reliable; if it isn't, file a bug.
