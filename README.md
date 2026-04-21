# tplink-m7010

Go tool to query the TP-Link M7010 mobile Wi-Fi hotspot and display its state
either as a Bubble Tea TUI dashboard or as a waybar JSON module.

## Features

- Connection type (2G/3G/4G/4G+) with operator name and LTE band
- Signal strength (derived from RSRP, since the firmware reports `signalStrength: 0`)
- Battery percent + charging state
- Data usage (total bytes) and monthly limit if set
- WAN IPv4, connected device count

## Modes

| Mode       | Flag              | Output                                       |
| ---------- | ----------------- | -------------------------------------------- |
| TUI        | (default)         | Interactive dashboard, polls every 10s       |
| Waybar     | `--waybar`        | Single JSON line: `{text, tooltip, class}`   |
| Raw dump   | `--raw`           | Pretty-printed raw `status` + `flowstat` JSON |
| Power off  | `--poweroff`      | Shut the modem down (it will need physical button press to wake) |
| Reboot     | `--reboot`        | Restart the modem                             |
| Debug      | `--debug`         | Dumps every HTTP request/response, plaintext and encrypted |

## Flags

```
--addr      modem IP (default 192.168.0.1)
--pass      admin password (default "admin")
--waybar    waybar JSON mode
--raw       dump decrypted API responses
--poweroff  power the modem off and exit
--reboot    reboot the modem and exit
--debug     verbose HTTP/crypto logging
--refresh   TUI refresh interval (default 10s)
```

## TUI keybinds

| Key           | Action                                                        |
| ------------- | ------------------------------------------------------------- |
| `r`           | Refresh now                                                   |
| `p`, then `p` | Power off the modem (first press arms, second confirms)       |
| `R`, then `R` | Reboot the modem (first press arms, second confirms)          |
| `q` / `esc`   | Quit                                                          |

## Password

Three ways to supply the admin password, in order of preference:

1. **Password file** (what the bundled waybar wrapper uses):

   ```sh
   install -d -m700 ~/.config/tplink-m7010
   printf '%s' 'your-password' > ~/.config/tplink-m7010/password
   chmod 600 ~/.config/tplink-m7010/password
   ```

   Then read it into the env var when running by hand:

   ```sh
   TPLINK_PASS=$(<~/.config/tplink-m7010/password) tplink-m7010
   ```

   `make install` creates the directory and prints this hint if the file is
   missing.

2. **Environment variable** — set `TPLINK_PASS` (and optionally `TPLINK_ADDR`)
   in your shell config. Both override the corresponding flags.

3. **`--pass` flag** — fine for one-off debugging, but visible in `ps` output
   on a shared machine.

## Installation

```sh
make install            # builds + installs to ~/.local/bin
make install-waybar     # drops the waybar wrapper script in ~/.config/waybar/scripts
```

Other useful targets: `make build`, `make run`, `make raw`, `make clean`,
`make vet`, `make tidy`.

## Waybar integration

See `WAYBAR.md` for the setup on this machine (binary path, password file,
wrapper script that hides the module when the modem is unreachable).

## Protocol notes

See `PROTOCOL.md` for everything reverse-engineered about the M7010 web API
(endpoints, AES+RSA envelope, response schema). This was not trivial — the
M7010 uses a different wire format than the M7350 (plain JSON + nonce digest)
that most existing reverse-engineering efforts target.

## Documentation

| File              | Covers                                                                 |
| ----------------- | ---------------------------------------------------------------------- |
| `README.md`       | This file — what it is, how to run it                                  |
| `PROTOCOL.md`     | Reverse-engineered wire format, module/action catalog, example bodies  |
| `ARCHITECTURE.md` | Code structure, request flow, `Client` state, TUI model                |
| `WAYBAR.md`       | Waybar setup on this specific machine (paths, wrapper script behavior) |
| `DEVELOPMENT.md`  | Build log: what was tried, what failed, what stuck                     |
| `CLAUDE.md`       | Pointers for future Claude Code sessions                               |

## Files

- `main.go` — CLI entry point, flag parsing, waybar/raw/TUI modes, TUI model
- `client.go` — HTTP client, AES-128-CBC + RSA-PKCS1v15 envelope, status/flowstat parsing
- `Makefile` — build, install, waybar integration
- `waybar-example.jsonc` — Minimal waybar module snippet for other machines
- `contrib/mifi.sh` — Waybar wrapper: silent when modem is unreachable
- `contrib/mifi-tui.sh` — Opens the TUI in a terminal (wired to waybar tile click)
