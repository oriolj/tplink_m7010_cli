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

## Error codes (live-probed)

The dispatcher overloads JSON-RPC error codes in a way that took a
session to figure out, so write it down:

| Code     | Meaning in practice                                                            |
| -------- | ------------------------------------------------------------------------------ |
| `-32601` | Method not found. Either the service does not exist, or it exists but doesn't have this method. There's no way to distinguish from the wire — a totally bogus service name and a real service + wrong method both return this. |
| `-32602` | Invalid params. The service **does** exist; the (method, args) pair just doesn't validate. Useful as a discovery signal: if you get `-32602` for every method/arg combo on a service, you've found a real service whose API you haven't decoded yet. |
| `-32000` | Access denied. Almost always means the sid expired — re-login. |

## Service / method catalog (probed against firmware 4.8.3 on the Mudi 7 in May 2026)

Confirmed-working calls. Method names without an asterisk return
`result: []` or `result: {}` when there's nothing to report — they're real
but quiet.

| Service    | Method                 | Args                | Notes                                                                                                       |
| ---------- | ---------------------- | ------------------- | ----------------------------------------------------------------------------------------------------------- |
| —          | `challenge`            | `{"username":"root"}` | Unauthenticated; params is a plain object, not the `[sid, …]` quadruple.                                  |
| —          | `login`                | `{"username","hash"}` | Unauthenticated; same shape.                                                                              |
| —          | `logout`               | `{"sid":"…"}`        | Best-effort; server expires sids on its own.                                                               |
| `system`   | `get_info`             | `[]` or `{}`         | Static device info: model, MAC, SN, hardware/software feature flags, firmware version, openwrt version.    |
| `system`   | `get_status`           | `[]` or `{}`         | Home-screen snapshot: `network[]`, `wifi[]`, `service[]`, `client[]`, `system{mcu, cpu, memory, flash, uptime, …}`. **Battery is at `system.mcu.charge_percent`.** |
| `system`   | `reboot`               | `[]`                 | Returns `[]`. May need confirmation params we haven't found — test before relying on it.                   |
| `system`   | `poweroff`             | `[]`                 | Same caveat as `reboot`.                                                                                   |
| `modem`    | `get_modems_info`      | `[]` or `{}`         | List of **USB** modems. Returns `[]` on Mudi 7 because its modem is CPU-integrated (see "Mudi 7" below).  |
| `clients`  | `get_status`           | `{}` or `[]`         | Per-link client counts: `{cable_total, usbeth_total, wireless_total}`.                                     |
| `ui`       | `check_initialized`    | `{}`                 | Returns `{initialized, surpport_screen_init, environment_support, band_mutex, vpn_wizard_done, inited_internet, mac}`. Useful for sanity-checking auth. |
| `ui`       | `get_menu_list`        | `{}`                 | Returns the menu the web UI uses. Hint: the `view` keys often correspond to service names elsewhere.       |
| `qos`      | `get_config`           | `{}`                 | Returns `{enable, mode}`. Probably more keys when QoS is enabled.                                          |
| `network`  | `get_arp_list`         | (none)               | `{entries:[{mac, device, ip}]}`.                                                                           |
| `network`  | `check_wan_cable`      | `{}`                 | `{cable_enabled, cable_inserted, macclone_enabled}`.                                                       |
| `network`  | `get_dhcp_leases`      | (none)               | Documented in api_description; not tested live.                                                            |
| `kmwan`    | `get_config`           | `{}`                 | Multi-WAN failover config: `{interfaces:[{interface, metric, weight, track_proto, track_method, track_ipv4[], track_ipv6[], enable_check, enable_ssl, track_mode}], mode}`. On Mudi 7 the chain is wan → wwan → tethering → modem_cpu (metrics 10 / 20 / 30 / 40). |

### Services that exist but we haven't decoded

These return `-32602 Invalid params` for every method/arg combo we tried,
which means they exist but the API surface isn't obvious. Could be WiFi
band controllers, internal state machines, or just methods that take very
specific params:

- `5g`, `5g_band` — almost certainly the 5 GHz **WiFi** radio control,
  not cellular 5G. `ui.check_initialized` returns `band_mutex: "5G+6G"`
  which is a WiFi concept.
