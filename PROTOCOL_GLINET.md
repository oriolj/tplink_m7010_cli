# GL.iNet Mudi (GL-E5800) JSON-RPC — reverse-engineered notes

This documents the JSON-RPC API exposed by the GL.iNet Mudi GL-E5800
(firmware 4.x, based on OpenWrt 22.03). The official 4.x API docs at
`dev.gl-inet.com/router-4.x-api/` have been offline since early 2024, so
what follows is a mix of `python-glinet` source reading, gl-sdk4 package
listings, and live probing against a real device on May 16 2026.

## Endpoint

Plain HTTP on the LAN side (default `192.168.8.1`):

| URL          | Purpose                                  |
| ------------ | ---------------------------------------- |
| `POST /rpc`  | All JSON-RPC traffic (auth + commands)   |

Content-Type: `application/json`. The body is a JSON-RPC 2.0 envelope.

> Note: the device also serves HTTPS on port 443 with a self-signed cert
> (`console.gl-inet.com`). We use plain HTTP from the LAN to stay symmetric
> with the M7010 path and skip cert handling — both flows are inside the
> same physical network.

## Authentication

The auth flow is **challenge/response**, not session-cookie. Logging in
yields a `sid` that has to be passed as the first element of `params` on
every subsequent call.

### Step 1: challenge

```json
{"jsonrpc":"2.0","id":1,"method":"challenge","params":{"username":"root"}}
```

Response:

```json
{"id":1,"jsonrpc":"2.0","result":{
  "alg":5,                  // crypt(3) algorithm id; 5 = SHA-256
  "hash-method":"sha256",   // outer hash
  "salt":"8cZ9zPFLRxER8bEK",
  "nonce":"vPiFi2f7tyLd9q0sRlUio655eylETvKn"
}}
```

`alg: 5` means the password is stored on the device as
`crypt(P, "$5$salt$")` — the standard SHA-256 variant of crypt(3). The
client must reproduce that hash, then take `sha256(username + ":" +
cryptHash + ":" + nonce)` as the login proof.

### Step 2: login

```json
{"jsonrpc":"2.0","id":2,"method":"login","params":{
  "username":"root",
  "hash":"<sha256 hex of username + : + cryptHash + : + nonce>"
}}
```

Response:

```json
{"id":2,"jsonrpc":"2.0","result":{"username":"root","sid":"TrOZShEm34..."}}
```

The same `sid` is also set as an `Admin-Token` cookie, but the cookie is
*not* required for subsequent RPC calls — every `call` must carry the
sid in `params[0]`.

### Step 3: authenticated calls

```json
{"jsonrpc":"2.0","id":3,"method":"call","params":[
  "<sid>", "<service>", "<method>", <args>
]}
```

`<args>` **must be a JSON array** — `[]` for nullary methods. A JSON
object `{}` is rejected with `Invalid params` even when the method takes
no parameters. This is the single biggest landmine in the protocol.

## SHA-256 crypt(3)

