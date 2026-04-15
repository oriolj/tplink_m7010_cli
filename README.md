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
| Debug      | `--debug`         | Dumps every HTTP request/response, plaintext and encrypted |

## Flags

```
--addr      modem IP (default 192.168.0.1)
--pass      admin password (default "admin")
--waybar    waybar JSON mode
--raw       dump decrypted API responses
--debug     verbose HTTP/crypto logging
--refresh   TUI refresh interval (default 10s)
```

Environment variables `TPLINK_ADDR` and `TPLINK_PASS` override the flags; prefer
these in shell config to keep the password out of `ps`.

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
