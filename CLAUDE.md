# CLAUDE.md

Notes for future Claude sessions working on this repo.

## What this is

A Go CLI that talks to a TP-Link M7010 mobile Wi-Fi hotspot over its
undocumented LAN web API. Two output modes: a Bubble Tea TUI and a waybar
JSON module. Tiny scope — no config file, no daemon, no caching.

## Before touching the crypto

Read `PROTOCOL.md` end-to-end. The wire format is not obvious and the three
public reverse-engineering projects (M7350 C++, M7200 PHP, M7010 — no
published impl before this one) use **different** transports. The M7010 is
closest to the M7200 PHP library; the M7350 one (plain JSON + token) does not
work.

## Things that were surprisingly hard and will bite again

1. **Go `crypto/rsa` refuses <2048-bit keys**. The M7010 ships a 512-bit key.
   `rsaEncryptBlock` in `client.go` is a hand-rolled PKCS1v15 using
   `math/big`. Don't replace it with `rsa.EncryptPKCS1v15` — it will fail at
   runtime, not compile time.
2. **Sign string parameter order is load-bearing**. Go's `url.Values.Encode()`
   sorts alphabetically; PHP's `http_build_query` preserves insertion order.
   The M7010 firmware apparently position-parses the sign string, because
   alphabetical order returns garbage AES. Always build the sign string
   manually: `key=…&iv=…&h=…&s=…` for login; `h=…&s=…` afterwards.
3. **RSA message > key size → chunk, don't fail.** The sign string (~87
   bytes) is larger than one 53-byte PKCS1v15 block. phpseclib auto-chunks;
   we mirror that. Each chunk produces `keyLen` bytes of ciphertext; output
   is the concatenation, hex-encoded.
4. **`battery.voltage` is the percent**, not a voltage. The field is
   misnamed in the firmware. Don't try to convert.
5. **`signalStrength` is always 0.** Use `rsrp` and map to 0-5 bars
   yourself (see `rsrpToSignal`).
6. **Responses are base64.** The step-1 response is one giant base64 string
   of JSON; subsequent responses are base64 AES ciphertext. Both look alike
   — don't confuse them when debugging.

## Where things live

- `client.go` — HTTP + crypto + response parsing. Single `Client` type.
  Methods: `Login`, `Logout`, `GetStatus`, `GetFlowStats`.
- `main.go` — flag parsing and three entry points: `runTUI`, `runWaybar`,
  `runRaw`. `fetchStatus` is the shared "login, read, logout" helper.
- `Makefile` — `build / install / install-waybar / run / raw / vet / tidy`.
- `PROTOCOL.md` — wire format reference (read this before crypto changes).
- `WAYBAR.md` — this machine's setup for the waybar tile.
- `contrib/mifi.sh` — waybar wrapper; silent when modem unreachable.

## Testing loop

```sh
make raw           # dumps the two decrypted responses verbatim
make build && ./tplink-m7010 --debug --raw --pass "$PASS"
```

There's no unit test suite — everything interesting is an integration against
a physical device and none of us have a reliable simulator. Add tests for the
helpers (`firstStr`, `firstInt`, `rsrpToSignal`, `networkTypeStr`,
`parseFloatStr`) if you're editing them; leave the crypto uncovered unless
you want to record/replay a real session.

## Commit style

Short subject, then a paragraph explaining the why. See the two existing
commits for the register.
