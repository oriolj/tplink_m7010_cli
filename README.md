# tplink-m7010

Go tool to query a mobile Wi-Fi hotspot and display its state as a Bubble Tea
TUI, a waybar JSON module, or a noctalia-shell CustomButton widget.

Despite the name, the binary supports three hotspot models with the
same code:

| Family             | Model        | Default IP     | Wire format                       |
| ------------------ | ------------ | -------------- | --------------------------------- |
| TP-Link            | M7010        | `192.168.0.1`  | AES-128-CBC + RSA-PKCS1v15 envelope |
| TP-Link            | M7450        | `192.168.0.1`  | AES-128-CBC + RSA-PKCS1v15 envelope |
| GL.iNet (OpenWrt)  | Mudi GL-E5800 | `192.168.8.1` | JSON-RPC with SHA-256-crypt + sid |

The same binary autodetects which device is on the LAN — it prefers the
default-gateway match and falls back to probing the known addresses in parallel.
Both signals are confirmed with a cheap unauthenticated **protocol probe**
(the TP-Link hello / the Mudi challenge), so a home router that happens to
live at 192.168.0.1 is not mistaken for a hotspot. If neither hotspot is
reachable, widget modes emit empty JSON and exit quickly so the laptop
battery isn't burned on doomed retries.

## Features

All devices surface the same set of metrics (where available):

- Connection type, operator name, LTE band
- Signal strength (RSRP → bars when the firmware reports `signalStrength: 0`)
- Battery percent + charging state, plus an **estimated remaining time**
  (see "Remaining battery time" below)
- Data usage and monthly limit
- WAN IPv4, connected device count
- Power off + reboot

The TUI adds a few things the widgets don't show:

- **RSRP history sparkline** (fixed dBm scale) — watch the signal react
  while physically repositioning the hotspot
- **Live throughput** — reported directly by the TP-Link models; derived from the
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
| JSON       | `--json`          | Parsed status as machine-readable JSON, for scripts |
| Raw dump   | `--raw`           | Pretty-printed raw API responses (per device) |
| Power off  | `--poweroff`      | Shut the router down                         |
| Reboot     | `--reboot`        | Restart the router                           |
| Debug      | `--debug`         | Dumps HTTP traffic (plus crypto for TP-Link) |

## Flags

