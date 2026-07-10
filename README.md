# tplink-m7010

Go tool to query a mobile Wi-Fi hotspot and display its state as a Bubble Tea
TUI, a waybar JSON module, or a noctalia-shell CustomButton widget.

Despite the name, the binary now supports **two router families** with the
same code:

| Family             | Model        | Default IP     | Wire format                       |
| ------------------ | ------------ | -------------- | --------------------------------- |
| TP-Link            | M7010        | `192.168.0.1`  | AES-128-CBC + RSA-PKCS1v15 envelope |
| GL.iNet (OpenWrt)  | Mudi GL-E5800 | `192.168.8.1` | JSON-RPC with SHA-256-crypt + sid |

The same binary autodetects which device is on the LAN — it prefers the
default-gateway match and falls back to probing both addresses in parallel.
Both signals are confirmed with a cheap unauthenticated **protocol probe**
(the M7010 hello / the Mudi challenge), so a home router that happens to
live at 192.168.0.1 is not mistaken for a hotspot. If neither hotspot is
reachable, widget modes emit empty JSON and exit quickly so the laptop
battery isn't burned on doomed retries.

## Features

Both devices surface the same set of metrics (where available):

- Connection type, operator name, LTE band
- Signal strength (RSRP → bars when the firmware reports `signalStrength: 0`)
- Battery percent + charging state
- Data usage and monthly limit
- WAN IPv4, connected device count
- Power off + reboot

The TUI adds a few things the widgets don't show:

- **RSRP history sparkline** (fixed dBm scale) — watch the signal react
  while physically repositioning the hotspot
- **Live throughput** — reported directly by the M7010; derived from the
  traffic-counter delta on the Mudi
- **Data-limit gauge** and today's usage
- **Stale-data handling** — a failed refresh keeps the last-known data on
  screen with the error and its age in the footer, instead of blanking
  the dashboard

## Modes

| Mode       | Flag              | Output                                       |
| ---------- | ----------------- | -------------------------------------------- |
| TUI        | (default)         | Interactive dashboard, polls every 10s       |
| Waybar     | `--waybar`        | Single JSON line: `{text, tooltip, class}`   |
| Noctalia   | `--noctalia`      | Single JSON line for the noctalia-shell widget |
| Raw dump   | `--raw`           | Pretty-printed raw API responses (per device) |
| Power off  | `--poweroff`      | Shut the router down                         |
| Reboot     | `--reboot`        | Restart the router                           |
| Debug      | `--debug`         | Dumps HTTP traffic (plus crypto for M7010)   |

## Flags

```
--device    m7010 | mudi   (default: autodetect via default gateway, then TCP probe)
--addr      router IP (overrides per-device default)
--pass      admin password (overrides env var and password file)
--waybar    waybar JSON mode
--noctalia  noctalia-shell JSON mode
--raw       dump raw API responses
--poweroff  power the router off and exit
--reboot    reboot the router and exit
--debug     verbose HTTP/crypto logging
--refresh   TUI refresh interval (default 10s)
```

## TUI keybinds

| Key           | Action                                                        |
| ------------- | ------------------------------------------------------------- |
| `r`           | Refresh now                                                   |
| `w`           | Open the router's web UI in the default browser (xdg-open)    |
| `p`, then `p` | Power off (first press arms, second confirms)                 |
| `R`, then `R` | Reboot (first press arms, second confirms)                    |
| `q` / `esc`   | Quit                                                          |

## Password

Each device looks for its password in this order:

1. **`--pass` flag** — fine for one-off debugging, visible in `ps`.
2. **Environment variable** — `M7010_PASS` or `MUDI_PASS` (or `TPLINK_PASS`
   / `GLINET_PASS` — all four are recognised, pick whichever you like).
3. **Password file** under `$XDG_CONFIG_HOME` (default `~/.config`):

   ```
   ~/.config/tplink-m7010/password   # TP-Link M7010
   ~/.config/gl-e5800/password       # GL.iNet Mudi GL-E5800
   ```

   `make install` prints a hint if neither file exists.

   ```sh
   install -d -m700 ~/.config/gl-e5800
   printf '%s' 'your-password' > ~/.config/gl-e5800/password
   chmod 600 ~/.config/gl-e5800/password
   ```

