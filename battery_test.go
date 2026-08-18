package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// base is an arbitrary fixed instant; these tests never call time.Now for
// anything they assert on.
var battBase = time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)

func at(min int) time.Time { return battBase.Add(time.Duration(min) * time.Minute) }

// runsAt builds a history from (minute, percent) pairs, all discharging,
// as if each percent were first seen at that minute.
func runsAt(pairs ...[2]int) []batteryRun {
	var runs []batteryRun
	for _, p := range pairs {
		t := at(p[0]).Unix()
		runs = append(runs, batteryRun{Pct: p[1], From: t, To: t})
	}
	return runs
}

func TestAppendSampleExtendsRunWhilePercentHolds(t *testing.T) {
	runs, _ := appendBatterySample(nil, 88, false, at(0))
	runs, _ = appendBatterySample(runs, 88, false, at(1))
	runs, ev := appendBatterySample(runs, 88, false, at(2))

	if ev != battHeld {
		t.Errorf("event = %v, want battHeld", ev)
	}

	if len(runs) != 1 {
		t.Fatalf("want 1 run while the percent holds, got %d: %+v", len(runs), runs)
	}
	if runs[0].From != at(0).Unix() {
		t.Errorf("From must stay the first sighting (the edge), got %v", runs[0].From)
	}
	if runs[0].To != at(2).Unix() {
		t.Errorf("To must track the latest sample, got %v", runs[0].To)
	}
}

func TestAppendSampleOpensRunOnPercentChange(t *testing.T) {
	runs, _ := appendBatterySample(nil, 88, false, at(0))
	runs, ev := appendBatterySample(runs, 87, false, at(6))

	if ev != battEdge {
		t.Errorf("event = %v, want battEdge", ev)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(runs))
	}
	if runs[1].From != at(6).Unix() {
		t.Errorf("new run must anchor on the transition instant, got %v", runs[1].From)
	}
}

func TestAppendSampleResetsHistory(t *testing.T) {
	tests := []struct {
		name string
		pct  int
		chg  bool
		when time.Time
	}{
		// The router may simply have been off during the silence, which
		// reads as "barely discharging" and would inflate the estimate.
		{"poll gap", 87, false, at(40)},
		// Charging and discharging are different rates entirely.
		{"charger plugged in", 87, true, at(6)},
		// A double-digit jump is a gauge reset or a swapped battery.
		{"implausible jump", 70, false, at(6)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runs, _ := appendBatterySample(nil, 89, false, at(0))
			runs, _ = appendBatterySample(runs, 88, false, at(5))
			runs, ev := appendBatterySample(runs, tc.pct, tc.chg, tc.when)

			if ev != battReset {
				t.Errorf("event = %v, want battReset", ev)
			}
			if len(runs) != 1 {
				t.Fatalf("history must reset to a single run, got %d: %+v", len(runs), runs)
			}
			if runs[0].Pct != tc.pct || runs[0].From != tc.when.Unix() {
				t.Errorf("reset must keep only the new sample, got %+v", runs[0])
			}
		})
	}
}

func TestMeasuredRateWarmUp(t *testing.T) {
	// runs[0] is the percent we joined partway through, so it carries no
	// usable edge; the span is measured across runs[1:] only. Two edges
	// three points apart is the first history that qualifies.
	tests := []struct {
		name    string
		runs    []batteryRun
		wantPct float64 // percent/hour, 0 = must not estimate yet
	}{
		{"single run", runsAt([2]int{0, 90}), 0},
		{"one edge", runsAt([2]int{0, 90}, [2]int{6, 89}), 0},
		{"span of 2 is still warm-up", runsAt(
			[2]int{0, 90}, [2]int{6, 89}, [2]int{12, 88}, [2]int{18, 87}), 0},
		{"span of 3 qualifies", runsAt(
			[2]int{0, 90}, [2]int{6, 89}, [2]int{12, 88}, [2]int{18, 87}, [2]int{24, 86}),
			// edges 89@6min .. 86@24min: exactly 3 percent in 18 min.
			10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := measuredRate(tc.runs, false)
			if (got > 0) != (tc.wantPct > 0) {
				t.Fatalf("rate = %v, want %v", got, tc.wantPct)
			}
			if tc.wantPct > 0 && (got < tc.wantPct-0.01 || got > tc.wantPct+0.01) {
				t.Errorf("rate = %v %%/h, want %v", got, tc.wantPct)
			}
		})
	}
}

