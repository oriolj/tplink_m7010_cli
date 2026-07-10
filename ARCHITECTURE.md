# Architecture

Six Go files, one binary. Two router families share the data model and
output formatters; the protocol-specific code is isolated behind the
`Device` interface.

| File        | Role                                                        |
| ----------- | ----------------------------------------------------------- |
| `main.go`   | Flags, mode dispatch, TUI, waybar/noctalia formatters       |
| `device.go` | `Device` interface, device registry, autodetect, passwords  |
| `client.go` | TP-Link M7010 client (AES+RSA envelope)                     |
| `mudi.go`   | GL.iNet Mudi client (JSON-RPC + WebSocket event collection) |
| `ws.go`     | Minimal hand-rolled WebSocket client used by `mudi.go`      |
| `crypt.go`  | SHA-256 crypt(3) for the Mudi challenge/response            |

## Process flow

```
main.go :: main()
   ├── flag.Parse()              -- CLI flags
   ├── --poweroff → runPower("shutdown")
   ├── --reboot   → runPower("reboot")
   ├── --waybar   → runWaybar()
   ├── --noctalia → runNoctalia()
   ├── --raw      → runRaw()
   └── (default)  → runTUI()

every mode goes through:
   pickDevice()            -- explicit --device, or detectDevice()
     ├── defaultGateway()  -- /proc/net/route → IP; instant
     │     └── confirmed by d.Probe() (unauthenticated protocol hello)
     └── parallel d.Probe() of every resolved address (500ms timeout)
   openDevice(d, ...)      -- resolveAddr + resolvePassword + d.New().Connect()
   dev.Fetch()             -- protocol-specific RPCs (Mudi: RPC + WS) → *Status
   (dev.Close()            -- best-effort logout, async-safe)
```

`pickDevice` returns `nil` when no supported router is reachable. Widget
modes (`--waybar` / `--noctalia`) treat that as "emit empty JSON, exit"
so the laptop battery isn't burned on doomed logins.

## Device interface

```go
type Device interface {
    Name() string                   // human label
    Connect(password string) error  // login
    Fetch() (*Status, error)        // pull live state into Status
    Shutdown() error                // power off
    Reboot() error                  // reboot
    Close()                         // logout, best-effort
}
```

Both `*Client` (M7010) and `*MudiClient` implement it. Adding a third
device is a matter of writing another file and appending an entry to
`supportedDevices` in `device.go` — the full checklist is at the end of
this document.

## Autodetect (the laptop-battery story)

We try two signals in order:

1. **Default gateway**. If the kernel's default route points at a
   known device address (env overrides included), that's where our
   traffic is already going — the cheapest possible hint.
2. **Parallel protocol probe**. Every device's resolved address is
   probed in parallel with a 500ms timeout. First hit wins in
   registration order, not race order — flapping between two
   simultaneously-up routers is worse than a stable choice.

Both signals are confirmed by the device's `Probe` function — one
unauthenticated HTTP round-trip that checks the *protocol*, not just
TCP reachability (`probeM7010` sends the step-1 hello and looks for
the nonce; `probeMudi` sends a `challenge` and looks for salt+nonce).

This matters twice over:

- `192.168.0.1:80` is often "accepted" by upstream NAT without anything
  answering HTTP — a bare TCP probe used to pick the M7010 when only
  the Mudi was on the LAN.
- `192.168.0.1` is also the most common *home router* gateway address.
  Without the protocol check, any such LAN made autodetect claim an
  M7010 was present, and the widget rendered a login-error tile instead
  of collapsing.

## Client state

### `Client` (M7010)

```
addr         "192.168.0.1"
baseURL      "http://192.168.0.1"
password     "…"               -- kept because every request signs md5('admin'+pass)
token        "SSnASCGDZclim6kp"
aesKey/aesIV [16]byte digits   -- random per login, reused this session
rsaMod       *big.Int           -- server RSA modulus from step-1
rsaPubKey    *big.Int           -- server RSA exponent (usually 0x10001)
seqNum       int                -- from step-1, never incremented
httpClient   *http.Client       -- 5s timeout
debug        bool
```

### `MudiClient` (GL.iNet)

```
addr         "192.168.8.1"
httpClient   *http.Client       -- 5s timeout
debug        bool
sid          "TrOZShEm34hgSPHzPWJZEz0ecbDnQAZS"  -- from login response
```

The Mudi's wire format is plaintext JSON over plain HTTP — every
authenticated call carries the `sid` in `params[0]`. No per-session
key material is held client-side.

## Request helpers

### M7010 (`client.go`)

- `postRaw(endpoint, body)` — send bytes, read bytes, log if `debug`.
- `encryptedRequest(endpoint, payload)` — build `{data, sign}` envelope
  (AES-128-CBC + RSA-PKCS1v15), POST, decrypt, parse as JSON. All
  post-login calls go through this.

### Mudi (`mudi.go`)

- `rpc(method, params)` — generic JSON-RPC envelope around `POST /rpc`.
- `callSvc(service, method, args)` — auth-aware wrapper that prepends
  the `sid` and emits the `[sid, service, method, args]` shape every
  authenticated call needs.

## Status struct

A flat container shared by both protocols:

```
Model, Firmware, Operator        -- device labels
NetworkType, Band, ConnectStatus -- WAN
SignalStrength, RSRP, RSRQ,
  RSSI, SNR                       -- coverage (M7010: signalStrength=0,
                                                derived from RSRP)
WanIP                            -- WAN IPv4
BatteryPercent, BatteryCharging  -- battery
TxSpeed, RxSpeed                 -- current throughput
TotalBytes, DailyBytes,
  AdjustedBytes, MonthLimitBytes,
  PaymentDay                      -- data usage
ConnectedDevices                 -- LAN client count
RawStatus, RawFlowStat           -- raw JSON for --raw / debugging
```

