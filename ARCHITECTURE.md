# Architecture

Five Go files, one binary. Two router families share the data model and
output formatters; the protocol-specific code is isolated behind the
`Device` interface.

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
     └── parallel TCP probe (500ms per device)
   openDevice(d, ...)      -- resolveAddr + resolvePassword + d.New().Connect()
   dev.Fetch()             -- one or two protocol-specific RPCs → *Status
   (dev.Close()            -- best-effort logout, async-safe)
```

`pickDevice` returns `nil` when no supported router is reachable. Widget
modes (`--waybar` / `--noctalia`) treat that as "emit empty JSON, exit"
so the laptop battery isn't burned on doomed logins.

## Device interface

```go
type Device interface {
    Name() string                   // human label
    Addr() string                   // address we're talking to
    Connect(password string) error  // login
    Fetch() (*Status, error)        // pull live state into Status
    Shutdown() error                // power off
    Reboot() error                  // reboot
    Close()                         // logout, best-effort
}
```

Both `*Client` (M7010) and `*MudiClient` implement it. Adding a third
device is a matter of writing another file and appending an entry to
`supportedDevices` in `device.go`.

## Autodetect (the laptop-battery story)

We try two signals in order:

1. **Default gateway**. If the kernel's default route points at a
   known device IP, that's where our traffic is already going — picking
   it costs ~0ms and is unambiguous.
2. **Parallel TCP probe**. Each device's port 80 is dialed with a
   500ms timeout, in parallel. First reachable wins (in registration
   order, not race order — flapping between two simultaneously-up
   routers is worse than a stable wrong-but-consistent choice).

The gateway-first ordering specifically defends against false positives
on `192.168.0.1`: that address is often "accepted" by upstream NAT but
doesn't respond to HTTP. A pure TCP probe was picking the M7010 even
when only the Mudi was on the LAN — using the gateway as the first
signal makes the right answer free.

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

- Trying multiple field names in order (`firstStr`, `firstInt`,
  `firstFloatish`). Add candidates when a new firmware shows up, don't
  rip out the old ones.
- Mapping numeric enums to strings (`networkTypeStr`) in one place.
- Parsing decimal-string byte counts (e.g. `"14473800628.000000"`)
  through `firstFloatish`, which falls through to numeric types too.

## TUI model (Bubble Tea)

Standard Elm-ish shape, identical to the original single-device path:

- `model` holds `*SupportedDevice`, `*Status`, error, loading flag,
  refresh interval, pending power-action state.
- `Init` kicks off `fetchCmdFor(d)` plus a `tickCmd`.
- `tickMsg` every `--refresh` seconds triggers another `fetchCmdFor`.
- `r` re-fetches; `q/esc/ctrl+c` quits; `p`-then-`p` / `R`-then-`R`
  power-off / reboot with a two-press confirmation.
- The TUI holds a single logged-in `Device` across ticks (in
  `tuiDevice`); on any error the device is dropped and the next tick
  reconnects.

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
