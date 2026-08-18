# CLAUDE.md

Notes for future Claude sessions working on this repo.

## What this is

A Go CLI that talks to **two** mobile-Wi-Fi hotspot families over their
LAN web APIs and renders the state as a Bubble Tea TUI, a waybar JSON
module, or a noctalia-shell CustomButton widget. Tiny scope — no daemon,
and no caching of device responses.

The one exception to "no state": `battery.go` keeps a percent history in
`$XDG_STATE_HOME/tplink-m7010/battery.json`. It has to. Neither device
reports a remaining time, so the only way to get one is to measure how
fast the percent moves, and a one-shot process cannot do that from a
single reading. Everything in that file is disposable — deleting it costs
one warm-up window.

Supported devices:

| ID      | Model               | Default IP    | Transport                       |
| ------- | ------------------- | ------------- | ------------------------------- |
| `m7010` | TP-Link M7010       | `192.168.0.1` | undocumented AES+RSA envelope   |
| `mudi`  | GL.iNet Mudi GL-E5800 | `192.168.8.1` | OpenWrt JSON-RPC at `/rpc`     |

Same binary; the right protocol is picked by autodetect (default-gateway
match first, parallel probe as fallback — both confirmed by a cheap
unauthenticated *protocol* probe, see `Probe` in `device.go`) or via
`--device <id>`.

Passwords come from one of:

- `--pass` flag
- `M7010_PASS` / `MUDI_PASS` env vars (or `TPLINK_PASS` / `GLINET_PASS` —
  all four are accepted, none is preferred)
- `$XDG_CONFIG_HOME/tplink-m7010/password` or `$XDG_CONFIG_HOME/gl-e5800/password`
  (both are first-class — pick whichever path matches the device you have)

`--waybar` / `--noctalia` modes emit an **empty JSON object** when no
supported router is reachable so the widget collapses, without paying a
login-timeout cost.

## Before touching the M7010 crypto

Read `PROTOCOL.md` end-to-end. The wire format is not obvious and the
three public reverse-engineering projects (M7350 C++, M7200 PHP, M7010 —
no published impl before this one) use **different** transports. The
M7010 is closest to the M7200 PHP library; the M7350 one (plain JSON +
token) does not work.

## Before touching the Mudi RPC

Read `PROTOCOL_GLINET.md`. The official GL.iNet 4.x API docs have been
offline since early 2024, so what we know is from `python-glinet`, the
gl-sdk4-* package list, and live probing. Key things:

- Auth is challenge/response, **not** session-cookie. Hash is
  `sha256(username + ":" + sha256-crypt(password, "$5$salt$") + ":" + nonce)`.
- Authenticated calls are `{"method":"call","params":[sid, service,
  method, args]}` where `args` **must be an array** (`[]` for nullary).
  An object `{}` is rejected as "Invalid params".
- `system.reboot` / `system.poweroff` return `result: []` and may need
  confirmation params we haven't reverse-engineered. They appear to work
  but are flagged as such in `mudi.go`.

## Things that were surprisingly hard and will bite again

### M7010 (TP-Link)

1. **Go `crypto/rsa` refuses <1024-bit keys**. The M7010 ships a
   512-bit key. `rsaEncryptBlock` in `client.go` is a hand-rolled
   PKCS1v15 using `math/big`. Don't replace it with
   `rsa.EncryptPKCS1v15` — it will fail at runtime, not compile time.
2. **Sign string parameter order is load-bearing**. Go's
   `url.Values.Encode()` sorts alphabetically; PHP's `http_build_query`
   preserves insertion order. The M7010 firmware position-parses the
   sign string. Always build it manually: `key=…&iv=…&h=…&s=…` for
   login; `h=…&s=…` afterwards.
3. **RSA message > key size → chunk, don't fail.** The sign string
   (~87 bytes) is larger than one 53-byte PKCS1v15 block. phpseclib
   auto-chunks; we mirror that.
4. **`battery.voltage` is the percent**, not a voltage. Integer, 1% steps
   — which is why the remaining-time estimate anchors on transitions
   rather than samples (see below).
5. **`signalStrength` is always 0.** Use `rsrp` and map to 0-5 bars
   yourself (`rsrpToSignal`).
6. **Responses are base64.** Step-1 is base64 JSON; subsequent are
   base64 AES ciphertext. Both look alike — don't confuse them.

### Remaining battery time

1. **Both devices report an integer percent and nothing else** — no
   current, no voltage, no time. The estimate is derived, so treat
   `TypicalRuntime` in the device registry as a documented datasheet
   figure (M7010 8 h, Mudi 7 13.5 h), not a measurement.
2. **Measure between edges, not samples.** 88% is an interval, not a
   point: two arbitrary samples 1% apart carry a ±100% rate error.
   `measuredRate` skips `runs[0]` on purpose — we joined that percent
   partway through, so its timestamp is when we started looking, not
   when the percent was entered.
3. **A poll gap is not slow discharge.** A router that was switched off
   looks exactly like one barely discharging, and that reading inflates
   the estimate. Anything over `batteryGapMax` resets the history.
4. **Don't add a charging bootstrap** without a source: neither vendor
   publishes a charge time, so time-to-full stays silent until measured.

### Mudi (GL.iNet GL-E5800)

1. **Args must be a JSON array**. `[]` for nullary methods. `{}` (the
   obvious Go default) returns `Invalid params` even when the method
   exists.