func TestMeasuredRateIgnoresWrongDirection(t *testing.T) {
	// A gauge that climbed while we believe we are discharging is not a
	// discharge rate; the caller must fall back rather than divide by it.
	rising := runsAt([2]int{0, 80}, [2]int{6, 81}, [2]int{12, 82}, [2]int{18, 83}, [2]int{24, 84})
	if got := measuredRate(rising, false); got != 0 {
		t.Errorf("rate = %v, want 0 for a rising gauge while discharging", got)
	}
}

func TestEstimateFallsBackToDatasheet(t *testing.T) {
	m7010 := findDeviceByID("m7010")
	est := estimateBattery(m7010, 50, false, &batteryHistory{Runs: runsAt([2]int{0, 50})})

	if est.Source != "typical" {
		t.Fatalf("source = %q, want typical during warm-up", est.Source)
	}
	// 8 h datasheet runtime at 50% = 4 h.
	if est.Minutes != 240 {
		t.Errorf("minutes = %d, want 240", est.Minutes)
	}
	if !strings.Contains(est.Label(), "(typical)") {
		t.Errorf("label must flag the guess, got %q", est.Label())
	}
}

func TestEstimateUsesMeasuredRateOnceWarm(t *testing.T) {
	m7010 := findDeviceByID("m7010")
	// 3 percent in 18 min = 10 %/h, i.e. far faster than the 12.5 %/h
	// datasheet rate — the point being that the measurement wins.
	runs := runsAt([2]int{0, 90}, [2]int{6, 89}, [2]int{12, 88}, [2]int{18, 87}, [2]int{24, 86})
	est := estimateBattery(m7010, 86, false, &batteryHistory{Runs: runs})

	if est.Source != "measured" {
		t.Fatalf("source = %q, want measured", est.Source)
	}
	if est.Minutes != 516 { // 86% / 10 %/h = 8.6 h
		t.Errorf("minutes = %d, want 516", est.Minutes)
	}
	if want := "~8h36m left"; est.Label() != want {
		t.Errorf("label = %q, want %q", est.Label(), want)
	}
}

func TestEstimateChargingHasNoDatasheetFallback(t *testing.T) {
	// Neither vendor publishes a charge time, so time-to-full must stay
	// silent until it has been measured.
	m7010 := findDeviceByID("m7010")
	cold := estimateBattery(m7010, 50, true, &batteryHistory{})
	if cold.known() {
		t.Errorf("charging estimate = %+v, want unknown before measurement", cold)
	}

	var runs []batteryRun
	for i, pct := range []int{50, 51, 52, 53, 54} {
		t := at(i * 6).Unix()
		runs = append(runs, batteryRun{Pct: pct, Chg: true, From: t, To: t})
	}
	warm := estimateBattery(m7010, 54, true, &batteryHistory{Runs: runs})
	if !warm.ToFull || warm.Source != "measured" {
		t.Fatalf("charging estimate = %+v, want a measured to-full figure", warm)
	}
	if !strings.Contains(warm.Label(), "to full") {
		t.Errorf("label = %q, want a to-full phrasing", warm.Label())
	}
}

func TestEstimateSilentWhenFullOrEmpty(t *testing.T) {
	m7010 := findDeviceByID("m7010")
	if est := estimateBattery(m7010, 100, true, &batteryHistory{}); est.known() {
		t.Errorf("charging at 100%% must not count down, got %+v", est)
	}
	if est := estimateBattery(m7010, 0, false, &batteryHistory{}); est.known() {
		t.Errorf("a 0/absent percent must not estimate, got %+v", est)
	}
}

func TestFormatMinutes(t *testing.T) {
	tests := []struct {
		min  int
		want string
	}{
		{47, "47m"},
		{60, "1h00m"},
		{252, "4h12m"},
		{516, "8h36m"},
	}
	for _, tc := range tests {
		if got := formatMinutes(tc.min); got != tc.want {
			t.Errorf("formatMinutes(%d) = %q, want %q", tc.min, got, tc.want)
		}
	}
}

