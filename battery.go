package main

// Remaining-battery-time estimation.
//
// Neither router reports a time: the M7010 gives `battery.voltage` (a
// percent despite the name) plus a charging flag, and the Mudi gives
// `system.mcu.charge_percent` plus `charging_status`. No current, no
// voltage, no time field. So the time has to come from how fast the
// percent moves — and this binary is one-shot, so "how fast" has to
// survive between invocations. Hence the one piece of state in an
// otherwise stateless tool: a small JSON file under XDG_STATE_HOME.
//
// Two things make the estimate honest despite 1% quantisation:
//
//  1. **Edge anchoring.** A sample that reads 88% tells us nothing about
//     where inside that percent we are, so measuring between two arbitrary
//     samples carries a ±1% error on a span that may itself be 1%. We
//     instead anchor on the *instants the percent changed* — the first
//     sample of each new percent. Between two such edges the drop is
//     exactly (p_first - p_last) percent, and the only error left is the
//     poll interval (30s from noctalia).
//
//  2. **A warm-up gate.** Even edge-anchored, one percent step is a
//     sample size of one. We wait for batteryMinSpanPct steps before
//     trusting the measurement, which at a typical ~6 min/percent means
//     the measured figure appears after ~20 min of observation.
//
// Until then the datasheet runtime for the detected model is used, and the
// estimate is labelled "typical" so a guess never masquerades as a
// measurement. Charging has no such fallback: neither vendor publishes a
// charge time, so time-to-full only ever appears once measured.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// Percent steps that must be observed before the measured rate is
	// trusted. Three edges of movement keep the quantisation error under
	// about a third of what a single step would give; raise it for a
	// steadier (but slower-appearing) figure.
	batteryMinSpanPct = 3

	// A silence longer than this breaks the history: the laptop slept, the
	// widget was hidden, or the router was off. We cannot tell "did not
	// discharge because it was off" from "discharges very slowly", and the
	// second reading inflates the estimate, so the samples before the gap
	// are dropped rather than trusted.
	batteryGapMax = 20 * time.Minute

	// A percent moving this far in one step is a gauge reset or a battery
	// swap, not discharge. Start over.
	batteryJumpMax = 10

	// History bounds. Runs are one entry per distinct percent, so a full
	// discharge is ~100 entries; these caps only bite on pathological
	// gauges.
	batteryMaxAge  = 12 * time.Hour
	batteryMaxRuns = 256

	// Anything past this is noise, not an estimate.
	batteryMaxMinutes = 48 * 60
)

// batteryRun is a span during which the percent held one value. From is
// the instant we first saw that percent (the edge we measure against); To
// is the most recent sample still reading it (what gap detection needs).
type batteryRun struct {
	Pct  int   `json:"pct"`
	Chg  bool  `json:"chg"`
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

type batteryHistory struct {
	Runs []batteryRun `json:"runs"`
}

type batteryState struct {
	Version int                        `json:"v"`
	Devices map[string]*batteryHistory `json:"devices"`
}

// batteryEstimate is the rendered answer. Minutes == 0 means "not known
// yet" and every caller omits the line entirely rather than showing a
// placeholder.
type batteryEstimate struct {
	Minutes int    `json:"minutes"`
	Text    string `json:"text"`              // "4h12m"
	Source  string `json:"source"`            // "measured" | "typical"
	ToFull  bool   `json:"to_full,omitempty"` // charging: time to full, not to empty
}

func (e batteryEstimate) known() bool { return e.Minutes > 0 }

// RowLabel / RowValue render the TUI's own dashboard row. The estimate
// gets a row rather than being appended to the battery row because the box
// is 46 columns wide and the combined line wraps.
func (e batteryEstimate) RowLabel() string {
	if !e.known() {
		return ""
	}
	if e.ToFull {
		return "To full"
	}
	return "Remaining"
}

func (e batteryEstimate) RowValue() string {
	if !e.known() {
		return ""
	}
	s := "~" + e.Text
	if e.Source == "typical" {
		s += " (typical)"
	}
	return s
}

// Label is the phrase appended after the percent in the widget tooltip,
// e.g. "~4h12m left" / "~7h02m left (typical)" / "~22m to full".
func (e batteryEstimate) Label() string {
	if !e.known() {
		return ""
	}
	s := "~" + e.Text
	if e.ToFull {
		s += " to full"
	} else {
		s += " left"
	}
	if e.Source == "typical" {
		s += " (typical)"
	}
	return s
}

// xdgStateDir mirrors xdgConfigDir (device.go) for XDG_STATE_HOME, whose
// spec default is ~/.local/state. Returns "" when $HOME is unset; callers
// must treat that as "no state available" rather than building a path.
func xdgStateDir() string {
	if v := os.Getenv("TPLINK_STATE_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state")
	}
	return ""
}

func batteryStatePath() string {
	dir := xdgStateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "tplink-m7010", "battery.json")
}