- `4g`, `4g_modem`, `5g_modem` — unclear; possibly more WiFi band glue.
- `acl` — listed in upstream api_description but doesn't respond on Mudi 7.

### Methods present in upstream api_description but absent on Mudi 7

The python-glinet `api_description.json` documents these for older
GL.iNet firmware — they return `-32601` on the Mudi 7 (4.8.3):

- `modem.get_status` / `modem.get_info` (object args per upstream).
- `modem.get_cells_info`, `modem.set_connect`, `modem.send_at_command`,
  `modem.get_sms_list`, etc.
- `modem.get_traffic_config`, `modem.reset_traffic_count`.

These are all USB-modem-centric (every example uses `{"bus":"1-1"}` to
identify the modem). The Mudi 7's `build_in_modem` is `"cpu"` — the
modem isn't on the USB bus, so this whole branch doesn't apply.

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

On devices with a USB modem this returns `[{bus, vendor, name, type,
sms_support, imei, protocols, simcard{iccid, phone_number, mcc, mnc, …},
signal{strength, rssi, rsrp, rsrq, sinr, mode}, network{ip, tx, rx, …}}]`
per upstream api_description.

On the **Mudi 7 (E5800)** with an active 5G SIM it still returns `[]` —
see the next section. `parseMudiModem` in `mudi.go` tries several likely
field names per metric (`carrier` / `operator_name` / `operator`,
`network_type` / `act`, `band` / `lte_band`, etc.); when this branch
finally fires on a USB-modem device, add candidates rather than replacing
them when a new firmware shows up.

## Mudi 7 (GL-E5800) specifics

The Mudi 7 has a Qualcomm X72 5G modem **integrated into the SoC**
(Qualcomm IPQ5018 / similar), not on the USB bus. `system.get_info`
makes this explicit:

```json
"hardware_feature": {
  "build_in_modem": "cpu",
  "modem_reset": 2,
  "screen": true,
  "mcu": true,
  ...
}
"software_feature": {
  "cellular_ref": "1.0",
  ...
}
"board_info": {
  "architecture": "ARMv8 Processor rev 0",
  "hostname": "GL-E5800",
  "kernel_version": "5.15.170-perf",
  "openwrt_version": "OpenWrt 23.05.4 r24012-d8dd03c46f",
  "model": "GL.iNet GL-E5800"
}
"firmware_version": "4.8.3"
```

`build_in_modem: "cpu"` is the critical signal. The standard
`modem.*` API (`get_status`, `get_info`, `get_cells_info`, …) is
USB-modem-only and is **not registered** on Mudi 7 — calls return
`-32601`. `modem.get_modems_info` exists but enumerates USB modems and
returns `[]` even when the CPU modem is online.

### Where the cellular signal actually lives: WebSocket at /ws

The web UI does **not** poll `/rpc` for cellular state. Instead the dashboard
opens a WebSocket and the server pushes a stream of named JSON events. The
home-page tile reads these directly. This is why no `/rpc` probe finds the
data — there is no RPC method for it on the Mudi 7.

Two channels we've confirmed do **not** carry it:

- `system.get_status.network[]` only lists interface up/online — the
  `modem_cpu` entry goes `online: true` when the link is up but carries
  no signal data. Use it to decide whether to show the cellular
  section at all.
- `modem.get_modems_info` returns `[]` regardless of cellular state
  because it enumerates USB modems and the Mudi 7's modem is on the CPU.

#### Handshake

```
GET /ws?sid=<sid> HTTP/1.1
Host: 192.168.8.1
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Key: <16 random bytes base64>
Sec-WebSocket-Version: 13
Origin: http://192.168.8.1
Cookie: Admin-Token=<sid>
```

The sid goes both in the query string **and** the cookie. Server returns
the standard `HTTP/1.1 101 Switching Protocols` with `Sec-WebSocket-Accept`.
`permessage-deflate` is not negotiated even when the client offers it —
all frames are uncompressed plaintext UTF-8 JSON.

#### Event envelope

Every event is a text frame containing one JSON object:

```json
{"name": "<event>", "data": {...}}
```

No `id`, no `jsonrpc`, no error channel. The `name` is hierarchical
(`cellular.sims_status`, `cellular.networks_info`, …) and matches the
ubus path of the underlying service. Most events fire on a ~10s cycle.