## Installation

```sh
make install            # builds + installs to ~/.local/bin
make install-waybar     # drops the waybar wrapper script in ~/.config/waybar/scripts
```

Other useful targets: `make build`, `make run`, `make raw`, `make clean`,
`make test`, `make vet`, `make tidy`.

## Battery-friendly behaviour

This binary is meant to run on the host laptop on every waybar tick (30s
in the shipped config) without being a battery hog. Two specific choices
keep it cheap:

- **Autodetect prefers the kernel's default gateway** over probing every
  address. Reading `/proc/net/route` is essentially free; the parallel
  probe only happens when the gateway doesn't match any supported device.
  Either way the pick is confirmed with one cheap unauthenticated HTTP
  round-trip before any login is attempted.
- **Widget modes exit silently when no router is reachable.** No login
  attempts, no 5-second timeouts, no retries. Empty JSON makes waybar /
  noctalia hide the module.

If you also want to skip autodetect entirely (e.g. you only ever use one
router), pass `--device m7010` or `--device mudi` from your waybar wrapper.

Two related notes:

- **Address overrides via env** (`M7010_ADDR` / `MUDI_ADDR` / `TPLINK_ADDR`
  / `GLINET_ADDR`) are honoured by autodetect too — the gateway match and
  the probe both use the resolved address, not just the factory default.
- **Widget modes don't log out** (saves a round-trip per tick; the router
  ages sessions out on its own). On the M7010, which allows one active web
  session at a time, this can keep the browser UI locked out until the
  token expires — log in from the TUI or wait a minute if that bites.

## Waybar integration

See `WAYBAR.md` for the setup on this machine (binary path, password file,
wrapper script). With autodetect baked into the binary, the wrapper is now
a single-line `exec`.

## Protocol notes

- `PROTOCOL.md` — TP-Link M7010 wire format (AES+RSA envelope).
- `PROTOCOL_GLINET.md` — GL.iNet Mudi (GL-E5800) JSON-RPC, challenge hash,
  service catalog probed so far.

Both are reverse-engineered and were not trivial — see the docs for the
specific landmines.

## Documentation

| File                  | Covers                                                                 |
| --------------------- | ---------------------------------------------------------------------- |
| `README.md`           | This file — what it is, how to run it                                  |
| `PROTOCOL.md`         | TP-Link M7010 wire format                                              |
| `PROTOCOL_GLINET.md`  | GL.iNet Mudi (GL-E5800) JSON-RPC                                       |
| `ARCHITECTURE.md`     | Code structure, multi-device flow, TUI model                           |
| `WAYBAR.md`           | Waybar setup on this specific machine                                  |
| `PERFORMANCE.md`      | Measured per-tick CPU/RAM/network cost + daily power estimate          |
| `DEVELOPMENT.md`      | Build log: what was tried, what failed, what stuck                     |
| `CLAUDE.md`           | Pointers for future Claude Code sessions                               |

## Files

- `main.go`     — CLI entry, flag parsing, waybar/noctalia/raw/TUI modes
- `device.go`   — `Device` interface, supported-device registry, autodetect
- `client.go`   — TP-Link M7010 client (AES-128-CBC + RSA-PKCS1v15)
- `mudi.go`     — GL.iNet Mudi (GL-E5800) JSON-RPC client
- `ws.go`       — Minimal WebSocket client for the Mudi's `/ws` event stream
- `crypt.go`    — Pure-Go SHA-256 crypt(3) for the GL.iNet challenge/response
- `*_test.go`   — Unit tests: pure helpers, WS frames, probes, and full
  fake-device envelope tests for both protocols (`make test`)
- `Makefile`    — build, install, test, waybar integration
- `contrib/mifi.sh`     — Waybar wrapper (one-line exec, autodetect is in the binary)
- `contrib/mifi-tui.sh` — Opens the TUI in a terminal (waybar tile click)
