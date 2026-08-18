# Noctalia integration — this machine

The `--noctalia` mode emits a single JSON line for noctalia's bar
**CustomButton** widget in its script-parsing mode. This documents the
setup and, importantly, which noctalia versions it works on.

## Version caveat (read this first)

The exec-JSON CustomButton contract below is implemented by the **QML
noctalia** (`noctalia-shell`, up to and including the v4.x series —
verified against the v4.7.7 source). The **v5 C++ rewrite changed the
CustomButton into a static button** (label/tooltip/commands come from
settings; it does not run a script or parse JSON). Custom bar tiles in
v5 are done through the plugin system instead.

If/when this machine moves to v5, the path forward is a small QML
plugin under `~/.config/noctalia/plugins/` that consumes the binary's
stable scripting interface:

```sh
tplink-m7010 --json     # parsed status as machine-readable JSON
```

`--json` exists precisely so the next integration doesn't depend on any
bar's widget schema.

## Output contract (v4 CustomButton, parseJson mode)

One JSON object on stdout:

| Field       | Used for                                                      |
| ----------- | ------------------------------------------------------------- |
| `text`      | The tile label, e.g. `5G ▅▆▆░░  88%  32.1GB`                  |
| `tooltip`   | Multi-line detail (connection, signal, battery + remaining time, data, speed…) |
| `textColor` | One of `primary / secondary / tertiary / error / none`        |

The widget also accepts `icon`, `iconColor`, and a legacy `color` field
— we don't set them today.

The battery line carries the estimated remaining time when one is
available — `Battery: 56% · ~4h29m left (typical)`, or without the
`(typical)` marker once the rate has been measured rather than taken
from the datasheet. It is in the tooltip only, never in `text`: the tile
is a four-field glance line, and the caveat needs somewhere to be read.
See the README's "Remaining battery time" for how the number is derived.

States the binary emits:

- **Router present and healthy** → full text; `textColor` maps from the
  shared class: warning → `secondary`, critical → `error`, else `none`.
- **No supported router detected** → empty object `{}` so the tile
  collapses (see below).
- **Router detected but fetch failed** (wrong password, firmware
  change) → `text: "--"`, the error in the tooltip, `textColor:
  "error"`. Since autodetect confirms the protocol before picking a
  device, this state always means a *real* problem worth showing —
  it's deliberately not hidden.

## Widget settings

In noctalia's Settings → Bar → CustomButton (the 30s cadence is what
PERFORMANCE.md budgets; the click handler reuses the waybar TUI script):

| Setting            | Value                                             |
| ------------------ | ------------------------------------------------- |
| Text command       | `~/.local/bin/tplink-m7010 --noctalia`            |
| Parse JSON         | on                                                |
| Interval           | 30000 ms (PERFORMANCE.md budgets this cadence)    |
| Text collapse      | `/^$/` — regex matching empty text hides the tile |
| Left click command | `~/.config/waybar/scripts/mifi-tui.sh` (opens TUI) |

Notes on the collapse setting: the widget hides when its *text* matches
the "Text collapse" pattern (exact string, or `/regex/`). The empty
`{}` output produces empty text, so `/^$/` is what makes the
unreachable-router case collapse the tile instead of leaving an empty
pill.

The interval floor in the widget is 250 ms — don't be tempted; every
tick is a full login (see PERFORMANCE.md for the per-tick cost).

## Troubleshooting

- **Tile missing while connected** — run
  `~/.local/bin/tplink-m7010 --noctalia` directly. `{}` means
  autodetect found no supported router (check `ip route show default`);
  a `"--"` tile with an error tooltip means the router was found but
  login/fetch failed (usually the password file).
- **Tile shows an empty pill instead of hiding** — the "Text collapse"
  setting isn't matching; set it to `/^$/`.
- **Everything works in waybar but not noctalia** — check the noctalia
  version: v5's CustomButton doesn't execute scripts (see the version
  caveat above).