#### Known events (Mudi 7, firmware 4.8.3)

These are the cellular events observed during reverse-engineering. Field
types are inferred from one device with a Movistar SIM in slot 1; treat
field presence as best-effort.

**`cellular.sims_info`** — SIM identity, fires per slot:

```json
{"sims": [
  {
    "slot": "1",
    "bus": "cpu",
    "iccid": "8934075700136600514F",
    "imsi": "214070613106368",
    "apn_list": ["telefonica.es"],
    "is_certification": false,
    "is_special_operator": false
  },
  {"slot": "2", "bus": "cpu", "is_certification": false, "is_special_operator": false}
]}
```

**`cellular.sims_status`** — SIM operational state. `strength` is 0-4
bars (we add 1 to get the 0-5 scale we use elsewhere). `technology` is a
numeric code (51 observed on 5G NR); we infer the radio mode from
`cellular.networks_info.cell_info.mode` instead.

```json
{"sims": [
  {
    "slot": "1",
    "bus": "cpu",
    "iccid": "8934075700136600514F",
    "carrier": "Movistar",
    "strength": 4,
    "technology": 51,
    "type": 0,
    "status": 6
  },
  {"slot": "2", "bus": "cpu", "type": 0, "status": 0, "strength": 0}
]}
```

**`cellular.networks_info`** — per-SIM radio + IP detail. `cell_info`
holds RSRP/RSRQ/SINR as **decimal strings** (e.g. `"-79"`), and `band` /
`*_level` fields as numbers. `mode` is the radio mode we surface as
NetworkType (`"NR5G-NSA"`, `"LTE"`, etc.).

```json
{"networks": [
  {
    "slot": "1",
    "bus": "cpu",
    "network_interface": "modem_cpu",
    "network_mode": "AUTO",
    "ip_type": 0,
    "mtu_sync": 0,
    "cell_info": {
      "type": "servingcell",
      "mode": "NR5G-NSA",
      "band": 78,
      "dl_bandwidth": "100MHz",
      "tx_channel": "",
      "rsrp": "-79", "rsrp_level": 4,
      "rsrq": "-10", "rsrq_level": 4,
      "sinr": "12",  "sinr_level": 4
    },
    "ipv4": {
      "ip": "10.112.6.17",
      "netmask": "255.255.255.252",
      "gateway": "10.112.6.18",
      "dns": ["80.58.61.250", "80.58.61.254"]
    }
  },
  {"slot": "2", "bus": "cpu", "network_interface": "modem_cpu",
   "network_mode": "AUTO", "ip_type": 0, "mtu_sync": 0}
]}
```

**`cellular.networks_status`** — traffic counter + dial state per SIM.
`traffic_total` is a **decimal string in bytes** (matches the M7010's
`totalStatistics` shape). `dial_status: 0` means the SIM is the active
WAN.

```json
{"networks": [
  {
    "slot": "1",
    "bus": "cpu",
    "iccid": "8934075700136600514F",
    "protocol": "rmnet",
    "traffic_total": "234770803",
    "status": 0,
    "dial_status": 0,
    "callcode": 0,
    "calltype": 0,
    "call_reason": "Success"
  },
  {"slot": "2", "bus": "cpu", "protocol": "rmnet", "traffic_total": "0",
   "status": 2, "dial_status": 1, "callcode": 0, "calltype": 0}
]}
```

#### Discovery story (so we don't have to redo this)

The web UI doesn't poll `/rpc` for cellular state. Every plausible RPC
service was tried (`modem.get_status`, `cellular.*`, `5g.*`, `4g.*`,
`kmwan.*`, …) — none return signal data. The data only surfaces over
`/ws`. The hint that finally cracked it was the user pasting a verbatim
`curl ws://192.168.8.1/ws?sid=…` request from their browser; the
browser uses it for the dashboard's live tile and there's no HTTP-side
equivalent.

#### Subscribe protocol: unknown

A bare WebSocket open is **not** enough to start receiving events. With
the browser closed (so no other session can be hogging the stream), my
plain client gets the `101 Switching Protocols` response and then
silence for 30+ seconds. The server isn't pushing autonomously — the
browser must send something on the WS to subscribe.

