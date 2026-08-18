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
// Until then a *learned* rate is used — what this router has actually
// averaged across previous sessions — falling back to the model's
// datasheet runtime only when nothing has been learned yet. Each source
// is labelled ("avg" / "typical") so a guess never masquerades as a
// measurement.
//
// Learning across sessions matters because a window reset means only "I
// cannot measure across this discontinuity", not "I know nothing about
// this router". Plugging the charger in for five minutes used to throw
// away an hour of evidence and send the estimate back to a vendor number.
// So the window and the knowledge are now separate: the run list resets,
// while every percent step it observed has already been banked into a
// pooled (percent, hours) accumulator that survives.
//
// The pool is a bounded average, not a lifetime one: once it holds
// batteryLearnMaxPct percent-points of evidence — about two full
// discharges — older evidence is scaled down. A cell that ages, or a
// usage pattern that changes, therefore shows up within a couple of
// cycles instead of being diluted forever. Note what the pool implicitly
// selects for: samples only accrue while something is polling, i.e. while
// the laptop is awake behind the router, so what is learned is the rate
// under *use*, not idle standby.
//
// Charging is pooled separately and has no datasheet fallback (no vendor
// publishes a charge time), so time-to-full appears from the second
// session onwards rather than never.

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

	// Bounded memory for the learned rate, in percent-points of observed
	// movement. ~2 full discharges: enough to average out one unusually
	// idle or unusually heavy window, short enough that an ageing cell
	// surfaces within a couple of cycles.
	batteryLearnMaxPct = 200

	// Evidence needed before the learned rate outranks the datasheet.
	// A single decent session (the user's "we consumed 10%") clears it.
	batteryLearnMinPct = 5

	// Weight of the datasheet as a prior inside the discharge pool, in
	// percent-points. Without it, the first banked window — which may be
	// an idle one — would swing the estimate wholesale; with it, early
	// evidence blends toward the vendor figure and then washes out as
	// real observations accumulate. Charging gets no prior: there is no
	// published number to prior it with.
	batteryLearnPrior = 10
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

// batteryEdge marks the last transition already folded into the pool, so
// repeated observations can bank incrementally without double counting.
// Cleared on every window reset — the interval spanning a discontinuity
// is exactly the one we cannot trust.
type batteryEdge struct {
	Pct int   `json:"pct"`
	T   int64 `json:"t"`
}

// batteryLearned is a pooled average kept as its two sums rather than as
// a rate. Summing percent and hours separately is the physically correct
// pooling: averaging the per-window *rates* would weight a 3-minute
// window the same as a 3-hour one.
type batteryLearned struct {
	Pct     float64 `json:"pct"`   // percent-points of evidence
	Hours   float64 `json:"hours"` // hours they took
	Obs     int     `json:"obs"`   // banked steps, for diagnostics
	Updated int64   `json:"updated"`
}

func (l *batteryLearned) rate() float64 {
	if l == nil || l.Hours <= 0 {
		return 0
	}
	return l.Pct / l.Hours
}

type batteryHistory struct {
	Runs   []batteryRun `json:"runs"`
	Banked *batteryEdge `json:"banked,omitempty"`
	// Discharge and Charge are pooled separately: they are different
	// physics, and only one of them has a datasheet to fall back on.
	Discharge *batteryLearned `json:"discharge,omitempty"`
	Charge    *batteryLearned `json:"charge,omitempty"`
}

func (h *batteryHistory) learned(charging bool) *batteryLearned {
	if charging {
		return h.Charge
	}
	return h.Discharge
}

type batteryState struct {
	Version int                        `json:"v"`
	Devices map[string]*batteryHistory `json:"devices"`
}

// batteryEstimate is the rendered answer. Minutes == 0 means "not known
// yet" and every caller omits the line entirely rather than showing a
// placeholder.
type batteryEstimate struct {
	Minutes int     `json:"minutes"`
	Text    string  `json:"text"`   // "4h12m"
	Source  string  `json:"source"` // "measured" | "learned" | "typical"
	Rate    float64 `json:"rate_pct_per_hour"`
	ToFull  bool    `json:"to_full,omitempty"` // charging: time to full, not to empty
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
	if m := e.sourceMark(); m != "" {
		s += " " + m
	}
	return s
}