Go's standard library does not ship a crypt() equivalent. `crypt.go`
contains a pure-Go implementation matching the Drepper SHA-256 spec
(https://www.akkadia.org/drepper/SHA-crypt.txt) and produces byte-identical
output to `openssl passwd -5 -salt SALT KEY`. The algorithm is:

1. `altResult = SHA256(P || S || P)`
2. Mix that with P/S into a fresh SHA256 to produce `a`
3. Derive a P-permutation and an S-permutation from two more SHA256 chains
4. Run 5000 rounds of `SHA256(?P || ?S || ?P || cur)` with a bit-pattern
   schedule determined by the round number
5. Emit a 43-char custom base64 of the final digest

The implementation is ~120 lines and intentionally inlined rather than
pulled in as a dependency, matching the existing hand-rolled RSA in
`client.go`.

## Service / method catalog (probed against firmware 4.x as shipped May 2026)

This is what we've confirmed exists on a real device. Method names
without an asterisk return `result: []` or `result: {}` when there's
nothing to report — they're real but quiet.

| Service  | Method               | Notes                                                                        |
| -------- | -------------------- | ---------------------------------------------------------------------------- |
| —        | `challenge`          | Unauthenticated; takes `{"username":"root"}` directly in params.             |
| —        | `login`              | Unauthenticated; takes `{"username","hash"}` directly in params.             |
| —        | `logout`             | Takes `{"sid":"…"}` directly; best-effort, server expires sids on its own.   |
| `system` | `get_info`           | Static device info: model, MAC, SN, hardware feature flags.                  |
| `system` | `get_status`         | The home-screen snapshot: `network[]`, `wifi[]`, `service[]`, `client[]`, `system{mcu, cpu, memory, flash, uptime, …}`. **Battery percent lives at `system.mcu.charge_percent`.** |
| `system` | `reboot`             | Takes `[]`. Returns `[]` — appears scheduled; client may not see a response. |
| `system` | `poweroff`           | Takes `[]`. Same caveat as `reboot`.                                         |
| `modem`  | `get_modems_info`    | List of attached modems. Empty `[]` when no SIM/cellular is active.          |

### `system.get_status` shape (real device, abridged)

```json
{
  "network": [
    {"interface":"modem_cpu","online":true,"up":true},
    {"interface":"wan","online":false,"up":false},
    {"interface":"wwan","online":false,"up":false},
    ...
  ],
  "wifi": [
    {"name":"wifi2g","band":"2G","ssid":"…","passwd":"…","up":true,"encryption":"psk2","hidden":false,"guest":false,"channel":0,"mld":false}
  ],
  "service": [{"name":"wgserver","status":0}, ...],
  "client":  [{"cable_total":0,"usbeth_total":1,"wireless_total":0}],
  "system": {
    "lan_ip":"192.168.8.1",
    "lan_netmask":"255.255.255.0",
    "uptime":453.75,
    "tzoffset":"+0200",
    "timestamp":1778962354,
    "cpu":{"temperature":43},
    "mcu":{
      "temperature":35.5,
      "charging_status":1,   // 0 = on battery, >0 = charger plugged in
      "fastcharge":false,
      "charge_cnt":0,
      "charge_percent":100,
      "abnormal":false,
      "abnormal_type":0
    },
    "memory_total":1675968512,
    "memory_free":1055318016,
    "memory_buff_cache":165433344,
    "flash_total":7818182656,
    "flash_free":2910171136,
    "flash_app":6119424,
    "load_average":[1.36,1.39,0.79],
    "load_average":[1.36,1.39,0.79],
    "mode":0,
    "guest_ip":"192.168.9.1",
    "guest_netmask":"255.255.255.0",
    "time_sync_status":true,
    "netnat_enabled":true,
    "ipv6_enabled":false,
    "sqm_enabled":false,
    "qos_enabled":false,
    "prio_enabled":false,
    "flow_statistics_enabled":false,
    "content_protection_enabled":false,
    "ddns_enabled":false
  }
}
```

### `modem.get_modems_info` shape (when a SIM is active)

We do not have a confirmed shape — the test device had no SIM during
reverse-engineering and the response was `[]`. `parseMudiModem` in
`mudi.go` tries several likely field names per metric (`carrier` /
`operator_name` / `operator`, `network_type` / `act`, `band` / `lte_band`,
etc.); when a new firmware breaks this, add candidates rather than
replacing them. The field-name guesses are based on patterns from the
`gl-sdk4-modem` and `gl-modem-at` packages.

## Gotchas

- **Args shape**: `[]` not `{}`. Re-read the spec section if you're
  tempted to use a Go map.
- **Battery is buried inside `system.mcu`**, not a dedicated `battery`
  service.
- **The `signalStrength` family of fields may be reported as `0`** when
  the modem is connected but the SDK hasn't refreshed yet. We fall back
  to deriving 0-5 bars from `rsrp`, the same way the M7010 path does.
- **The 4.x docs are offline.** When you need to confirm a method,
  probe the device directly — `python-glinet` carries the most accurate
  current understanding of the request shape, and the gl-sdk4 ipk list
  on `github.com/gl-inet/glinet` enumerates which services should
  exist (though not their method names).
- **The Mudi accepts `system.poweroff` / `system.reboot` with `[]` as
  args**, but the response is empty (`result: []`) and the methods may
  or may not actually fire on every firmware revision. Test by hand on
  a device you can physically power back on.

## References

- `tomtana/python-glinet` — reference Python client. Auth flow,
  `params` injection scheme, and method dispatch shape came from here.
- `gl-inet/glinet` — official ipk repository. `images.json` enumerates
  every `gl-sdk4-*` package shipped per device; useful for sniffing out
  which services should exist (`gl-sdk4-system`, `gl-sdk4-modem`,
  `gl-sdk4-ui-overview`, …).
- `gl-inet/gl-modem-at` — AT-command shim used by the modem service;
  worth grepping when looking for field names like `rsrp`/`rsrq`/`band`.
- `https://www.akkadia.org/drepper/SHA-crypt.txt` — definitive
  SHA-256 crypt(3) spec.