What I tried that **didn't** work:

- `{"id":1,"jsonrpc":"2.0","method":"subscribe","params":[…]}` with a
  bunch of param shapes (service name string, `{event}` object, …)
- `{"id":1,"method":"call","params":["cellular.status","info",{}]}`
- `{"sub":"cellular"}`, `{"event":"cellular.status"}`
- Concurrent RPC calls to `system.get_status` (in case events fire only
  while the UI is asking for system state)

What would unblock this:

- The first outgoing (green) WS frame the browser sends right after the
  101 response. That's almost certainly the subscribe shape the server
  expects. (DevTools → Network → click `ws` → Messages tab → look for
  the very first ▲ green frame.)
- Or: SSH + `ubus subscribe cellular.status` (and the rest of the
  `cellular.*` services) to confirm whether events are firing
  internally at all, or only on demand.

Until we have the subscribe message, `MudiClient.collectCellular` in
`mudi.go` connects to the WS but will time out without payload — leaving
the cellular fields empty while battery / network-online state continue
to populate from `system.get_status`. The widget falls back gracefully.

#### Underlying ubus services (visible over SSH)

`ubus list | grep cellular` on the Mudi 7 shows:

```
cellular.cm
cellular.collect
cellular.failover
cellular.modem
cellular.network
cellular.sim
cellular.status
modem.CPU.AT
network.interface.modem_cpu
```

These are real ubus services and `ubus call cellular.modem info` returns
the Quectel hardware info directly (model `RG650V-EU`, IMEIs per slot,
supported LTE/NR-NSA/NR-SA bands, capability flags). They are **not**
exposed under `/rpc` — the JSON-RPC dispatcher in front of `/rpc` only
hands out a curated subset (`system`, `clients`, `qos`, `network`,
`ui`, …; see catalog above). The WS at `/ws` is the only LAN-side
interface that surfaces `cellular.*` state to non-shell clients.

`/usr/share/rpcd/acl.d/` on a stock Mudi 7 contains only
`unauthenticated.json` — the standard OpenWrt rpcd acl.d/ pattern is
not how GL.iNet gates `/rpc` access. The dispatcher is its own C binary
with internal allowlists.

#### ubus method signatures (`ubus -v list cellular.*`)

These are the live methods on the cellular tree. `bus: "cpu"` everywhere
on the Mudi 7 because the modem is CPU-integrated; `slot` is 1 or 2.
The WS events on `/ws` are a superset of `*.status` + `*.info` calls
across these services.

```text
cellular.modem
    info                     {bus: String}                 -- Quectel HW info, bands, IMEIs
    status                   {bus: String}                 -- modem-level state
    get_modem_info           {bus: String}
    update_modem_info        {bus: String, action: String}
    clean_switch_count       {bus: String}
    get_all_config           {bus: String}
    get_feature_config       {bus: String}
    set_feature_config       {bus: String}
    get_slot_priority_config {bus: String}
    set_slot_priority_config {bus: String, slot_priority: Array}
    set_airplane_mode        {enable: Boolean}             -- the "kill switch"

cellular.sim
    init                     {bus: String}
    info                     {bus: String}                 -- iccid, imsi, apn_list per slot
    status                   {bus: String}                 -- carrier, strength (0-4), technology
    get_config               {iccid: String}
    set_config               {iccid: String, data: Table}
    set_pincode              {bus: String, iccid: String, pin_code: String}

cellular.network
    info                     {bus: String, slot: Integer}  -- cell_info{mode, band, rsrp/rsrq/sinr, dl_bandwidth}, ipv4{ip, gateway, dns}
    status                   {bus: String, slot: Integer}  -- traffic_total, dial_status, callcode, call_reason
    daig_info                {bus: String, slot: Integer}  -- (sic) diag info
    debug_at_info            {bus: String, slot: Integer}
    update_info              {bus: String, slot: Integer}
    update_status            {bus: String, slot: Integer, dial_progress: Integer, dial_status: String}

cellular.cm                                                  -- "connection manager" — dial control
    cm_get_status            {bus: String, slot: Integer, flag: Integer}
    cm_start_dial            {bus: String, slot: Integer, source: Integer}
    cm_stop_dial             {bus: String, slot: Integer, flag: Integer, source: Integer}
    cm_update_dial_status    {bus: String, slot: Integer, update: Table}
    cm_dial_restore          {bus: String, slot: Integer, nw_st: Integer}
    cm_update_conn_status    {bus: String, slot: Integer, enable: Integer}

cellular.collect
    get_signals              {bus: String, time: Integer}   -- historical signal samples
    get_traffic              {bus: String}                  -- aggregated traffic
    clean_traffic            {bus: String, slot: Integer, type: Integer}
```

