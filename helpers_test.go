package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRsrpToSignal(t *testing.T) {
	cases := []struct {
		rsrp, want int
	}{
		{-70, 5}, {-80, 5},
		{-81, 4}, {-90, 4},
		{-91, 3}, {-100, 3},
		{-101, 2}, {-110, 2},
		{-111, 1}, {-120, 1},
		{-121, 0}, {-140, 0},
	}
	for _, c := range cases {
		if got := rsrpToSignal(c.rsrp); got != c.want {
			t.Errorf("rsrpToSignal(%d) = %d, want %d", c.rsrp, got, c.want)
		}
	}
}

func TestNetworkTypeStr(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "No Service"}, {1, "2G"}, {2, "3G"}, {3, "4G"}, {4, "4G+"},
		{7, "Unknown(7)"},
	}
	for _, c := range cases {
		if got := networkTypeStr(c.in); got != c.want {
			t.Errorf("networkTypeStr(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFriendlyNetworkType(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"NR5G-SA", "5G+"},
		{"NR5G-NSA", "5G"},
		{"NR5G", "5G"},
		{"LTE", "4G"},
		{"LTE FDD", "4G"},     // duplex decoration stripped
		{"LTE-TDD", "4G"},     // dash variant
		{"LTE-FDD-CA", "4G+"}, // duplex inside a CA mode
		{"LTE-CA", "4G+"},
		{"lte", "4G"}, // case-insensitive
		{"HSPA+", "3G+"},
		{"WCDMA", "3G"},
		{"EDGE", "2G+"},
		{"GSM", "2G"},
		{"WEIRDMODE", "WEIRDMODE"}, // unknown falls through raw
	}
	for _, c := range cases {
		if got := friendlyNetworkType(c.in); got != c.want {
			t.Errorf("friendlyNetworkType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatUptime(t *testing.T) {
	cases := []struct {
		sec  float64
		want string
	}{
		{45, "45s"},
		{720, "12m"},
		{3*3600 + 27*60, "3h 27m"},
		{2*86400 + 4*3600, "2d 4h"},
	}
	for _, c := range cases {
		if got := formatUptime(c.sec); got != c.want {
			t.Errorf("formatUptime(%v) = %q, want %q", c.sec, got, c.want)
		}
	}
}

func TestPickActiveSlot(t *testing.T) {
	// Slot 2 dialled, slot 1 not: the dialled one wins regardless of order.
	c := cellularSnapshot{
		networksStatus: []map[string]any{
			{"slot": "1", "dial_status": float64(1)},
			{"slot": "2", "dial_status": float64(0)},
		},
	}
	if got := pickActiveSlot(c); got != "2" {
		t.Errorf("dialled slot: got %q, want 2", got)
	}

	// No dial succeeded: fall back to the first slot with data.
	c.networksStatus[1]["dial_status"] = float64(1)
	if got := pickActiveSlot(c); got != "1" {
		t.Errorf("fallback slot: got %q, want 1", got)
	}

	// No network data at all: fall back to sims_status.
	c = cellularSnapshot{
		simsStatus: []map[string]any{{"slot": "1"}},
	}
	if got := pickActiveSlot(c); got != "1" {
		t.Errorf("sims fallback: got %q, want 1", got)
	}

	if got := pickActiveSlot(cellularSnapshot{}); got != "" {
		t.Errorf("empty snapshot: got %q, want empty", got)
	}
}

func TestPkcs7RoundTrip(t *testing.T) {
	for _, msg := range []string{"", "a", "exactly-16-bytes", "something longer than one block"} {
		padded := pkcs7Pad([]byte(msg), 16)
		if len(padded)%16 != 0 {
			t.Errorf("pkcs7Pad(%q) length %d not block-aligned", msg, len(padded))
		}
		if got := pkcs7Unpad(padded, 16); string(got) != msg {
			t.Errorf("round trip %q → %q", msg, got)
		}
	}
}

func TestIntFrom(t *testing.T) {
	if got := intFrom(float64(42)); got != 42 {
		t.Errorf("float64: got %d", got)
	}
	if got := intFrom("17"); got != 17 {
		t.Errorf("string: got %d", got)
	}
	if got := intFrom(nil); got != 0 {
		t.Errorf("nil: got %d", got)
	}
}

func TestJsonFloatStr(t *testing.T) {
	m := map[string]any{
		"str":  "14473800628.000000",
		"neg":  "-79",
		"num":  float64(12.5),
		"junk": true,
	}
	if got := jsonFloatStr(m, "str"); got != 14473800628 {
		t.Errorf("decimal string: got %v", got)
	}
	if got := jsonFloatStr(m, "neg"); got != -79 {
		t.Errorf("negative string: got %v", got)
	}
	if got := jsonFloatStr(m, "num"); got != 12.5 {
		t.Errorf("number: got %v", got)
	}
	if got := jsonFloatStr(m, "junk"); got != 0 {
		t.Errorf("junk: got %v", got)
	}
	if got := jsonFloatStr(m, "absent"); got != 0 {
		t.Errorf("absent: got %v", got)
	}
}

func TestSparkline(t *testing.T) {
	// Fixed scale: -125 → lowest level, -75 (and better) → highest.
	got := sparkline([]int{-125, -100, -75})
	if got != "▁▄█" {
		t.Errorf("sparkline = %q, want ▁▄█", got)
	}
	// Out-of-range values clamp instead of indexing out of bounds.
	got = sparkline([]int{-140, -60})
	if got != "▁█" {
		t.Errorf("clamped sparkline = %q, want ▁█", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{512, "512 B"},
		{42.7 * (1 << 20), "42.7 MB"},
		{2964, "2.9 KB"},
		{13.48 * (1 << 30), "13.48 GB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestComputeRate(t *testing.T) {
	t0 := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Second)

	if got := computeRate(1000, 21000, t0, t1); got != 2000 {
		t.Errorf("rate = %v, want 2000 B/s", got)
	}
	// No previous sample yet.
	if got := computeRate(0, 21000, time.Time{}, t1); got != 0 {
		t.Errorf("no prev sample: rate = %v, want 0", got)
	}
	// Counter reset (reboot / statistics cleared).
	if got := computeRate(21000, 1000, t0, t1); got != 0 {
		t.Errorf("counter reset: rate = %v, want 0", got)
	}
	// Same timestamp must not divide by zero.
	if got := computeRate(1000, 2000, t0, t0); got != 0 {
		t.Errorf("zero interval: rate = %v, want 0", got)
	}
}

func TestGaugeBarClamps(t *testing.T) {
	if got := gaugeBar(150); got != "[██████████]" {
		t.Errorf("gaugeBar(150) = %q, want full bar", got)
	}
	if got := gaugeBar(0); got != "[░░░░░░░░░░]" {
		t.Errorf("gaugeBar(0) = %q, want empty bar", got)
	}
}

func TestParseDefaultGateway(t *testing.T) {
	// 0108A8C0 is 192.168.8.1 little-endian.
	route := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"wlan0\t00000000\t0108A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"wlan0\t0008A8C0\t00000000\t0001\t0\t0\t600\t00FFFFFF\t0\t0\t0\n"
	if got := parseDefaultGateway(strings.NewReader(route)); got != "192.168.8.1" {
		t.Errorf("got %q, want 192.168.8.1", got)
	}

	noDefault := "Iface\tDestination\tGateway\tFlags\n" +
		"eth0\t0008A8C0\t00000000\t0001\n"
	if got := parseDefaultGateway(strings.NewReader(noDefault)); got != "" {
		t.Errorf("no default route: got %q, want empty", got)
	}

	if got := parseDefaultGateway(bytes.NewReader(nil)); got != "" {
		t.Errorf("empty input: got %q, want empty", got)
	}
}
