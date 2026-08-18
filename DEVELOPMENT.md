# Development log

The path from "let's scrape the M7010" to "it works" was not linear. Keeping
the false starts here so the next person (or session) can skip them.

## Attempt 1 — M7350-style plain JSON + nonce MD5

Based on `vpaeder/tplink_m7350_cpp` and the Pen Test Partners write-up of
CVE-2019-12103. Assumed the M7010 shared the M7350's transport:

```
POST /cgi-bin/auth_cgi  {"module":"authenticator","action":0}
→ {"nonce":"…"}
POST /cgi-bin/auth_cgi  {"module":"authenticator","action":1,"digest":md5(pass+nonce)}
→ {"token":"…"}
```

What actually happened:

- The response body was base64 (`ewoJInJlc3VsdCI6CTEKfQ==`). Added a fallback
  decode.
- The step-1 response was `{"result":1}` with **no nonce**. Interpreted this
  as "this firmware doesn't use a nonce" and tried `md5(password)` alone as
  the digest. Still got `result:1`.
- Verdict: the M7010 isn't this protocol.

## Attempt 2 — M7200-style AES + RSA envelope

Found `mt-ks/tp-link-m7200-api` (PHP). It uses a completely different
envelope:

- Step 1 body is `{"data": base64(json)}`, not raw JSON. This is why step 1
  returned `result:1` before — the M7010 was treating our JSON as malformed
  and defaulting. Rewrote step 1 to match; immediately got `nonce`,
  `rsaPubKey`, `rsaMod`, `seqNum` back.
- Every subsequent request is `{data: aes(…), sign: rsa(…)}`. The login
  digest is `md5(password + ':' + nonce)` (colon separator — missed this
  initially).

Got into Go-specific pain:

1. `crypto/rsa` rejects the 512-bit key with
   `"512-bit keys are insecure"`. Had to re-implement PKCS1v15 with
   `math/big`.
2. Then the plaintext (sign string) was too long for one block. phpseclib
   auto-chunks; Go stdlib wouldn't do this even if it accepted the key.
   Added a chunking loop.
3. AES decryption of the response produced garbage. After staring at the
   PHP source: the sign string built with `url.Values.Encode()` gets sorted
   alphabetically (`h&iv&key&s`) but PHP's `http_build_query` preserves
   insertion order (`key&iv&h&s`). The M7010 firmware apparently does not
   parse this as a query string but as a positional blob. Switched to
   manual `fmt.Sprintf`. Login succeeded.

## Attempt 3 — making sense of the status response

First real status response surprised us:

- `signalStrength: 0` even with four bars on the device screen. Found `rsrp`
  (= -92 dBm) adjacent; derived bars from that.
- `battery: {voltage: 93}` — "voltage" is the percentage. Confirmed by
  watching it drop while unplugged.