Fields that one protocol doesn't fill are left as the zero value; the
TUI / waybar formatter skips them based on truthiness.

## Response parsing

Both clients tolerate firmware drift by:

- Reading fields through nil-safe helpers (`jsonStr`, `jsonInt`,
  `jsonBool`, `jsonFloatStr`, `subMap`) instead of raw type asserts.
- Mapping numeric enums to strings (`networkTypeStr`,
  `friendlyNetworkType`) in one place.
- Parsing decimal-string byte counts (e.g. `"14473800628.000000"`)
  through `jsonFloatStr`, which falls through to JSON numbers too.

## Mudi WebSocket path (`ws.go`)

The Mudi 7's CPU-integrated modem exposes **no `/rpc` method** for
cellular state — signal, operator, and traffic only arrive as pushed
events on `ws://<addr>/ws?sid=<sid>` (see PROTOCOL_GLINET.md for the
discovery story and event schema). `MudiClient.Fetch` therefore runs
two collectors in parallel:

- `system.get_status` over `/rpc` (battery, uptime, temps, clients);
- `collectCellular` over `/ws` — subscribe to the three `cellular.*`
  topics, read until all three arrived or a 2s deadline elapses.

`ws.go` is a deliberately tiny hand-rolled client (handshake, read
frames, masked writes, ping→pong, 1MB frame-size cap) — receive-mostly,
no gorilla/websocket dependency. The WS failing is non-fatal: the
widget just shows the RPC-derived fields.

## TUI model (Bubble Tea)

Standard Elm-ish shape:

- `model` holds `*SupportedDevice`, `*Status`, error, loading flag,
  refresh interval, pending power-action state — plus the cross-tick
  display state: an RSRP ring buffer for the sparkline, the previous
  traffic-counter sample for throughput derivation, and the
  last-successful-fetch timestamp for the stale indicator.
- `Init` kicks off `fetchCmdFor(d)` plus a `tickCmd`.
- `tickMsg` every `--refresh` seconds triggers another `fetchCmdFor`
  (skipped when a fetch is already in flight).
- `r` re-fetches; `q/esc/ctrl+c` quits; `p`-then-`p` / `R`-then-`R`
  power-off / reboot with a two-press confirmation.
- The TUI holds a single logged-in `Device` across ticks (in
  `tuiDevice`); on any error the device is dropped and the next tick
  reconnects.
- **Errors don't blank the dashboard**: once a fetch has succeeded, a
  failing tick keeps the last-known data on screen and reports the
  error plus its age in the footer ("showing data from 40s ago").
- **Signal history**: RSRP samples render as a sparkline on a fixed
  -125…-75 dBm scale (same span as `rsrpToSignal`), so the shape is
  comparable across sessions — useful when physically repositioning
  the hotspot.
- **Throughput**: the M7010 reports split rx/tx speeds directly; the
  Mudi only exposes a total-traffic counter, so `computeRate` derives
  a combined rate from the per-tick delta (0 on counter resets).
- Colors are `lipgloss.AdaptiveColor` pairs, so the dashboard is
  legible on light terminals too.

## Waybar / noctalia output

Single line of JSON on stdout:

- waybar: `{text, tooltip, class, alt}` — `class` is one of `good /
  warning / critical / disconnected`, used by `style.css` to colorize.
- noctalia: `{text, tooltip, textColor}` — `textColor` is one of
  `primary / secondary / tertiary / error / none`, used by the
  noctalia-shell CustomButton widget.

Both build their text + tooltip from the same `formatStatusLine`
helper so the two outputs can't drift.

When no router is reachable, both modes emit an empty JSON object
(`{"text":"","tooltip":""…}`) and exit zero — that hides the widget
without requiring a wrapper to handle the silence.

## Adding a new device — checklist

The interface is small on purpose; everything protocol-specific stays in
one new file. In order:

1. **Reverse-engineer first, code second.** Write the wire format down in
   a `PROTOCOL_<VENDOR>.md` *as you discover it* — both existing protocol
   docs exist because the knowledge evaporates otherwise. Capture real
   request/response samples (redact IMEI/ICCID/passwords).
2. **`<device>.go`** — a client type implementing `Device` (Name,
   Connect, Fetch, Shutdown, Reboot, Close). Parse responses through the
   nil-safe helpers (`jsonStr`, `jsonInt`, `jsonFloatStr`, `subMap`) and
   map onto the shared `Status` struct; leave fields the device can't
   fill at their zero value — the formatters skip them.
3. **A `probe<Device>` function** — one *unauthenticated* HTTP round-trip
   that proves the address speaks this protocol (a hello, a challenge, a
   version endpoint). Bare TCP reachability is not a signal; see the
   autodetect section above for why.
4. **Registry entry in `supportedDevices`** (`device.go`): ID, Title,
   DefaultAddr, `AddrEnvs` + `PasswordEnvs` (primary spelling first),
   `PasswordPath` under `~/.config/`, `New`, `Probe`.
5. **Tests** — a fake-device test in the style of
   `m7010_envelope_test.go` / `mudi_envelope_test.go` (httptest server
   speaking the server side of the protocol), plus unit tests for any
   new pure helpers. Probe tests included: real hello accepted, generic
   HTTP server rejected.
6. **Docs** — README (device table, password section), CLAUDE.md
   (landmines you hit; there will be some), and a Makefile
   `CONFDIR_<DEV>` + password-hint line in the `install` target.
7. **Live-test safety** — don't fire Shutdown/Reboot against hardware
   you can't physically restart. Add any new footguns to CLAUDE.md's
   "Live testing tips".
