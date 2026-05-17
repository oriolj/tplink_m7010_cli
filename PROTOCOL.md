# TP-Link M7010 web API — reverse-engineered notes

This documents what we figured out while making `tplink-m7010` work against a
real device (firmware `3.0.3 Build 250814 Rel.1021n`, HW `M7010(EU) v3.2`).
Much of this is undocumented; the few resources on the open internet target
the M7350 or M7200 and partially differ from the M7010.

> The other device this binary supports — the GL.iNet Mudi GL-E5800 — has
> its own wire format and notes at `PROTOCOL_GLINET.md`. Everything below
> is M7010-specific.

## Endpoints

All over plain HTTP on the LAN-side address (default `192.168.0.1`):

| URL                      | Purpose                                          |
| ------------------------ | ------------------------------------------------ |
| `POST /cgi-bin/auth_cgi` | Fetch nonce+RSA params, then submit login digest |
| `POST /cgi-bin/web_cgi`  | All post-login calls (status, flowstat, …)       |

Content-Type: `application/json`. Body is a JSON object — but the contents
depend on the step, see below.

## Transport envelope

### Step 1: Unencrypted "hello"

Request (body is JSON with a single base64-encoded inner JSON under `"data"`):

```json
{"data": "<base64 of {\"module\":\"authenticator\",\"action\":0}>"}
```

Response is base64-encoded JSON (the whole body is one big base64 string):

```json
{
  "authedIP":   "0.0.0.0",
  "nonce":      "6vM96Wg2m-G72XLG",
  "rsaPubKey":  "010001",                                  // hex, usually 0x10001
  "rsaMod":     "F0946D8F0560...56A57673",                 // hex, 512-bit modulus
  "seqNum":     1051688748,                                // int
  "result":     0
}
```

`result: 1` on the very first request is normal — the modem reports 1 when it
has no active session, but the other fields are still valid and usable.

### Step 2: Login (encrypted)

1. Generate a random 16-byte ASCII-digit AES-128-CBC key + IV (use digit
   characters to mirror the reference PHP lib; hex or base64 may or may not
   work — we didn't test).
2. AES-128-CBC encrypt this JSON with PKCS#7 padding, then base64 the
   ciphertext:

   ```json
   {"module":"authenticator","action":1,"digest":"<md5(password+':'+nonce)>"}
   ```

   Note the `:` separator between password and nonce.

3. Build a sign string in **insertion order** (alphabetical ordering breaks
   the server):

   ```
   key=<aes_key>&iv=<aes_iv>&h=<md5('admin'+password)>&s=<seqNum + len(base64(aes_ciphertext))>
   ```

4. RSA-PKCS1v15 encrypt the sign string with the public key from step 1. The
   sign string is longer than the RSA modulus allows (512-bit key → 53-byte
   max plaintext per block), so **chunk** it into 53-byte blocks, encrypt each
   separately, and concatenate the ciphertexts. Output as lowercase hex.

5. POST to `/cgi-bin/auth_cgi`:

   ```json
   {"data": "<base64 aes ciphertext>", "sign": "<hex rsa ciphertext>"}
   ```

Response body is just base64-encoded AES ciphertext (no JSON wrapper). Decrypt
with the same AES key/IV and parse as JSON:

```json
{"token": "SSnASCGDZclim6kp", "authedIP": "192.168.0.160", "factoryDefault": "1", "result": 0}
```

### Step 3: Subsequent calls

Same envelope, sent to `/cgi-bin/web_cgi`. Inner JSON includes the token. The
sign string drops `key` and `iv` (server already has them) but keeps `h` and
`s`:

```
h=<md5('admin'+password)>&s=<seqNum + len(base64(aes_ciphertext))>
```

`seqNum` is the one from step 1; do **not** increment it.

## Gotchas

- **Go's standard `crypto/rsa` refuses 512-bit keys** (security policy since
  Go 1.24). Implement PKCS1v15 manually with `math/big`. See
  `rsaEncryptBlock` in `client.go`.