```
--device    m7010 | m7450 | mudi   (default: autodetect via default gateway, then protocol probe)
--addr      router IP (overrides per-device default)
--pass      admin password (overrides env var and password file)
--waybar    waybar JSON mode
--noctalia  noctalia-shell JSON mode
--json      parsed status as JSON (stable scripting interface)
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
2. **Environment variable** — `M7010_PASS`, `M7450_PASS`, or `MUDI_PASS`
   (the family aliases `TPLINK_PASS` / `GLINET_PASS` also work).
3. **Password file** under `$XDG_CONFIG_HOME` (default `~/.config`):

   ```
   ~/.config/tplink-m7010/password   # TP-Link M7010
   ~/.config/tplink-m7450/password   # TP-Link M7450
   ~/.config/gl-e5800/password       # GL.iNet Mudi GL-E5800
   ```

   M7450 also falls back to the M7010 password file for compatibility
   with installations that used the shared TP-Link protocol before the
   model became a distinct device ID.

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

## Remaining battery time

Neither router reports a time — only an integer percent and a charging
flag. The remaining time is therefore derived from how fast that percent
moves, which needs history, which a one-shot CLI does not have. So this is
the single piece of state the tool keeps:

```
$XDG_STATE_HOME/tplink-m7010/battery.json   # ~/.local/state/... by default
```

It holds one entry per distinct percent seen (per device), which is a few
KB at most. Delete it freely — you lose the current estimate, nothing else.
`TPLINK_STATE_DIR` overrides the location.

How the number is arrived at:

- **Edges, not samples.** A reading of 88% says nothing about where inside
  that percent you are, so the rate is measured between the *instants the
  percent changed*. Between two such edges the drop is exactly N percent
  and the only error left is the poll interval.
- **A warm-up gate.** One percent step is a sample size of one, so the
  in-session measurement is not trusted until 3 steps have been observed
  (~20 min at a typical rate).
- **A learned rate covers the warm-up.** Every step observed is banked
  into a pooled average for that router, which *survives* the window
  resets below. So from the second session onwards the cold start is
  "what this unit actually averages", not a vendor claim — labelled
  **`(avg)`**.
- **The datasheet is the last resort**, used only before anything has
  been learned: 8 h for the M7010, 15 h for the M7450, and 13.5 h for
  the Mudi 7. Labelled
  **`(typical)`** so a guess never reads as a measurement. It also stays
  on as a weak prior inside the pool, so one unusually idle session
  cannot swing the estimate wholesale.
- **Continuity checks.** The *window* resets when the charger is plugged
  or unplugged, when polling has been silent for over 20 min (a router
  that was *off* looks identical to one discharging very slowly, and that
  reading inflates the estimate), and on any double-digit jump. The
  *learning* survives all three: a reset means "I cannot measure across
  this discontinuity", not "I know nothing about this router".
- **Charging** shows time-to-full. There is no datasheet bootstrap (no
  vendor publishes a charge time), so it appears once measured — and from
  the next session on, straight away.

So the estimate degrades in three steps, best-known first:

| Source      | Shown as     | Means                                            |
| ----------- | ------------ | ------------------------------------------------ |
| `measured`  | `~4h12m`     | this session's own rate — reflects current load  |
| `learned`   | `~4h12m (avg)`  | this router's average across past sessions    |
| `typical`   | `~4h12m (typical)` | the vendor's datasheet runtime             |

### Is that battery health?

Not quite, and `--json` says so in a comment. `battery_learned` reports
`observed_runtime_hours` — how long a full charge has actually lasted
**under your usage** — which mixes cell ageing with how hard the router
was worked (five clients streaming on a weak signal drain a healthy
battery fast). Separating the two needs a full-charge capacity readout,
which neither device exposes, or a controlled-load test. Compare it
against `typical_runtime_hours` as a **trend over months**, not as a
health percentage.

Note also what the pool implicitly selects for: samples only accrue while
something is polling, i.e. while your laptop is awake behind the router.
What is learned is therefore the rate *under use*, not idle standby —
which is the rate you want when you ask how long it will last.

Where it shows: the TUI dashboard as its own `Remaining` / `To full` row,
the waybar and noctalia **tooltips**, and `--json` as `battery_estimate`.
Deliberately **not** on the bar tile itself, which stays the glanceable
four-field line.

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
router), pass `--device m7010`, `--device m7450`, or `--device mudi` from
your waybar wrapper.

Two related notes:

- **Address overrides via env** (`M7010_ADDR` / `M7450_ADDR` / `MUDI_ADDR`
  / `TPLINK_ADDR` / `GLINET_ADDR`) are honoured by autodetect too — the gateway match and
  the probe both use the resolved address, not just the factory default.
- **Widget modes don't log out** (saves a round-trip per tick; the router
  ages sessions out on its own). On the TP-Link models, which allow one active web
  session at a time, this can keep the browser UI locked out until the
  token expires — log in from the TUI or wait a minute if that bites.

## Waybar / noctalia integration

See `WAYBAR.md` and `NOCTALIA.md` for the setup on this machine (binary
path, password file, wrapper script, widget settings). With autodetect
baked into the binary, the waybar wrapper is now a single-line `exec`.

Heads-up for noctalia: the exec-JSON CustomButton this feeds exists in
the QML v4.x series; the v5 C++ rewrite replaced it with a plugin
system. `--json` is the version-independent interface to build on —
see `NOCTALIA.md`.

## Protocol notes

- `PROTOCOL.md` — TP-Link M7010/M7450 wire format (AES+RSA envelope).
- `PROTOCOL_GLINET.md` — GL.iNet Mudi (GL-E5800) JSON-RPC, challenge hash,
  service catalog probed so far.

Both are reverse-engineered and were not trivial — see the docs for the
specific landmines.

## Documentation

| File                  | Covers                                                                 |
| --------------------- | ---------------------------------------------------------------------- |
| `README.md`           | This file — what it is, how to run it                                  |
| `PROTOCOL.md`         | TP-Link M7010/M7450 wire format                                        |
| `PROTOCOL_GLINET.md`  | GL.iNet Mudi (GL-E5800) JSON-RPC                                       |
| `ARCHITECTURE.md`     | Code structure, multi-device flow, TUI model                           |
| `WAYBAR.md`           | Waybar setup on this specific machine                                  |
| `NOCTALIA.md`         | Noctalia CustomButton setup + v4/v5 version caveat                     |
| `PERFORMANCE.md`      | Measured per-tick CPU/RAM/network cost + daily power estimate          |
| `DEVELOPMENT.md`      | Build log: what was tried, what failed, what stuck                     |
| `CLAUDE.md`           | Pointers for future Claude Code sessions                               |

## Files

- `main.go`     — CLI entry, flag parsing, waybar/noctalia/raw/TUI modes
- `device.go`   — `Device` interface, supported-device registry, autodetect
- `client.go`   — TP-Link M7010/M7450 client (AES-128-CBC + RSA-PKCS1v15)
- `mudi.go`     — GL.iNet Mudi (GL-E5800) JSON-RPC client
- `ws.go`       — Minimal WebSocket client for the Mudi's `/ws` event stream
- `crypt.go`    — Pure-Go SHA-256 crypt(3) for the GL.iNet challenge/response
- `battery.go`  — Remaining-time estimate: percent history, edge-anchored
  rate, datasheet fallback
- `*_test.go`   — Unit tests: pure helpers, WS frames, probes, and full
  fake-device envelope tests for both protocols (`make test`)
- `Makefile`    — build, install, test, waybar integration
- `contrib/mifi.sh`     — Waybar wrapper (one-line exec, autodetect is in the binary)
- `contrib/mifi-tui.sh` — Opens the TUI in a terminal (waybar tile click)