- `wan.totalStatistics: "14473800628.000000"` — bytes as a decimal string,
  not MB. Same shape for `dailyStatistics`. Added a string-tolerant float
  parser (today's `jsonFloatStr`).
- `wan.networkType: 3` — numeric enum, not "4G". Mapped manually.

## Attempt 4 — waybar tile that doesn't haunt the bar when disconnected

`custom/mifi` with `interval: 30` left a stale "4G 89% 14GB" tile on the
waybar for up to 30 seconds after leaving the M7010's Wi-Fi, and an error
tile while trying to connect. Fix was a wrapper script:

- TCP-connect to `192.168.0.1:80` with a 500ms timeout.
- If it fails, exit 0 with empty stdout → waybar hides the module.
- Otherwise exec the binary and pass its JSON through.

This ended up being the right layer to put it in: the Go binary stays dumb
about reachability, the bash wrapper handles UX.

## Things we did not try

- **HTTPS.** The M7010 also speaks HTTPS with a self-signed cert. Not needed
  for a LAN-side tool, and adds cert pinning/skipping ceremony.
- **Persisting the session token across invocations.** Would save ~200ms per
  waybar tick. Not worth the staleness handling.
- **Monitoring SMS / reboot / WLAN.** All supported by the API (see
  `PROTOCOL.md` module table), just not needed for the waybar use case.
- **A recorded-response test harness.** Would make the crypto code
  regression-testable without a device. Deferred at the time — done in
  Attempt 9 as live fake-device servers instead of recordings.

## Attempt 5 — adding the GL.iNet Mudi (GL-E5800)

The Mudi 7 runs OpenWrt 22.03 with GL.iNet's gl-sdk4 packages on top, and
exposes a JSON-RPC API at `POST /rpc`. Auth is challenge/response, not
session-cookie. The two interesting landmines:

1. **`crypt(3)` with the `$5$` SHA-256 algorithm.** Go's stdlib has no
   crypt. Pulled in a 120-line implementation in `crypt.go`, cross-checked
   byte-for-byte against `openssl passwd -5 -salt SALT KEY`. Could've
   pulled in `GehirnInc/crypt` but the existing repo style is "roll your
   own crypto when stdlib refuses" (see the M7010's hand-rolled RSA).
2. **`params[3]` must be a JSON array.** Authenticated calls have the
   shape `{"method":"call","params":[sid, service, method, args]}`, and
   `args` is rejected as `Invalid params` when it's `{}`. Even nullary
   methods need `[]`. This wasted a half hour of "the service must not
   exist" probing before I noticed that `[]` made `system.get_status`
   answer with a giant payload.

The 4.x API docs at `dev.gl-inet.com/router-4.x-api/` have been offline
since early 2024, so the method catalog in `PROTOCOL_GLINET.md` is built
from `python-glinet`, the gl-sdk4 ipk list on github, and live probing.

## Attempt 6 — making autodetect not pick the wrong device

A naive parallel TCP probe to both default addresses claimed the M7010
was reachable when only the Mudi was on the LAN — `192.168.0.1:80` was
accepted (likely by upstream NAT or a stale ARP entry) but the
connection then timed out at the application layer. Switched to
"default gateway first, TCP probe as fallback" in `detectDevice`. The
gateway signal is essentially free (one `/proc/net/route` read) and
unambiguous because it's literally where our packets are going.

## Attempt 7 — running the same binary against two devices

The `Device` interface in `device.go` is intentionally tiny (Connect,
Fetch, Shutdown, Reboot, Close, Name). Sharing the `Status`
struct between protocols meant the TUI / waybar / noctalia output
formatters didn't need a single change — every protocol-specific
field-name guess lives inside the per-device client.

Password and address resolution moved into `device.go` so the
per-device env vars (`M7010_PASS` / `MUDI_PASS`) and password files
(`~/.config/tplink-m7010/password` / `~/.config/gl-e5800/password`) are
discovered from one place. `TPLINK_PASS` / `TPLINK_ADDR` / `GLINET_PASS`
are accepted as alternate spellings — none is preferred, neither file
path is preferred. Both devices are first-class.

## Attempt 8 — review hardening: protocol probes, RFC-clean WS, unit tests

A full code review (July 2026) turned up a batch of small-but-real
issues, fixed together:

- **Autodetect probed TCP, not the protocol.** 192.168.0.1 is the most
  common home-router gateway IP in existence, so the gateway-first match
  from Attempt 6 could still claim an M7010 on any such LAN and leave
  waybar showing a login-error tile instead of collapsing. Each registry
  entry now carries a `Probe` (unauthenticated M7010 hello / Mudi
  challenge) and both detect signals must pass it. The probes also honour
  the `*_ADDR` env overrides via `resolveAddr`.
- **The WS pong reply was unmasked and didn't echo the ping payload** —
  both RFC 6455 violations a strict server answers by dropping the
  connection. All client frames now go through one masked `sendFrame`.
  Frame payload lengths are capped at 1MB so a corrupt length field
  can't trigger a giant allocation.
- **`rebootAction` swallowed every error**, including "couldn't even
  build the request". It now returns pre-send failures and only swallows
  the expected mid-request connection drop.
- **A malformed step-1 response could hang the M7010 client**: an
  unparseable RSA modulus left `chunkSize` negative and the chunking
  loop ran backwards forever. Both `SetString` results are now checked.
- **The Makefile's `SRC` list had drifted** (missing `ws.go`), so edits
  to it didn't trigger rebuilds. Now `$(wildcard *.go)`.
- **Unit tests exist now** (`make test`): openssl-verified `sha256Crypt`
  vectors, the pure helpers, the WS frame parser against byte fixtures,
  and the probes against `httptest` servers.

## Attempt 9 — fake-device envelope tests

The crypto envelopes were the last untested code, and "record/replay"
(deferred in the early list above) turned out to be the wrong shape —
the M7010's envelope is nondeterministic (random AES key/iv per login),
so recordings can't be replayed byte-for-byte. Implementing the *server*
side of each protocol in `httptest` works better:

- `m7010_envelope_test.go` hands out a fixed 512-bit RSA key, decrypts
  the client's chunked PKCS1v15 sign string with `math/big`, and
  position-parses it exactly like the firmware — the `key,iv,h,s` order
  from Attempt 2 is now enforced by a test instead of by memory. It also
  validates PKCS#7 padding byte-for-byte (stricter than the client's own
  unpad, so a symmetric client bug can't cancel out) and serves the
  canned status/flowstat bodies from PROTOCOL.md.
- `mudi_envelope_test.go` verifies the challenge hash with the same
  `sha256Crypt` the device would use, rejects object-shaped `args` with
  `-32602` (the Attempt 5 landmine), and hijacks the HTTP connection to
  run a real WS handshake + event push through `ws.go`.

One latent bug surfaced while wiring this up: `dialWS` hardcoded port
80, so `--addr host:port` worked for `/rpc` but silently broke the WS
path (and httptest servers). Fixed to honour an explicit port.

## Attempt 10 — remaining battery time from a quantised percent

**Want**: `hh:mm` remaining, in the CLI and in the bar widget's popup.

**Problem**: neither device reports one. The M7010 gives
`battery.voltage` (a percent despite the name) plus a charging flag; the
Mudi gives `system.mcu.charge_percent` plus `charging_status`. No
current, no voltage, no time. Both are integers, 1% steps. And the
binary is one-shot, so there is no history to differentiate.

**What didn't work, on paper**:

- *Rate between two arbitrary samples.* A reading of 88% is an interval,
  not a point. Two samples 1% apart could be 1.01% or 2.99% of real
  drop — a ±100% error on the rate, which is worse than useless when it
  is rendered as a confident `4h12m`.
- *In-process history only (TUI).* At ~5 min per percent, a TUI session
  would have to stay open ~20 min before showing anything, and the
  noctalia poll — a fresh process every 30s — would never show anything
  at all. That is the surface that actually wanted the feature.

**What stuck**:

1. **A percent history in `$XDG_STATE_HOME/tplink-m7010/battery.json`.**
   Stored as runs (one entry per distinct percent, with first-seen and
   last-seen timestamps) rather than one entry per poll, so a full
   discharge is ~100 entries instead of ~1200.
2. **Edge anchoring.** The rate is measured between the instants the
   percent *changed*, where the drop is exactly N percent and the only
   residual error is the poll interval. `runs[0]` is skipped: we joined
   that percent partway through.
3. **A warm-up gate of 3 percent steps** (~20 min), because one step is
   a sample size of one.
4. **A datasheet bootstrap** so the line is useful immediately: 8 h for
   the M7010 (2000 mAh, "8 h of 4G sharing"), 13.5 h for the Mudi 7
   (5380 mAh). Labelled `(typical)` in the UI — a guess that looks like
   a measurement is the failure mode worth engineering against.
5. **Continuity resets** on charger flips, >20 min polling gaps, and
   double-digit jumps. The gap rule matters most: a router that was
   switched off is indistinguishable from one discharging very slowly,
   and that reading inflates the estimate.

**Deliberately not done**: a charging bootstrap. Neither vendor
publishes a charge time, so time-to-full appears only once measured.

**Caught in review**: appending the estimate to the TUI's battery row
wrapped it onto a second line — the box is 46 columns. It gets its own
`Remaining` / `To full` row, and the test asserts the line doesn't wrap.