func TestBatteryStateRoundTripPerDevice(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TPLINK_STATE_DIR", dir)

	st := loadBatteryState()
	st.Devices["m7010"] = &batteryHistory{Runs: runsAt([2]int{0, 88})}
	st.Devices["mudi"] = &batteryHistory{Runs: runsAt([2]int{0, 42})}
	saveBatteryState(st)

	got := loadBatteryState()
	if len(got.Devices) != 2 {
		t.Fatalf("want both devices kept, got %+v", got.Devices)
	}
	if got.Devices["m7010"].Runs[0].Pct != 88 || got.Devices["mudi"].Runs[0].Pct != 42 {
		t.Errorf("histories crossed over: %+v", got.Devices)
	}

	// One binary serves both routers, so switching networks must not make
	// the other device's history disappear.
	if _, err := os.Stat(filepath.Join(dir, "tplink-m7010", "battery.json")); err != nil {
		t.Errorf("state file not at the documented path: %v", err)
	}
}

func TestLoadBatteryStateSurvivesCorruption(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TPLINK_STATE_DIR", dir)
	path := filepath.Join(dir, "tplink-m7010", "battery.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"v":1,"devices":{"m7010":{"runs":[{"pct":`), 0o644); err != nil {
		t.Fatal(err)
	}

	st := loadBatteryState()
	if st == nil || st.Devices == nil {
		t.Fatal("a truncated file must yield an empty usable state, not nil")
	}
	if len(st.Devices) != 0 {
		t.Errorf("want a fresh state, got %+v", st.Devices)
	}
}

func TestBatteryRemainingWritesState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TPLINK_STATE_DIR", dir)

	d := findDeviceByID("mudi")
	est := batteryRemaining(d, &Status{BatteryPercent: 50})
	if est.Source != "typical" {
		t.Errorf("first observation should fall back to the datasheet, got %+v", est)
	}

	data, err := os.ReadFile(filepath.Join(dir, "tplink-m7010", "battery.json"))
	if err != nil {
		t.Fatalf("state not written: %v", err)
	}
	var st batteryState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	if h := st.Devices["mudi"]; h == nil || len(h.Runs) != 1 || h.Runs[0].Pct != 50 {
		t.Errorf("observation not recorded: %+v", st.Devices)
	}
}

func TestTooltipCarriesEstimateAndBarTextDoesNot(t *testing.T) {
	// The whole point of the feature request: the time shows in the
	// popup, never on the bar tile.
	s := &Status{NetworkType: "4G", BatteryPercent: 86, SignalStrength: 4}
	est := batteryEstimate{Minutes: 516, Text: "8h36m", Source: "measured"}
	text, tooltip, _ := formatStatusLine(findDeviceByID("m7010"), s, est)

	if !strings.Contains(tooltip, "Battery: 86% · ~8h36m left") {
		t.Errorf("tooltip missing the estimate:\n%s", tooltip)
	}
	if strings.Contains(text, "8h36m") || strings.Contains(text, "left") {
		t.Errorf("bar text must stay glanceable, got %q", text)
	}
}

func TestTooltipOmitsUnknownEstimate(t *testing.T) {
	s := &Status{NetworkType: "4G", BatteryPercent: 86}
	_, tooltip, _ := formatStatusLine(findDeviceByID("m7010"), s, batteryEstimate{})

	if !strings.Contains(tooltip, "Battery: 86%\n") {
		t.Errorf("battery line malformed without an estimate:\n%s", tooltip)
	}
	if strings.Contains(tooltip, "·") {
		t.Errorf("no separator should be left dangling:\n%s", tooltip)
	}
}

// --- cross-session learning ---

// observeSeries feeds (minute, percent) readings through the full observe
// path, i.e. exactly what a series of polls would do.
func observeSeries(h *batteryHistory, chg bool, pairs ...[2]int) {
	for _, p := range pairs {
		h.observe(p[1], chg, at(p[0]))
	}
}

func TestObserveBanksEdgeIntervals(t *testing.T) {
	h := &batteryHistory{}
	observeSeries(h, false,
		[2]int{0, 90}, [2]int{6, 89}, [2]int{12, 88}, [2]int{18, 87}, [2]int{24, 86})

	if h.Discharge == nil {
		t.Fatal("nothing banked")
	}
	// Four edges (89..86) yield three banked intervals of 1% / 6 min. The
	// first percent is skipped: we joined it partway through.
	if h.Discharge.Pct != 3 || h.Discharge.Obs != 3 {
		t.Errorf("banked = %+v, want 3 percent over 3 observations", h.Discharge)
	}
	// The pooled rate must agree with the in-session edge-anchored rate,
	// or the two halves of this file disagree about the same window.
	if got, want := h.Discharge.rate(), measuredRate(h.Runs, false); got < want-0.001 || got > want+0.001 {
		t.Errorf("pooled rate %v != in-session rate %v", got, want)
	}
	if h.Charge != nil {
		t.Errorf("discharge leaked into the charging pool: %+v", h.Charge)
	}
}

