package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// View is pure, so the dashboard states can be rendered offline.
func TestViewRendersDashboard(t *testing.T) {
	m := model{
		device:  &supportedDevices[0],
		refresh: 10 * time.Second,
		status: &Status{
			NetworkType:     "4G",
			SignalStrength:  4,
			RSRP:            -85,
			BatteryPercent:  88,
			TotalBytes:      32 * (1 << 30),
			MonthLimitBytes: 100 * (1 << 30),
			DailyBytes:      42 * (1 << 20),
			RxSpeed:         "2964",
			TxSpeed:         "2202",
			Operator:        "Movistar",
		},
		rsrpHist:   []int{-90, -85, -83},
		lastUpdate: time.Now(),
	}
	out := m.View()
	for _, want := range []string{
		"History", // sparkline row appears once ≥2 samples exist
		"▅▆▆",     // the sparkline for -90,-85,-83 on the fixed scale
		"Today",   // daily usage row
		"Speed",   // split speeds from the M7010 fields
		"↓ 2.9 KB/s", "↑ 2.2 KB/s",
		"32.00 / 100 GB (32%)",
		"[███░░░░░░░]", // data gauge
	} {
		if !strings.Contains(out, want) {
			t.Errorf("View() missing %q", want)
		}
	}
}

func TestViewDerivedRateWhenNoSplitSpeeds(t *testing.T) {
	m := model{
		device:      &supportedDevices[1],
		refresh:     10 * time.Second,
		status:      &Status{NetworkType: "5G", TotalBytes: 1 << 30},
		derivedRate: 1.5 * (1 << 20),
		lastUpdate:  time.Now(),
	}
	out := m.View()
	if !strings.Contains(out, "≈ 1.5 MB/s") {
		t.Errorf("View() missing derived rate; got:\n%s", out)
	}
}

func TestViewStaleDataOnError(t *testing.T) {
	m := model{
		device:     &supportedDevices[1],
		refresh:    10 * time.Second,
		status:     &Status{NetworkType: "5G", BatteryPercent: 50},
		err:        errors.New("connection refused"),
		lastUpdate: time.Now().Add(-40 * time.Second),
	}
	out := m.View()
	// The dashboard stays visible with the error in the footer.
	if !strings.Contains(out, "5G") {
		t.Error("stale dashboard hidden on error; want last-known data visible")
	}
	if !strings.Contains(out, "connection refused") || !strings.Contains(out, "showing data from") {
		t.Errorf("View() missing stale-error footer; got:\n%s", out)
	}

	// Before any successful fetch, errors still get the full-screen form.
	m.status = nil
	out = m.View()
	if !strings.Contains(out, "Error: connection refused") || !strings.Contains(out, "r = retry") {
		t.Errorf("View() missing full-screen error; got:\n%s", out)
	}
}