- **Field ordering in the sign string matters.** `url.Values.Encode()` sorts
  alphabetically in Go — PHP's `http_build_query` preserves insertion order.
  We had garbage AES responses until we matched the PHP order (`key,iv,h,s`).
- **`seqNum` may appear as a JSON number**, not a string. Handle both.
- **The M7010 firmware always returns `signalStrength: 0`**. The real signal
  quality lives in `rsrp` (dBm). We map RSRP → 0-5 bars ourselves:
  `≥-80 = 5, ≥-90 = 4, ≥-100 = 3, ≥-110 = 2, ≥-120 = 1, else 0`.
- **`battery.voltage` is actually the battery percent**, not a voltage. The
  field is mis-named in the firmware.
- **`totalStatistics` / `dailyStatistics` are decimal strings in bytes**
  (e.g. `"14473800628.000000"`), not integers or MB.

## Module/action catalog

Reverse-engineered from `tp_m7350_enums.h` in `vpaeder/tplink_m7350_cpp`.
Numbers below are the `action` values inside each module. Not exhaustive — we
only use `status`/`flowstat`/`authenticator`/`reboot`, the rest are untested
on the M7010 but likely share the convention.

| Module        | String              | Key actions                                                                   |
| ------------- | ------------------- | ----------------------------------------------------------------------------- |
| authenticator | `authenticator`     | 0=Load (nonce), 1=Login, 2=GetAttempts, 3=Logout, 4=Update                    |
| status        | `status`            | 0=Get                                                                         |
| flowstat      | `flowstat`          | 0=GetConfiguration, 1=SetConfiguration                                        |
| wan           | `wan`               | 0=Get, 1=Set, 8=SetNetworkSelectionMode, 9=QueryAvailableNetworks, …          |
| wlan          | `wlan`              | 0=Get, 1=Set                                                                  |
| message       | `message`           | 0=Get, 2=ReadMessage, 3=SendMessage, 5=DeleteMessage, 6=MarkAsRead            |
| reboot        | `reboot`            | 0=Reboot, 1=Shutdown                                                          |
| connectedDevices | `connectedDevices` | 0=Get                                                                         |

## Example `status` response (real device, abridged)

```json
{
  "deviceInfo": {"model":"M7010", "firmwareVer":"3.0.3 Build 250814 Rel.1021n", ...},
  "battery":    {"connected":true, "charging":false, "voltage":93},
  "wan": {
    "connectStatus":4, "ipv4":"5.205.144.6",
    "networkType":3,      // 0=NoSvc, 1=2G, 2=3G, 3=4G, 4=4G+
    "signalStrength":0,   // always 0 — use rsrp instead
    "totalStatistics":"14473800628.000000",
    "dailyStatistics":"42744727.000000",
    "txSpeed":"2964", "rxSpeed":"2202",
    "operatorName":"Movistar", "band":20,
    "rsrp":-126, "rsrq":-20, "rssi":22, "snr":-80
  },
  "connectedDevices": {"number":2},
  "result": 0
}
```

## Example `flowstat` response

```json
{
  "settings": {
    "enableDataLimit": false,
    "limitation": "0.000000",     // monthly limit in bytes, 0 = unlimited
    "paymentDay": 1,
    "adjustStatistics": "14473800628.000000",
    "warningPercent": 90
  },
  "result": 0
}
```

## References

- `mt-ks/tp-link-m7200-api` — PHP library for M7200/M7000. The AES+RSA
  envelope came from here. The M7010 uses the same envelope.
- `vpaeder/tplink_m7350_cpp` — C++ driver for M7350. Module/action enums came
  from here. The M7350 uses a different, simpler transport (plain JSON with a
  token) that **does not work** on the M7010.
- Pen Test Partners post on CVE-2019-12103 — useful background on the
  `qcmap_web_cgi` architecture inside the firmware.