2. **Battery is `system.get_status.system.mcu.charge_percent`**, not a
   dedicated `battery` service. `mcu.charging_status > 0` means
   charging.
3. **Cellular state is NOT on `/rpc` at all.** On the Mudi 7 the modem
   is CPU-integrated, so `modem.get_modems_info` always returns `[]`
   (it enumerates USB modems). Signal, operator, and traffic arrive as
   pushed events on the WebSocket at `/ws` — `collectCellular` in
   `mudi.go` + the hand-rolled client in `ws.go`. Read the
   "Where the cellular signal actually lives" section of
   PROTOCOL_GLINET.md before assuming an RPC method exists.
4. **Autodetect's first signal is the default gateway**, confirmed by
   an unauthenticated protocol probe (`probeM7010` / `probeMudi`). A
   bare TCP probe is not enough twice over: 192.168.0.1 is often
   accepted through upstream NAT without answering HTTP, and it's also
   the most common home-router gateway IP. See `detectDevice()` in
   `device.go`.
5. **The 4.x API docs are offline.** Don't trust forum posts older
   than ~2024-01 — methods and service names have drifted. When in
   doubt, probe the device with `--debug`.

## Where things live

- `client.go`   — M7010 HTTP + crypto + response parsing. `Client`
                  type implements the `Device` interface. Also
                  `probeM7010` for autodetect.
- `mudi.go`     — Mudi JSON-RPC client + WS cellular collection.
                  `MudiClient` implements the `Device` interface.
                  Also `probeMudi` for autodetect.
- `ws.go`       — Minimal hand-rolled WebSocket client (handshake,
                  frame read/write, masked sends, 1MB frame cap) for
                  the Mudi's `/ws` event stream.
- `device.go`   — `Device` interface, supported-device registry,
                  autodetect (gateway + parallel protocol probe),
                  password/address resolution.
- `crypt.go`    — Pure-Go SHA-256 crypt(3) implementation for the
                  Mudi challenge response. Cross-checked against
                  `openssl passwd -5`.
- `battery.go`  — Remaining-time estimate. Percent history in
                  XDG_STATE_HOME, edge-anchored rate, datasheet
                  fallback while warming up. Read its file header
                  before changing any constant there: the warm-up
                  gate and the reset rules are what keep the number
                  from being confidently wrong.
- `main.go`     — Flag parsing, mode dispatch (`runTUI`, `runWaybar`,
                  `runNoctalia`, `runJSON`, `runRaw`, `runPower`). All
                  modes go through `pickDevice() + openDevice() +
                  dev.Fetch()`. `--json` is the stable scripting
                  interface — prefer extending it over inventing new
                  widget-specific output modes.
- `*_test.go`   — Unit tests for the pure helpers, probes, crypt, and
                  the WS frame parser. `make test`.
- `Makefile`    — `build / install / install-waybar / run / raw / test / vet / tidy`.
- `PROTOCOL.md` — TP-Link M7010 wire format.
- `PROTOCOL_GLINET.md` — GL.iNet Mudi (GL-E5800) JSON-RPC + WS events.
- `ARCHITECTURE.md` — Code structure, autodetect design, WS path.
- `DEVELOPMENT.md`  — Build log: what was tried, what failed, what stuck.
- `PERFORMANCE.md`  — Measured per-tick CPU/RAM/network cost.
- `WAYBAR.md`   — This machine's setup for the waybar tile.
- `NOCTALIA.md` — Noctalia CustomButton setup, output contract, and the
                  v4 (QML, exec-JSON works) vs v5 (C++, plugins only)
                  version caveat.
- `contrib/mifi.sh` — Waybar wrapper (now a one-line exec; autodetect
                  lives in the binary).

## Testing loop

```sh
make raw                                # whichever device is on the LAN
./tplink-m7010 --device mudi --raw      # force Mudi
./tplink-m7010 --device m7010 --raw     # force M7010
./tplink-m7010 --debug --raw            # show HTTP traffic (M7010 includes crypto)
```

`make test` runs the unit suite: `sha256Crypt` (openssl-verified
vectors), the pure helpers (`rsrpToSignal`, `networkTypeStr`,
`friendlyNetworkType`, `formatUptime`, `pickActiveSlot`, `jsonFloatStr`,
`intFrom`, `parseDefaultGateway`, PKCS#7), the WS frame parser (byte
fixtures), and the autodetect probes (httptest).

Both protocol envelopes are covered end-to-end by **fake-device tests**:

- `m7010_envelope_test.go` — an httptest server implementing the M7010
  server side (real 512-bit RSA decrypt, strict sign-string *order*
  validation, AES round-trip, digest check). If you reorder the sign
  string or touch the crypto, this fails the way the hardware would.
- `mudi_envelope_test.go` — fake `/rpc` (challenge/login with real
  sha256-crypt verification, args-must-be-array enforcement) plus a
  real WS handshake on `/ws` pushing the three cellular events.

Extend the fakes when you add fields — put the new field in the canned
response and assert it on `Status`. Live devices are still the final
word for anything the fakes only *assume* (firmware quirks, timing).
CI (`.github/workflows/ci.yml`) runs gofmt + vet + test + build on
every push/PR.

**Live testing tips:**

- **Don't poke `system.reboot` / `system.poweroff` against a real device
  unless you can plug it back in by hand.** They appear to fire instantly
  on the Mudi.
- The M7010 `Shutdown` command requires a physical button press to wake
  the modem back up — same precaution.

## Commit style

Short subject, then a paragraph explaining the why. See the existing
commits for the register.