func TestObserveDoesNotDoubleCountHeldPercent(t *testing.T) {
	h := &batteryHistory{}
	observeSeries(h, false, [2]int{0, 90}, [2]int{6, 89}, [2]int{12, 88})
	banked := *h.Discharge

	// Same percent, polled repeatedly — no new evidence.
	observeSeries(h, false, [2]int{13, 88}, [2]int{14, 88}, [2]int{15, 88})
	if *h.Discharge != banked {
		t.Errorf("pool moved on a held percent: %+v -> %+v", banked, *h.Discharge)
	}
}

func TestLearningSurvivesWindowResets(t *testing.T) {
	// The point of the whole mechanism: a charger flip or a sleep gap
	// destroys the measurable window but must not destroy the knowledge.
	for _, tc := range []struct {
		name string
		next func(h *batteryHistory)
	}{
		{"sleep gap", func(h *batteryHistory) { h.observe(86, false, at(120)) }},
		{"charger plugged in", func(h *batteryHistory) { h.observe(86, true, at(30)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &batteryHistory{}
			observeSeries(h, false,
				[2]int{0, 90}, [2]int{6, 89}, [2]int{12, 88}, [2]int{18, 87}, [2]int{24, 86})
			before := *h.Discharge

			tc.next(h)

			if len(h.Runs) != 1 {
				t.Errorf("window should have restarted, got %d runs", len(h.Runs))
			}
			if h.Banked != nil {
				t.Error("the interval spanning the discontinuity must not stay pending")
			}
			if *h.Discharge != before {
				t.Errorf("learning lost across the reset: %+v -> %+v", before, *h.Discharge)
			}
		})
	}
}

func TestBankIgnoresWrongDirection(t *testing.T) {
	h := &batteryHistory{}
	// A gauge wobbling upwards while discharging is not discharge evidence.
	observeSeries(h, false, [2]int{0, 88}, [2]int{6, 89}, [2]int{12, 90})
	if h.Discharge != nil {
		t.Errorf("wrong-direction movement was banked: %+v", h.Discharge)
	}
}

func TestLearnedRateOutranksDatasheet(t *testing.T) {
	m7010 := findDeviceByID("m7010")
	// This unit has actually averaged 20 %/h — a 5 h runtime against the
	// datasheet's 8 h, which is what an aged cell or a heavier usage
	// pattern looks like.
	h := &batteryHistory{Discharge: &batteryLearned{Pct: 20, Hours: 1, Obs: 20}}
	est := estimateBattery(m7010, 50, false, h)

	if est.Source != "learned" {
		t.Fatalf("source = %q, want learned", est.Source)
	}
	// The datasheet still counts as a weak prior, so the rate lands
	// between the two: (20+10) / (1 + 10*8/100) = 16.67 %/h.
	if est.Minutes != 180 {
		t.Errorf("minutes = %d, want 180 (50%% at 16.67 %%/h)", est.Minutes)
	}
	if !strings.Contains(est.Label(), "(avg)") {
		t.Errorf("label must mark a cross-session average, got %q", est.Label())
	}
}

func TestThinEvidenceStillDefersToDatasheet(t *testing.T) {
	m7010 := findDeviceByID("m7010")
	// One brief window is not a rate — a couple of percent seen during a
	// quiet moment would otherwise claim a 40 h battery.
	h := &batteryHistory{Discharge: &batteryLearned{Pct: 2, Hours: 1, Obs: 2}}
	if est := estimateBattery(m7010, 50, false, h); est.Source != "typical" {
		t.Errorf("source = %q, want typical below the evidence threshold", est.Source)
	}
}

func TestMeasuredRateOutranksLearned(t *testing.T) {
	m7010 := findDeviceByID("m7010")
	h := &batteryHistory{
		Runs: runsAt([2]int{0, 90}, [2]int{6, 89}, [2]int{12, 88}, [2]int{18, 87}, [2]int{24, 86}),
		// Wildly different history: what is happening now must win.
		Discharge: &batteryLearned{Pct: 100, Hours: 1, Obs: 100},
	}
	if est := estimateBattery(m7010, 86, false, h); est.Source != "measured" {
		t.Errorf("source = %q, want measured — the live window beats history", est.Source)
	}
}

func TestChargingLearnsWithoutADatasheetPrior(t *testing.T) {
	m7010 := findDeviceByID("m7010")
	h := &batteryHistory{Charge: &batteryLearned{Pct: 40, Hours: 1, Obs: 40}}
	est := estimateBattery(m7010, 50, true, h)

	if est.Source != "learned" || !est.ToFull {
		t.Fatalf("estimate = %+v, want a learned to-full figure", est)
	}
	// No prior to blend, so the pooled 40 %/h is used as-is: 50% to go.
	if est.Minutes != 75 {
		t.Errorf("minutes = %d, want 75", est.Minutes)
	}
}

func TestLearnedMemoryIsBounded(t *testing.T) {
	h := &batteryHistory{}
	// Ten full discharges' worth of evidence at a steady 10 %/h.
	for i := 0; i < 1000; i++ {
		h.bank(false, batteryEdge{Pct: 100, T: at(i * 6).Unix()},
			batteryRun{Pct: 99, From: at(i*6 + 6).Unix()}, at(i*6+6))
	}
	if h.Discharge.Pct > batteryLearnMaxPct+1 {
		t.Errorf("pool grew unbounded: %+v", h.Discharge)
	}
	if got := h.Discharge.rate(); got < 9.99 || got > 10.01 {
		t.Errorf("scaling the pool changed the rate: %v, want 10", got)
	}
}

func TestLearningRoundTripsThroughState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TPLINK_STATE_DIR", dir)
	d := findDeviceByID("m7010")

	// Session one: one reading every 6 min, as a real poll would deliver.
	batteryNow = func() time.Time { return at(0) }
	defer func() { batteryNow = time.Now }()
	for i, pct := range []int{90, 89, 88, 87, 86, 85, 84, 83} {
		min := i * 6
		batteryNow = func() time.Time { return at(min) }
		batteryRemaining(d, &Status{BatteryPercent: pct})
	}
	// Session two, in a brand new process: the file is all it has.
	st := loadBatteryState()
	if h := st.Devices["m7010"]; h == nil || h.Discharge == nil || h.Discharge.Obs == 0 {
		t.Fatalf("learning did not reach disk: %+v", st.Devices)
	}
}