func loadBatteryState() *batteryState {
	st := &batteryState{Version: 1, Devices: map[string]*batteryHistory{}}
	path := batteryStatePath()
	if path == "" {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	var got batteryState
	// A truncated or hand-mangled file is not worth an error path: the
	// history is a cache of observations, so starting over costs one
	// warm-up window and nothing else.
	if err := json.Unmarshal(data, &got); err != nil || got.Version != 1 || got.Devices == nil {
		return st
	}
	return &got
}

// saveBatteryState writes atomically (temp + rename) because a TUI and a
// bar poll can run concurrently; a torn file would be discarded on the
// next read, losing the window. Last writer wins, which is fine — both
// are writing the same observation.
func saveBatteryState(st *batteryState) {
	path := batteryStatePath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".battery-*.json")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
	}
}

// appendBatterySample folds one observation into the run list, resetting
// the history whenever continuity is broken (see the reset rules in the
// file header). Returns the new list.
func appendBatterySample(runs []batteryRun, pct int, chg bool, now time.Time) []batteryRun {
	t := now.Unix()
	fresh := []batteryRun{{Pct: pct, Chg: chg, From: t, To: t}}
	if len(runs) == 0 {
		return fresh
	}
	last := &runs[len(runs)-1]

	switch {
	case t < last.To: // clock moved backwards (NTP step, timezone-less RTC)
		return fresh
	case time.Duration(t-last.To)*time.Second > batteryGapMax:
		return fresh
	case chg != last.Chg: // charger plugged or unplugged — different physics
		return fresh
	case abs(pct-last.Pct) >= batteryJumpMax:
		return fresh
	case pct == last.Pct:
		last.To = t
		return trimBatteryRuns(runs, now)
	}
	runs = append(runs, batteryRun{Pct: pct, Chg: chg, From: t, To: t})
	return trimBatteryRuns(runs, now)
}

func trimBatteryRuns(runs []batteryRun, now time.Time) []batteryRun {
	cutoff := now.Add(-batteryMaxAge).Unix()
	i := 0
	// Keep at least the final two runs: one edge is worth nothing, but two
	// is the seed of the next measurement.
	for i < len(runs)-2 && runs[i].To < cutoff {
		i++
	}
	runs = runs[i:]
	if len(runs) > batteryMaxRuns {
		runs = runs[len(runs)-batteryMaxRuns:]
	}
	return runs
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// measuredRate returns percent-per-hour derived from the run list, or 0
// when the warm-up gate has not been cleared.
//
// runs[0] is deliberately skipped: we joined that percent partway through,
// so its From is when we started looking, not when the percent was
// entered. Only runs[1:] carry true edges.
func measuredRate(runs []batteryRun, charging bool) float64 {
	if len(runs) < 3 {
		return 0 // need two real edges, so three runs
	}
	edges := runs[1:]
	first, last := edges[0], edges[len(edges)-1]
	if first.Chg != charging || last.Chg != charging {
		return 0
	}

	span := first.Pct - last.Pct // positive while discharging
	if charging {
		span = -span
	}
	if span < batteryMinSpanPct {
		return 0 // warm-up, or the gauge moved the wrong way
	}
	elapsed := time.Duration(last.From-first.From) * time.Second
	if elapsed <= 0 {
		return 0
	}
	return float64(span) / elapsed.Hours()
}

// estimateBattery turns a run list into a rendered estimate, falling back
// to the model's datasheet runtime while the measurement warms up.
func estimateBattery(d *SupportedDevice, pct int, charging bool, runs []batteryRun) batteryEstimate {
	if pct <= 0 || pct > 100 {
		return batteryEstimate{}
	}
	remaining := pct
	if charging {
		if pct >= 100 {
			return batteryEstimate{} // full; nothing to count down
		}
		remaining = 100 - pct
	}

	rate, source := measuredRate(runs, charging), "measured"
	if rate <= 0 {
		// No datasheet charge time is published for either model, so the
		// bootstrap covers discharge only.
		if charging || d == nil || d.TypicalRuntime <= 0 {
			return batteryEstimate{}
		}
		rate, source = 100/d.TypicalRuntime.Hours(), "typical"
	}

	minutes := int(float64(remaining)/rate*60 + 0.5)
	if minutes <= 0 || minutes > batteryMaxMinutes {
		return batteryEstimate{}
	}
	return batteryEstimate{
		Minutes: minutes,
		Text:    formatMinutes(minutes),
		Source:  source,
		ToFull:  charging,
	}
}

// formatMinutes renders hh:mm as "4h12m" (and "47m" under the hour),
// matching the fleet's other battery widgets.
func formatMinutes(minutes int) string {
	h, m := minutes/60, minutes%60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// batteryRemaining is the one call sites need: record this observation,
// then answer with the best estimate available. Every failure mode ends in
// an unknown estimate, never an error — a widget must not break because a
// state file could not be written.
func batteryRemaining(d *SupportedDevice, s *Status) batteryEstimate {
	if d == nil || s == nil || s.BatteryPercent <= 0 {
		return batteryEstimate{}
	}
	now := time.Now()

	st := loadBatteryState()
	if st.Devices == nil {
		st.Devices = map[string]*batteryHistory{}
	}
	h := st.Devices[d.ID]
	if h == nil {
		h = &batteryHistory{}
		st.Devices[d.ID] = h
	}
	h.Runs = appendBatterySample(h.Runs, s.BatteryPercent, s.BatteryCharging, now)
	saveBatteryState(st)

	return estimateBattery(d, s.BatteryPercent, s.BatteryCharging, h.Runs)
}
