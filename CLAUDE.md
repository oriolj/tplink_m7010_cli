# CLAUDE.md

Notes for future Claude sessions working on this repo.

## What this is

A Go CLI that talks to **two** mobile-Wi-Fi hotspot families over their
LAN web APIs and renders the state as a Bubble Tea TUI, a waybar JSON
module, or a noctalia-shell CustomButton widget. Tiny scope — no daemon,
no caching.

Supported devices:

| ID      | Model               | Default IP    | Transport                       |
| ------- | ------------------- | ------------- | ------------------------------- |
| `m7010` | TP-Link M7010       | `192.168.0.1` | undocumented AES+RSA envelope   |
| `mudi`  | GL.iNet Mudi GL-E5800 | `192.168.8.1` | OpenWrt JSON-RPC at `/rpc`     |

Same binary; the right protocol is picked by autodetect (default-gateway
match first, parallel TCP probe as fallback) or via `--device <id>`.

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
4. **`battery.voltage` is the percent**, not a voltage.
5. **`signalStrength` is always 0.** Use `rsrp` and map to 0-5 bars
   yourself (`rsrpToSignal`).
6. **Responses are base64.** Step-1 is base64 JSON; subsequent are
   base64 AES ciphertext. Both look alike — don't confuse them.

### Mudi (GL.iNet GL-E5800)

1. **Args must be a JSON array**. `[]` for nullary methods. `{}` (the
   obvious Go default) returns `Invalid params` even when the method
   exists.
2. **Battery is `system.get_status.system.mcu.charge_percent`**, not a
   dedicated `battery` service. `mcu.charging_status > 0` means
   charging.
3. **Modem details only show up when a SIM is active.**
   `modem.get_modems_info` returns `[]` otherwise, so the field-name
   guessing in `parseMudiModem` is untested in CI — list multiple
   candidates (`carrier`, `operator_name`, `operator`, …) rather than
   guessing one.
4. **Autodetect's first signal is the default gateway**, not a TCP
   probe. The M7010's default IP (192.168.0.1) is often accepted as
   "open" through upstream NAT but doesn't actually answer HTTP — a
   pure TCP probe would pick the wrong device. See `defaultGateway()`
   in `device.go`.
5. **The 4.x API docs are offline.** Don't trust forum posts older
   than ~2024-01 — methods and service names have drifted. When in
   doubt, probe the device with `--debug`.

## Where things live

- `client.go`   — M7010 HTTP + crypto + response parsing. `Client`
                  type implements the `Device` interface.
- `mudi.go`     — Mudi JSON-RPC client. `MudiClient` implements the
                  `Device` interface.
- `device.go`   — `Device` interface, supported-device registry,
                  autodetect (gateway + parallel TCP fallback),
                  password/address resolution.
- `crypt.go`    — Pure-Go SHA-256 crypt(3) implementation for the
                  Mudi challenge response. Cross-checked against
                  `openssl passwd -5`.
- `main.go`     — Flag parsing, mode dispatch (`runTUI`, `runWaybar`,
                  `runNoctalia`, `runRaw`, `runPower`). All modes go
                  through `pickDevice() + openDevice() + dev.Fetch()`.
- `Makefile`    — `build / install / install-waybar / run / raw / vet / tidy`.
- `PROTOCOL.md` — TP-Link M7010 wire format.
- `PROTOCOL_GLINET.md` — GL.iNet Mudi (GL-E5800) JSON-RPC.
- `WAYBAR.md`   — This machine's setup for the waybar tile.
- `contrib/mifi.sh` — Waybar wrapper (now a one-line exec; autodetect
                  lives in the binary).

## Testing loop

```sh
make raw                                # whichever device is on the LAN
./tplink-m7010 --device mudi --raw      # force Mudi
./tplink-m7010 --device m7010 --raw     # force M7010
./tplink-m7010 --debug --raw            # show HTTP traffic (M7010 includes crypto)
```

There's no unit test suite — everything interesting is an integration
against a physical device and none of us has a reliable simulator. The
one exception is `crypt.go`: `sha256Crypt` can be cross-checked against
`openssl passwd -5 -salt SALT KEY` in a few seconds.

Add tests for the helpers (`firstStr`, `firstInt`, `rsrpToSignal`,
`networkTypeStr`, `parseFloatStr`, `defaultGateway`, `sha256Crypt`) if
you're editing them. Leave the crypto envelope uncovered unless you
want to record/replay a real session.

**Live testing tips:**

- **Don't poke `system.reboot` / `system.poweroff` against a real device
  unless you can plug it back in by hand.** They appear to fire instantly
  on the Mudi.
- The M7010 `Shutdown` command requires a physical button press to wake
  the modem back up — same precaution.

## Commit style

Short subject, then a paragraph explaining the why. See the existing
commits for the register.