// sourceMark names where the rate came from, for every surface that
// renders one. An unmarked figure is measured in this session; anything
// else says so, because a guess that reads as a measurement is the
// failure mode worth engineering against.
func (e batteryEstimate) sourceMark() string {
	switch e.Source {
	case "learned":
		return "(avg)"
	case "typical":
		return "(typical)"
	}
	return ""
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
	if m := e.sourceMark(); m != "" {
		s += " " + m
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

// What one observation did to the window. The caller needs to know,
// because a new edge is bankable evidence and a reset invalidates the
// pending interval.
type batteryEvent int

const (
	battReset batteryEvent = iota // continuity broken; window restarted
	battHeld                      // same percent, run extended
	battEdge                      // percent changed; a real transition
)

// appendBatterySample folds one observation into the run list, resetting
// the window whenever continuity is broken (see the reset rules in the
// file header). Returns the new list and what happened.
func appendBatterySample(runs []batteryRun, pct int, chg bool, now time.Time) ([]batteryRun, batteryEvent) {
	t := now.Unix()
	fresh := []batteryRun{{Pct: pct, Chg: chg, From: t, To: t}}
	if len(runs) == 0 {
		return fresh, battReset
	}
	last := &runs[len(runs)-1]

	switch {
	case t < last.To: // clock moved backwards (NTP step, timezone-less RTC)
		return fresh, battReset
	case time.Duration(t-last.To)*time.Second > batteryGapMax:
		return fresh, battReset
	case chg != last.Chg: // charger plugged or unplugged — different physics
		return fresh, battReset
	case abs(pct-last.Pct) >= batteryJumpMax:
		return fresh, battReset
	case pct == last.Pct:
		last.To = t
		return trimBatteryRuns(runs, now), battHeld
	}
	runs = append(runs, batteryRun{Pct: pct, Chg: chg, From: t, To: t})
	return trimBatteryRuns(runs, now), battEdge
}

// observe records one reading: it advances the window and banks whatever
// new evidence that produced. Banking is incremental — each transition is
// folded exactly once, against the previous transition — so trimming or a
// long session can never double count or lose a step.
func (h *batteryHistory) observe(pct int, chg bool, now time.Time) {
	runs, ev := appendBatterySample(h.Runs, pct, chg, now)
	h.Runs = runs

	switch ev {
	case battReset:
		// The interval spanning the discontinuity is unmeasurable, so it
		// is dropped — but everything banked before it stays.
		h.Banked = nil
	case battEdge:
		edge := runs[len(runs)-1]
		if h.Banked != nil {
			h.bank(chg, *h.Banked, edge, now)
		}
		h.Banked = &batteryEdge{Pct: edge.Pct, T: edge.From}
	}
}

// bank folds one edge-to-edge interval into the pooled average.
func (h *batteryHistory) bank(chg bool, from batteryEdge, to batteryRun, now time.Time) {
	moved := from.Pct - to.Pct
	if chg {
		moved = -moved
	}
	hours := (time.Duration(to.From-from.T) * time.Second).Hours()
	// A gauge that wobbled the wrong way is not evidence about a rate;
	// drop the interval rather than pooling a negative.
	if moved <= 0 || hours <= 0 {
		return
	}

	l := h.learned(chg)
	if l == nil {
		l = &batteryLearned{}
		if chg {
			h.Charge = l
		} else {
			h.Discharge = l
		}
	}
	l.Pct += float64(moved)
	l.Hours += hours
	l.Obs++
	l.Updated = now.Unix()

	// Bounded memory: scale both sums down together, which preserves the
	// rate while making room for newer evidence to move it.
	if l.Pct > batteryLearnMaxPct {
		k := batteryLearnMaxPct / l.Pct
		l.Pct *= k
		l.Hours *= k
	}
}

// learnedRate is the cross-session average for this router, or 0 when too
// little has been banked to beat the datasheet.
func (h *batteryHistory) learnedRate(d *SupportedDevice, charging bool) float64 {
	l := h.learned(charging)
	if l == nil || l.Pct < batteryLearnMinPct || l.Hours <= 0 {
		return 0
	}
	pct, hours := l.Pct, l.Hours
	if !charging && d != nil && d.TypicalRuntime > 0 {
		// Blend in the datasheet as a weak prior (see batteryLearnPrior).
		pct += batteryLearnPrior
		hours += batteryLearnPrior * d.TypicalRuntime.Hours() / 100
	}
	if hours <= 0 {
		return 0
	}
	return pct / hours
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

// estimateBattery renders the best estimate available, in strict order of
// how much it knows about *this* battery right now:
//
//	measured — this session's own rate, once the warm-up gate is cleared
//	learned  — what this router has averaged across previous sessions
//	typical  — the vendor's datasheet runtime (discharge only)
//
// The ordering is the whole point of the fallback chain: an in-session
// rate reflects current load, so it wins as soon as it exists; the
// learned rate reflects this specific unit under this specific usage, so
// it beats a vendor claim about a new cell in a lab.
func estimateBattery(d *SupportedDevice, pct int, charging bool, h *batteryHistory) batteryEstimate {
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

	if h == nil {
		h = &batteryHistory{}
	}
	rate, source := measuredRate(h.Runs, charging), "measured"
	if rate <= 0 {
		rate, source = h.learnedRate(d, charging), "learned"
	}
	if rate <= 0 {
		// No datasheet charge time is published for either model, so the
		// last-resort bootstrap covers discharge only.
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
		Rate:    rate,
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

// batteryNow is time.Now, indirected so tests can drive the whole
// record-then-estimate path across a plausible span of minutes instead of
// crowding every observation into one second.
var batteryNow = time.Now

// batteryRemaining is the one call sites need: record this observation,
// then answer with the best estimate available. Every failure mode ends in
// an unknown estimate, never an error — a widget must not break because a
// state file could not be written.
func batteryRemaining(d *SupportedDevice, s *Status) batteryEstimate {
	if d == nil || s == nil || s.BatteryPercent <= 0 {
		return batteryEstimate{}
	}
	now := batteryNow()

	st := loadBatteryState()
	if st.Devices == nil {
		st.Devices = map[string]*batteryHistory{}
	}
	h := st.Devices[d.ID]
	if h == nil {
		h = &batteryHistory{}
		st.Devices[d.ID] = h
	}
	h.observe(s.BatteryPercent, s.BatteryCharging, now)
	saveBatteryState(st)

	return estimateBattery(d, s.BatteryPercent, s.BatteryCharging, h)
}

// --- inspection ---

// batteryLearnedOut is the pooled knowledge, rendered for --json.
//
// ObservedRuntimeHours is deliberately NOT called battery health. It is
// how long a full charge has actually lasted *under this usage*, which
// mixes cell ageing with how hard the router was worked — five clients
// streaming on a weak signal drain a healthy battery fast. Telling those
// apart needs a full-charge capacity readout (neither device exposes one)
// or a controlled-load test. Compare it against TypicalRuntime for a
// trend over months, not as a health percentage.
type batteryLearnedOut struct {
	DischargeRate        float64 `json:"discharge_pct_per_hour,omitempty"`
	ObservedRuntimeHours float64 `json:"observed_runtime_hours,omitempty"`
	ChargeRate           float64 `json:"charge_pct_per_hour,omitempty"`
	TypicalRuntimeHours  float64 `json:"typical_runtime_hours,omitempty"`
	Observations         int     `json:"observations"`
	Updated              int64   `json:"updated,omitempty"`
}

// batteryLearnedFor re-reads the state file, so it is for --json only —
// the widget path already holds the history it needs and must not pay for
// a second read on every tick.
func batteryLearnedFor(d *SupportedDevice) *batteryLearnedOut {
	if d == nil {
		return nil
	}
	h := loadBatteryState().Devices[d.ID]
	if h == nil || (h.Discharge == nil && h.Charge == nil) {
		return nil
	}
	out := &batteryLearnedOut{TypicalRuntimeHours: d.TypicalRuntime.Hours()}
	if r := h.Discharge.rate(); r > 0 {
		out.DischargeRate = r
		out.ObservedRuntimeHours = 100 / r
		out.Observations += h.Discharge.Obs
		out.Updated = h.Discharge.Updated
	}
	if r := h.Charge.rate(); r > 0 {
		out.ChargeRate = r
		out.Observations += h.Charge.Obs
		if h.Charge.Updated > out.Updated {
			out.Updated = h.Charge.Updated
		}
	}
	return out
}
