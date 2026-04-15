# Architecture

Two files, one binary. Kept deliberately flat — there's no plugin system to
grow into.

## Process flow

```
main.go :: main()
   ├── flag.Parse()              -- CLI flags, TPLINK_ADDR / TPLINK_PASS env
   ├── --waybar → runWaybar()    -- one-shot JSON line to stdout
   ├── --raw    → runRaw()       -- pretty-print raw API responses
   └── (default)→ runTUI()       -- Bubble Tea dashboard

all three paths eventually:
   fetchStatus()
     └── NewClient(addr)
         ├── Login(pass)          -- nonce fetch, AES+RSA login, store token+keys
         ├── GetStatus()          -- module=status action=0 → decode tree
         ├── GetFlowStats(s)      -- module=flowstat action=0 → mutate s
         └── Logout()             -- module=authenticator action=3
```

`fetchStatus` opens a fresh session on every invocation. The modem is happy
with that; it avoids having to persist a token across waybar ticks.

## Client state

`Client` holds exactly what's needed to send one more encrypted request:

```
baseURL      "http://192.168.0.1"
password     "…"               -- kept because every request signs md5('admin'+pass)
token        "SSnASCGDZclim6kp" -- from login response, echoed in every body
aesKey/aesIV [16]byte digits   -- random per login, reused for this session
rsaMod       *big.Int           -- server RSA modulus from step-1
rsaPubKey    *big.Int           -- server RSA exponent (always 0x10001)
seqNum       int                -- from step-1, never incremented
httpClient   *http.Client       -- 5s timeout
debug        bool               -- --debug flag
```

## Request helpers

- `postRaw(endpoint, body)` — send bytes, read bytes, log if `debug`.
- `encryptedRequest(endpoint, payload)` — build `{data, sign}` envelope,
  POST, AES-decrypt the response, parse as JSON. All post-login calls go
  through this.

## Response parsing

The firmware returns nested objects (`deviceInfo`, `wan`, `battery`,
`connectedDevices`, `settings`). We tolerate firmware drift by:

- Using `first*` helpers that try multiple field names in order. Add more
  names when a new firmware changes them, don't rip out the old ones.
- Mapping numeric enums to strings (`networkTypeStr`) in one place.
- Parsing decimal-string byte counts (e.g. `"14473800628.000000"`) via
  `parseFloatStr`, which falls through to numeric types if the firmware
  ever stops quoting them.

## TUI model (Bubble Tea)

Standard Elm-ish shape:

- `model` holds `*Status`, last error, loading flag, refresh interval.
- `Init` kicks off `fetchCmd` plus a `tickCmd`.
- `tickMsg` every `--refresh` seconds triggers another `fetchCmd`.
- `r` re-fetches on demand; `q/esc/ctrl+c` quits.
- `View` renders with lipgloss — a single rounded-border box with aligned
  label+value rows. Colors shift with battery and data-limit thresholds.

## Waybar output

Single line of JSON on stdout: `{text, tooltip, class, alt}`. `class` is
picked from `good / warning / critical / disconnected` so CSS in `style.css`
can colorize the tile. The wrapper in `contrib/mifi.sh` exits with no output
when the modem isn't reachable, which makes waybar hide the module.
