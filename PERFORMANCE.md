# Performance & power impact

This binary is invoked every 30 seconds by noctalia (and on a similar
cadence by waybar). Numbers below are from two complementary harnesses
run against a live Mudi 7 GL-E5800 (LAN, 1ms RTT, firmware 4.8.3) on
a ThinkPad X13 Gen 3 (i5-1240P) running Arch Linux:

1. **`/tmp/measure.py`** — Python wrapper, 30 runs. Uses
   `subprocess.run` + `resource.getrusage(RUSAGE_CHILDREN)` deltas for
   CPU and reads `/proc/net/dev` before/after to attribute network
   bytes to the gateway interface. Gives the network number.
2. **`/usr/bin/time -v`** — GNU time, 30 runs aggregated by
   `/tmp/measure2.sh`. Gives single-process peak RSS (not inflated by
   the harness's cumulative `ru_maxrss`) and the context-switch
   counts.

## Per-invocation cost

| Metric                           | mean   | median | p95    | min    | max    | source |
| -------------------------------- | ------ | ------ | ------ | ------ | ------ | ------ |
| Wall time (ms)                   | 104.6  | 106.9  | 117.8  | 79.8   | 129.1  | python |
| CPU user (ms)                    | 4.0    | 4.0    | 7.0    | 0.8    | 7.3    | python |
| CPU sys (ms)                     | 7.6    | 7.6    | 10.1   | 5.0    | 10.7   | python |
| **CPU total (ms)**               | **11.6** | **11.7** | **13.1** | **8.9** | **13.8** | python |
| % of one core during run         | 9.5    | 9.0    | 11     | 7      | 13     | gnu-time |
| **Peak RSS (per process, kB)**   | **9671** | **9680** | **9936** | **9424** | **10064** | gnu-time |
| Page faults — minor              | 744    | 736    | 788    | 699    | 795    | gnu-time |
| Page faults — major              | 0      | 0      | 0      | 0      | 0      | both |
| Voluntary context switches       | 114    | 116    | 124    | 95     | 132    | gnu-time |
| Involuntary context switches     | 2.0    | 2.0    | 3.0    | 0      | 5      | gnu-time |
| Block I/O reads                  | 0      | 0      | 0      | 0      | 0      | both |
| Block I/O writes                 | 0      | 0      | 0      | 0      | 0      | both |
| Network RX (B)                   | 6206   | 5845   | 6186   | 5782   | 11626  | python |
| Network TX (B)                   | 3046   | 2654   | 5177   | 2493   | 7771   | python |

Key observations:

- **Wall is dominated by the WebSocket subscribe + first-burst wait
  (~80 ms after the TCP handshake).** CPU itself is ~12 ms across
  user + sys — SHA-256 crypt for login plus parsing six JSON
  responses.
- **Peak RSS ~9.7 MB** per process. Normal for a Go binary with
  Bubble Tea / lipgloss linked in (Go runtime alone is ~5-7 MB).
  (The Python harness reports ~15 MB because it accumulates
  `ru_maxrss` across all children; GNU time's per-run number is the
  honest one.)
- **~114 voluntary context switches per run, only ~2 involuntary.**
  The process is almost always blocked on network I/O — the
  scheduler hands the core to other work cleanly. Almost never gets
  preempted, which means the per-tick wall time is stable.
- **Zero major page faults, zero block I/O.** The 7.5 MB binary
  stays in the kernel disk cache between ticks, so every invocation
  is a warm exec.
- **~9 KB of network traffic per tick** (RX+TX), split between the
  RPC login handshake (~3 KB) and the WebSocket burst (~6 KB).

## Daily impact at noctalia's 30s cadence

`24 × 60 × 60 / 30 = 2880` ticks/day.

| Metric              | per tick | per day            |
| ------------------- | -------- | ------------------ |
| CPU time            | 12 ms    | 33 s (~0.55 min)   |
| Network bytes       | 9 KB     | 26 MB              |
| Memory peak         | 9.7 MB   | 9.7 MB (per-process; processes are short-lived, no carry-over) |
| Disk I/O            | 0        | 0                  |

**Energy estimate** (rough, Linux laptop):

The 12 ms of CPU is ~95 % idle waiting on network, so peak instantaneous
draw is dominated by C-state transitions, not core voltage. A ballpark
of 5-8 W incremental when a core is briefly busy gives:

  12 ms × 7 W × 2880 / 3600 s/h ≈ **0.07 Wh / day**

A typical laptop battery is 50-60 Wh. So one **full charge** of the
laptop pays for **~700-800 days** of this widget polling — i.e.,
several years of widget runs eat one charge cycle, with everything
else held equal. The wifi radio is already on for normal connectivity
so the marginal radio cost of an extra 9 KB/tick is in the noise.

In practice you will not be able to measure this widget's impact
against the noise floor of an idle laptop.

## Unreachable-router case

When autodetect fails fast — no supported router on the LAN — the
binary exits in **under 500 ms** (the parallel TCP probe timeout in
`detectDevice`). No login attempt, no widget tooltip, empty JSON
output. The noctalia widget collapses silently.

| Scenario                                    | Wall time   |
| ------------------------------------------- | ----------- |
| Mudi reachable + autodetect                 | ~105 ms     |
| Neither router on LAN, autodetect           | ~500 ms     |
| Explicit `--device m7010` and M7010 absent  | **~5030 ms** |
| Explicit `--device mudi --addr <unreach>`   | **~5030 ms** |

> Note: these numbers were measured before autodetect gained the
> protocol-confirmation probe. The probe adds one unauthenticated HTTP
> round-trip (~5 ms on the LAN) to the reachable cases; the
> unreachable case is unchanged (~500 ms, still bounded by the same
> detect timeout).

The 5 s case is the full `defaultTimeout` in `client.go` — when the
user pins the device explicitly, autodetect is skipped and we try to
log in directly. If the address is dead, the HTTP login dies on the
read timeout. This is acceptable for one-off CLI use (the user is
actively waiting), but **don't pin `--device` in a waybar/noctalia
wrapper** unless you know the router will always be there. Stick with
autodetect.

## Syscall profile

From `strace -c` on a single invocation:

```
395 syscalls total, 2.87 ms in syscall time, 5 % wall.

Top by call count:
   write          ~80   (RPC/WS body writes + stdout)
   read           ~70   (network reads + bufio fills)
   futex          ~50   (Go runtime scheduler)
   epoll_pwait    ~30   (poller blocking on socket I/O)
   mmap           ~40   (one-time runtime + JSON arena)
   rt_sigaction   ~20   (Go signal setup)
   …
```

No surprises. Most calls are either the Go runtime starting up or the
two sockets (RPC + WS) being driven.

## Re-running the measurement

Two harnesses, complementary:

```sh
# Python: 30 runs, gives wall + network bytes
python3 /tmp/measure.py ~/.local/bin/tplink-m7010 30

# GNU time: 30 runs, gives honest per-process RSS + ctx switches
/tmp/measure2.sh 30
```

Both scripts are kept in `/tmp` so they don't clutter the repo; copy
them in if you want them version-controlled. For a quick spot-check
without writing any wrapper:

```sh
/usr/bin/time -v ~/.local/bin/tplink-m7010 --noctalia
```

(GNU time is `pacman -S time` on Arch — installs to `/usr/bin/time`,
distinct from the shell built-in.)