`cellular.status` and `cellular.failover` exist but their method
signatures were not captured in this session — add when next inspected.

Equivalent SSH commands for the four WS events (drop-in replacements
once we figure out the WS subscribe message, or for one-shot probing):

```sh
ubus call cellular.sim     info       '{"bus":"cpu"}'                    # ≈ cellular.sims_info
ubus call cellular.sim     status     '{"bus":"cpu"}'                    # ≈ cellular.sims_status
ubus call cellular.network info       '{"bus":"cpu","slot":1}'           # ≈ cellular.networks_info (per slot)
ubus call cellular.network status     '{"bus":"cpu","slot":1}'           # ≈ cellular.networks_status (per slot)
ubus call cellular.collect get_traffic '{"bus":"cpu"}'                   # aggregated traffic
ubus call cellular.modem   info       '{"bus":"cpu"}'                    # hardware: model, IMEIs, supported bands
```

The WS clearly multiplexes these — the event names are pluralised
(`sims_info` vs the singular `sim`) and combine per-slot results into
one `sims: [...]` / `networks: [...]` array, which suggests the WS
service is calling each per-slot endpoint and merging.

#### Underlying ubus catalog (from `ubus call cellular.modem info`)

The hardware sub-tree (not the live signal — that still needs WS) is
already accessible via SSH and is useful when wiring new fields:

```json
{
  "modems": [{
    "bus": "cpu",
    "name": "RG650V-EU",
    "version": "QRM650VEU00ADR02A04G8G_OCPU_RGH_01.005.01.005",
    "vendor": "quectel",
    "sim_slot_num": 2,
    "standby_type": 1,
    "type": 0,
    "mtu_need_sync": 1,
    "support_pin_count": true,
    "offline_doc": true,
    "at_port": "/dev/smd9",
    "devices": ["/dev/smd9"],
    "protocols": ["rmnet"],
    "imei": [{"slot":"1","imei":"…"}, {"slot":"2","imei":"…"}],
    "supports_ip_type": [...],
    "slot_support_esim": [2],
    "band": {
      "LTE":    [1,3,5,7,8,20,28,32,38,40,41,42,43],
      "NR-NSA": [1,3,5,7,8,20,26,28,38,40,41,75,76,77,78],
      "NR-SA":  [1,3,5,7,8,20,26,28,38,40,41,75,76,77,78]
    },
    "signal_support": true,
    "sms_support": true,
    "lock_tower_support": true,
    "lock_carrier_support": true,
    "network_type_support": true
  }]
}
```

## Finding a new RPC method

When the binary is missing a field that the device clearly exposes via
its web UI, this is the fastest path:

1. Open the web UI in a browser (`http://192.168.8.1/`).
2. Open DevTools → Network, filter by `rpc`.
3. Navigate to the page (or scroll to the tile) that shows the missing
   data. Many GL.iNet UI pages refresh on focus; look for a fresh `/rpc`
   request at the moment the data appears.
4. The interesting call is the one whose **response body contains the
   missing field** (search for "rsrp", "carrier", "operator", whatever).
   The request body's `params` array has the shape
   `["<sid>", "<SERVICE>", "<METHOD>", <args>]`.
5. The first element is the active sid — replace with the sid your
   client just got from `login`. The other three are device-stable and
   are what you wire into `MudiClient`.

If the page refreshes too fast to see the call, throttle the Network tab
to "Slow 3G" or replay the request from DevTools' "Resend" menu.

Reflection (a generic "list methods on this service" RPC) is **not**
available — `list` / `methods` / `acl.list` all return `-32601`. The
only way to enumerate services without UI-sniffing is SSH + `ubus list`,
which is out of scope for this binary.

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