func TestSecondSessionStartsFromWhatTheFirstLearned(t *testing.T) {
	// The whole feature, end to end: a first session that actually
	// consumed some battery must leave the next cold start better off
	// than the vendor's datasheet number.
	dir := t.TempDir()
	t.Setenv("TPLINK_STATE_DIR", dir)
	d := findDeviceByID("m7010")
	defer func() { batteryNow = time.Now }()

	poll := func(min, pct int) batteryEstimate {
		batteryNow = func() time.Time { return at(min) }
		return batteryRemaining(d, &Status{BatteryPercent: pct})
	}

	// Session one: 90% -> 80% in 40 min, i.e. 15 %/h — this unit under
	// this usage is well short of the 12.5 %/h datasheet rate.
	if est := poll(0, 90); est.Source != "typical" {
		t.Fatalf("first ever reading: source = %q, want typical", est.Source)
	}
	for i, pct := range []int{89, 88, 87, 86, 85, 84, 83, 82, 81, 80} {
		poll((i+1)*4, pct)
	}

	// Two hours later — laptop slept, window unmeasurable, knowledge kept.
	est := poll(160, 80)
	if est.Source != "learned" {
		t.Fatalf("second session: source = %q, want learned", est.Source)
	}
	// Pool holds 9 percent over 0.6 h; with the datasheet prior that is
	// (9+10)/(0.6+0.8) = 13.57 %/h, so 80% has ~5h54m to go — between the
	// datasheet's optimistic 6h24m and the raw measured 5h20m.
	if est.Minutes != 354 {
		t.Errorf("minutes = %d, want 354", est.Minutes)
	}
	if want := "~5h54m left (avg)"; est.Label() != want {
		t.Errorf("label = %q, want %q", est.Label(), want)
	}
}
