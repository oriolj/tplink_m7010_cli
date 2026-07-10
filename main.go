// Command tplink-m7010 reads connection, signal, battery and data-usage state
// from a mobile Wi-Fi hotspot and renders it as one of:
//
//   - default:    interactive Bubble Tea TUI dashboard
//   - --waybar:   single JSON line for use as a waybar custom module
//   - --noctalia: single JSON line for the noctalia-shell CustomButton widget
//   - --json:     parsed status as machine-readable JSON, for scripts
//   - --raw:      raw API responses, for debugging / exploration
//
// Two device families are supported in the same binary:
//
//   - TP-Link M7010 (AES+RSA envelope; see PROTOCOL.md, client.go)
//   - GL.iNet Mudi GL-E5800 (OpenWrt JSON-RPC; see PROTOCOL_GLINET.md, mudi.go)
//
// By default the binary autodetects which router is on the LAN: it first
// checks whether the kernel's default gateway matches a known device
// address, then falls back to probing both addresses in parallel. Both
// signals are confirmed with a cheap unauthenticated protocol probe (see
// detectDevice in device.go). If nothing answers, widget modes emit empty
// output and exit so the laptop battery isn't burned on pointless
// retries. Use `--device m7010|mudi` to skip autodetect, or `--addr` to
// override the address for the selected device.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	flagDevice   = flag.String("device", "", "router model (autodetect by default)")
	flagAddr     = flag.String("addr", "", "router IP address (overrides per-device default)")
	flagPass     = flag.String("pass", "", "admin password (overrides env var and password file)")
	flagWaybar   = flag.Bool("waybar", false, "output waybar JSON and exit")
	flagNoctalia = flag.Bool("noctalia", false, "output noctalia-shell CustomButton JSON and exit")
	flagJSON     = flag.Bool("json", false, "output the parsed status as JSON and exit (for scripts)")
	flagRaw      = flag.Bool("raw", false, "dump raw API responses and exit")
	flagDebug    = flag.Bool("debug", false, "print debug HTTP traffic")
	flagPoweroff = flag.Bool("poweroff", false, "power the router off and exit")
	flagReboot   = flag.Bool("reboot", false, "reboot the router and exit")
	flagRefresh  = flag.Duration("refresh", 10*time.Second, "TUI refresh interval")
)

const detectTimeout = 500 * time.Millisecond

type powerAction string

const (
	powerShutdown powerAction = "shutdown"
	powerReboot   powerAction = "reboot"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s (devices: %s):\n",
			os.Args[0], supportedIDs())
		flag.PrintDefaults()
	}
	flag.Parse()

	switch {
	case *flagPoweroff:
		runPower(powerShutdown)
	case *flagReboot:
		runPower(powerReboot)
	case *flagWaybar:
		runWaybar()
	case *flagNoctalia:
		runNoctalia()
	case *flagJSON:
		runJSON()
	case *flagRaw:
		runRaw()
	default:
		runTUI()
	}
}

// pickDevice honours --device if set, otherwise autodetects. Returns nil
// when nothing is reachable; widget callers turn that into empty JSON.
func pickDevice() *SupportedDevice {
	if *flagDevice != "" {
		d := findDeviceByID(*flagDevice)
		if d == nil {
			fmt.Fprintf(os.Stderr, "unknown --device %q (supported: %s)\n",
				*flagDevice, supportedIDs())
			os.Exit(2)
		}
		return d
	}
	return detectDevice(detectTimeout)
}

func runPower(action powerAction) {
	d := pickDevice()
	if d == nil {
		fmt.Fprintln(os.Stderr, "no supported router reachable")
		os.Exit(1)
	}
	dev, err := openDevice(d, *flagAddr, *flagPass, *flagDebug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login (%s): %v\n", d.Title, err)
		os.Exit(1)
	}
	defer dev.Close()

	switch action {
	case powerShutdown:
		err = dev.Shutdown()
	case powerReboot:
		err = dev.Reboot()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
		os.Exit(1)
	}
	fmt.Printf("%s command sent to %s\n", action, d.Title)
}

// --- Waybar mode ---

type WaybarOutput struct {
	Text    string `json:"text"`
	Tooltip string `json:"tooltip"`
	Class   string `json:"class"`
	Alt     string `json:"alt"`
}

func runWaybar() {
	enc := json.NewEncoder(os.Stdout)
	d := pickDevice()
	if d == nil {
		enc.Encode(WaybarOutput{})
		return
	}
	status, err := fetchOnce(d)
	if err != nil {
		// Render an error tooltip and exit 0 so waybar shows it instead
		// of treating the module as failed.
		enc.Encode(WaybarOutput{
			Text:    " --",
			Tooltip: "Error: " + err.Error(),
			Class:   "disconnected",
		})
		return
	}
	text, tooltip, class := formatStatusLine(d, status)
	enc.Encode(WaybarOutput{
		Text:    text,
		Tooltip: tooltip,
		Class:   class,
		Alt:     status.NetworkType,
	})
}

// --- Noctalia mode ---

type noctaliaOutput struct {
	Text      string `json:"text"`
	Tooltip   string `json:"tooltip"`
	TextColor string `json:"textColor,omitempty"`
}

func classToNoctaliaColor(class string) string {
	switch class {
	case "warning":
		return "secondary"
	case "critical":
		return "error"
	default:
		return "none"
	}
}

func runNoctalia() {
	enc := json.NewEncoder(os.Stdout)
	d := pickDevice()
	if d == nil {
		enc.Encode(noctaliaOutput{})
		return
	}
	status, err := fetchOnce(d)
	if err != nil {
		// A fetch error after a successful protocol probe means a real
		// device with a real problem (wrong password, firmware change) —
		// show it instead of silently collapsing like the no-device case.
		enc.Encode(noctaliaOutput{
			Text:      "--",
			Tooltip:   "Error: " + err.Error(),
			TextColor: "error",
		})
		return
	}
	text, tooltip, class := formatStatusLine(d, status)
	enc.Encode(noctaliaOutput{
		Text:      text,
		Tooltip:   tooltip, // formatStatusLine already trims the trailing newline
		TextColor: classToNoctaliaColor(class),
	})
}

// formatStatusLine builds the shared widget text + tooltip + class string.
// Both waybar and noctalia drive their styling off the same data.
func formatStatusLine(d *SupportedDevice, s *Status) (text, tooltip, class string) {
	signal := signalBars(s.SignalStrength)
	dataGB := bytesToGB(s.TotalBytes)
	limitGB := bytesToGB(s.MonthLimitBytes)

	netLabel := s.NetworkType
	if netLabel == "" {
		netLabel = "—"
	}
	text = fmt.Sprintf("%s %s  %d%%  %.1fGB",
		netLabel, signal, s.BatteryPercent, dataGB)

	var tb strings.Builder
	fmt.Fprintf(&tb, "%s\n", d.Title)
	fmt.Fprintf(&tb, "Connection: %s", netLabel)
	if s.NetworkTypeRaw != "" && s.NetworkTypeRaw != netLabel {
		fmt.Fprintf(&tb, " (%s)", s.NetworkTypeRaw)
	}
	if s.Operator != "" {
		fmt.Fprintf(&tb, " · %s", s.Operator)
	}
	if s.Band > 0 {
		fmt.Fprintf(&tb, " B%d", s.Band)
	}
	if s.DLBandwidth != "" {
		fmt.Fprintf(&tb, " @ %s", s.DLBandwidth)
	}
	fmt.Fprintf(&tb, "\n")
	fmt.Fprintf(&tb, "Signal: %d/5 %s", s.SignalStrength, signal)
	if s.RSRP != 0 {
		fmt.Fprintf(&tb, "  RSRP: %d dBm", s.RSRP)
	}
	if s.RSRQ != 0 {
		fmt.Fprintf(&tb, "  RSRQ: %d dB", s.RSRQ)
	}
	if s.SNR != 0 {
		fmt.Fprintf(&tb, "  SINR: %d dB", s.SNR)
	}
	fmt.Fprintf(&tb, "\n")
	if s.BatteryCharging {
		fmt.Fprintf(&tb, "Battery: %d%% (charging)\n", s.BatteryPercent)
	} else {
		fmt.Fprintf(&tb, "Battery: %d%%\n", s.BatteryPercent)
	}
	fmt.Fprintf(&tb, "Data Used: %.2f GB", dataGB)
	if limitGB > 0 {
		fmt.Fprintf(&tb, " / %.0f GB", limitGB)
	}
	fmt.Fprintf(&tb, "\n")
	// The M7010 reports live speeds; the Mudi doesn't (deriving them
	// needs cross-tick state, which one-shot widget modes don't keep).
	if rx, tx := parseSpeed(s.RxSpeed), parseSpeed(s.TxSpeed); rx > 0 || tx > 0 {
		fmt.Fprintf(&tb, "Speed: ↓ %s  ↑ %s\n", humanRate(rx), humanRate(tx))
	}
	if s.WanIP != "" {
		fmt.Fprintf(&tb, "WAN IP: %s\n", s.WanIP)
	}
	if s.ConnectedDevices > 0 {
		fmt.Fprintf(&tb, "Devices: %d\n", s.ConnectedDevices)
	}
	if s.UptimeSec > 0 {
		fmt.Fprintf(&tb, "Uptime: %s\n", formatUptime(s.UptimeSec))
	}
	if temps := formatTemps(s); temps != "" {
		fmt.Fprintf(&tb, "%s\n", temps)
	}
	if s.LoadAvg[0] > 0 || s.LoadAvg[1] > 0 || s.LoadAvg[2] > 0 {
		fmt.Fprintf(&tb, "Load: %.2f %.2f %.2f", s.LoadAvg[0], s.LoadAvg[1], s.LoadAvg[2])
	}
	tooltip = strings.TrimRight(tb.String(), "\n")

	class = "good"
	if s.BatteryPercent < 20 {
		class = "critical"
	} else if s.BatteryPercent < 40 {
		class = "warning"
	}
	if s.NetworkType == "" || s.NetworkType == "No Service" {
		class = "disconnected"
	}
	return text, tooltip, class
}

// --- JSON mode ---

// runJSON emits the parsed Status as machine-readable JSON. This is the
// stable scripting interface: unlike --waybar / --noctalia it isn't tied
// to any bar's widget schema, so a future noctalia plugin, eww, polybar,
// or a shell script can consume it without us chasing widget formats.
func runJSON() {
	d := pickDevice()
	if d == nil {
		fmt.Fprintln(os.Stderr, "no supported router reachable")
		os.Exit(1)
	}
	status, err := fetchOnce(d)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(struct {
		Device string  `json:"device"`
		Title  string  `json:"title"`
		Status *Status `json:"status"`
	}{d.ID, d.Title, status})
}

// --- Raw dump mode ---

func runRaw() {
	d := pickDevice()
	if d == nil {
		fmt.Fprintln(os.Stderr, "no supported router reachable")
		os.Exit(1)
	}
	status, err := fetchOnce(d)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	fmt.Printf("=== %s — Status ===\n", d.Title)
	enc.Encode(status.RawStatus)
	if status.RawFlowStat != nil {
		fmt.Println("\n=== Flow / Modem ===")
		enc.Encode(status.RawFlowStat)
	}
}

// fetchOnce opens a session and fetches status. We deliberately skip
// Close: the router ages sessions out on its own, and logging out costs
// a full extra round-trip on every waybar/noctalia tick.
func fetchOnce(d *SupportedDevice) (*Status, error) {
	dev, err := openDevice(d, *flagAddr, *flagPass, *flagDebug)
	if err != nil {
		return nil, err
	}
	return dev.Fetch()
}

// --- TUI mode ---
//
// The TUI keeps a single logged-in device across ticks. On error the
// device is closed and the next tick reconnects.

var (
	tuiDevice   Device
	tuiDeviceMu sync.Mutex
)

func tuiFetch(d *SupportedDevice) (*Status, error) {
	tuiDeviceMu.Lock()
	defer tuiDeviceMu.Unlock()

	if tuiDevice == nil {
		dev, err := openDevice(d, *flagAddr, *flagPass, *flagDebug)
		if err != nil {
			return nil, err
		}
		tuiDevice = dev
	}
	status, err := tuiDevice.Fetch()
	if err != nil {
		tuiDevice.Close()
		tuiDevice = nil
		return nil, err
	}
	return status, nil
}

type model struct {
	device  *SupportedDevice
	status  *Status
	err     error
	loading bool
	refresh time.Duration

	lastUpdate time.Time // when status last refreshed successfully
	rsrpHist   []int     // recent RSRP samples for the sparkline

	// Cross-tick state for deriving throughput on devices that don't
	// report live speeds (the Mudi only exposes a total-bytes counter).
	prevTotalBytes float64
	prevSampleTime time.Time
	derivedRate    float64 // bytes/sec over the last tick interval

	pendingAction powerAction // "" when no action armed
	actionResult  string
}

// rsrpHistMax caps the sparkline history to what fits in the box next to
// the 14-column label.
const rsrpHistMax = 28

type statusMsg struct {
	status *Status
	err    error
}

type tickMsg time.Time

type actionMsg struct {
	action powerAction
	err    error
}

// noteMsg sets the transient footer text shown below the dashboard. Used
// for one-shot side effects like "opened web UI" that aren't power actions.
type noteMsg string

func initialModel(d *SupportedDevice, refresh time.Duration) model {
	return model{device: d, loading: true, refresh: refresh}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchCmdFor(m.device), tickCmd(m.refresh))
}

func fetchCmdFor(d *SupportedDevice) tea.Cmd {
	return func() tea.Msg {
		s, err := tuiFetch(d)
		return statusMsg{status: s, err: err}
	}
}

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		key := msg.String()
		if m.pendingAction != "" {
			switch {
			case m.pendingAction == powerShutdown && key == "p",
				m.pendingAction == powerReboot && key == "R":
				action := m.pendingAction
				m.pendingAction = ""
				m.actionResult = string(action) + "…"
				return m, actionCmdFor(m.device, action)
			default:
				m.pendingAction = ""
				return m, nil
			}
		}
		switch key {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			m.loading = true
			return m, fetchCmdFor(m.device)
		case "w":
			return m, openWebUICmd(m.device)
		case "p":
			m.pendingAction = powerShutdown
		case "R":
			m.pendingAction = powerReboot
		}
	case statusMsg:
		m.loading = false
		m.actionResult = ""
		if msg.err != nil {
			// Keep the last-known status visible; View marks it stale.
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.status = msg.status
		now := time.Now()
		m.lastUpdate = now
		if s := msg.status; s != nil {
			if s.RSRP != 0 {
				m.rsrpHist = append(m.rsrpHist, s.RSRP)
				if len(m.rsrpHist) > rsrpHistMax {
					m.rsrpHist = m.rsrpHist[len(m.rsrpHist)-rsrpHistMax:]
				}
			}
			if s.TotalBytes > 0 {
				m.derivedRate = computeRate(m.prevTotalBytes, s.TotalBytes, m.prevSampleTime, now)
				m.prevTotalBytes = s.TotalBytes
				m.prevSampleTime = now
			}
		}
	case tickMsg:
		// If the previous fetch is still in flight, don't queue another
		// behind the device mutex — just reschedule the tick.
		if m.loading {
			return m, tickCmd(m.refresh)
		}
		m.loading = true
		return m, tea.Batch(fetchCmdFor(m.device), tickCmd(m.refresh))
	case actionMsg:
		if msg.err != nil {
			m.actionResult = string(msg.action) + " failed: " + msg.err.Error()
		} else {
			m.actionResult = string(msg.action) + " sent"
		}
	case noteMsg:
		m.actionResult = string(msg)
	}
	return m, nil
}

// openWebUICmd launches the router's web UI in the user's default browser
// via xdg-open. Uses the --addr override if set, otherwise the device's
// default address.
func openWebUICmd(d *SupportedDevice) tea.Cmd {
	return func() tea.Msg {
		url := "http://" + resolveAddr(d, *flagAddr) + "/"
		if err := exec.Command("xdg-open", url).Start(); err != nil {
			return noteMsg("failed to open " + url + ": " + err.Error())
		}
		return noteMsg("opened " + url)
	}
}

func actionCmdFor(d *SupportedDevice, action powerAction) tea.Cmd {
	return func() tea.Msg {
		tuiDeviceMu.Lock()
		defer tuiDeviceMu.Unlock()

		if tuiDevice == nil {
			dev, err := openDevice(d, *flagAddr, *flagPass, *flagDebug)
			if err != nil {
				return actionMsg{action: action, err: err}
			}
			tuiDevice = dev
		}
		var err error
		switch action {
		case powerShutdown:
			err = tuiDevice.Shutdown()
		case powerReboot:
			err = tuiDevice.Reboot()
		}
		// The router drops the connection after either, so the device is stale.
		tuiDevice.Close()
		tuiDevice = nil
		return actionMsg{action: action, err: err}
	}
}

// Colors are adaptive: the old hardcoded palette assumed a dark terminal
// (white value text is invisible on a light background).
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#008a6c", Dark: "#00d4aa"}).
			MarginBottom(1)

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.AdaptiveColor{Light: "#aaaaaa", Dark: "#555555"}).
			Padding(1, 2).
			Width(46)

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#666666", Dark: "#888888"}).
			Width(14)

	valueStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#111111", Dark: "#ffffff"})

	goodStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#008a6c", Dark: "#00d4aa"})
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#a86e00", Dark: "#ffaa00"})
	critStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#c41e1e", Dark: "#ff4444"})
	dimStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#999999", Dark: "#555555"})
)

func writeRow(b *strings.Builder, label string, valStyle lipgloss.Style, value string) {
	b.WriteString(labelStyle.Render(label))
	b.WriteString(valStyle.Render(value))
	b.WriteByte('\n')
}

func (m model) View() string {
	var content strings.Builder
	content.WriteString(titleStyle.Render(m.device.Title))
	content.WriteByte('\n')

	// A full-screen error only when we never had data; once a fetch has
	// succeeded, errors show in the footer under the stale dashboard.
	if m.status == nil {
		if m.err != nil {
			content.WriteString(critStyle.Render("Error: " + m.err.Error()))
			content.WriteString("\n\n")
			content.WriteString(dimStyle.Render("r = retry  q = quit"))
		} else {
			content.WriteString(dimStyle.Render("Connecting..."))
		}
		return boxStyle.Render(content.String())
	}

	s := m.status

	netStr := s.NetworkType
	if netStr == "" {
		netStr = "Unknown"
	}
	netStyle := goodStyle
	if netStr == "No Service" || netStr == "Unknown" {
		netStyle = critStyle
	}
	writeRow(&content, "Connection", netStyle, netStr)

	bars := signalBars(s.SignalStrength)
	sigStyle := goodStyle
	switch {
	case s.SignalStrength <= 1:
		sigStyle = critStyle
	case s.SignalStrength <= 2:
		sigStyle = warnStyle
	}
	writeRow(&content, "Signal", sigStyle, fmt.Sprintf("%s  %d/5", bars, s.SignalStrength))

	if len(m.rsrpHist) >= 2 {
		writeRow(&content, "History", sigStyle, sparkline(m.rsrpHist))
	}

	batBar := gaugeBar(s.BatteryPercent)
	batStyle := goodStyle
	switch {
	case s.BatteryPercent < 20:
		batStyle = critStyle
	case s.BatteryPercent < 40:
		batStyle = warnStyle
	}
	charging := ""
	if s.BatteryCharging {
		charging = " [charging]"
	}
	writeRow(&content, "Battery", batStyle, fmt.Sprintf("%s %d%%%s", batBar, s.BatteryPercent, charging))

	dataGB := bytesToGB(s.TotalBytes)
	limitGB := bytesToGB(s.MonthLimitBytes)
	dataStr := fmt.Sprintf("%.2f GB", dataGB)
	dataStyle := valueStyle
	dataPct := -1.0
	if limitGB > 0 {
		dataPct = (dataGB / limitGB) * 100
		dataStr = fmt.Sprintf("%.2f / %.0f GB (%.0f%%)", dataGB, limitGB, dataPct)
		switch {
		case dataPct > 90:
			dataStyle = critStyle
		case dataPct > 70:
			dataStyle = warnStyle
		}
	}
	writeRow(&content, "Data Used", dataStyle, dataStr)
	if dataPct >= 0 {
		writeRow(&content, "", dataStyle, gaugeBar(int(dataPct)))
	}
	if s.DailyBytes > 0 {
		writeRow(&content, "Today", valueStyle, humanBytes(s.DailyBytes))
	}

	// Live throughput: the M7010 reports split speeds directly; on the
	// Mudi we derive a combined rate from the traffic counter delta.
	rx := parseSpeed(s.RxSpeed)
	tx := parseSpeed(s.TxSpeed)
	switch {
	case rx > 0 || tx > 0:
		writeRow(&content, "Speed", valueStyle,
			fmt.Sprintf("↓ %s  ↑ %s", humanRate(rx), humanRate(tx)))
	case m.derivedRate > 0:
		writeRow(&content, "Speed", valueStyle, "≈ "+humanRate(m.derivedRate))
	}

	if s.Operator != "" {
		opStr := s.Operator
		if s.Band > 0 {
			opStr += fmt.Sprintf("  B%d", s.Band)
		}
		writeRow(&content, "Operator", valueStyle, opStr)
	}

	if s.WanIP != "" {
		writeRow(&content, "WAN IP", valueStyle, s.WanIP)
	}

	if s.ConnectedDevices > 0 {
		writeRow(&content, "Devices", valueStyle, fmt.Sprintf("%d", s.ConnectedDevices))
	}
	if s.UptimeSec > 0 {
		writeRow(&content, "Uptime", valueStyle, formatUptime(s.UptimeSec))
	}
	if temps := formatTemps(s); temps != "" {
		// formatTemps prefixes with "Temp: "; strip it because the TUI
		// already prints a "Temp" label column.
		writeRow(&content, "Temp", valueStyle, strings.TrimPrefix(temps, "Temp: "))
	}
	if s.LoadAvg[0] > 0 || s.LoadAvg[1] > 0 || s.LoadAvg[2] > 0 {
		writeRow(&content, "Load", valueStyle, fmt.Sprintf("%.2f %.2f %.2f",
			s.LoadAvg[0], s.LoadAvg[1], s.LoadAvg[2]))
	}

	content.WriteByte('\n')
	switch {
	case m.pendingAction == powerShutdown:
		content.WriteString(warnStyle.Render("Press p again to POWER OFF, any other key to cancel"))
	case m.pendingAction == powerReboot:
		content.WriteString(warnStyle.Render("Press R again to REBOOT, any other key to cancel"))
	case m.actionResult != "":
		content.WriteString(goodStyle.Render(m.actionResult))
	case m.err != nil:
		ago := time.Since(m.lastUpdate).Round(time.Second)
		content.WriteString(critStyle.Render("Error: " + m.err.Error()))
		content.WriteByte('\n')
		content.WriteString(dimStyle.Render(
			fmt.Sprintf("showing data from %s ago — r retry  q quit", ago)))
	default:
		footer := "r refresh  w web UI  p poweroff  R reboot  q quit"
		if m.loading {
			footer += "  (refreshing…)"
		}
		content.WriteString(dimStyle.Render(footer))
	}

	return boxStyle.Render(content.String())
}

func runTUI() {
	d := pickDevice()
	if d == nil {
		fmt.Fprintln(os.Stderr, "no supported router on the LAN (probed: "+supportedIDs()+")")
		fmt.Fprintln(os.Stderr, "pass --device to skip autodetect, or check that the router is reachable")
		os.Exit(1)
	}
	p := tea.NewProgram(initialModel(d, *flagRefresh), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// --- Display helpers ---

func bytesToGB(b float64) float64 {
	return b / (1024 * 1024 * 1024)
}

// formatUptime turns the router's `uptime` (float seconds) into a compact
// human label: "45s", "12m", "3h 27m", "2d 4h". Picks the two largest
// non-zero units so it stays readable on a one-line tooltip.
func formatUptime(sec float64) string {
	s := int(sec)
	d := s / 86400
	h := (s % 86400) / 3600
	m := (s % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// formatTemps renders the per-package temperatures the Mudi reports.
// Returns empty when neither is populated (e.g. M7010, which has no
// equivalent).
func formatTemps(s *Status) string {
	switch {
	case s.CPUTempC > 0 && s.MCUTempC > 0:
		return fmt.Sprintf("Temp: CPU %d°C  MCU %.1f°C", s.CPUTempC, s.MCUTempC)
	case s.CPUTempC > 0:
		return fmt.Sprintf("Temp: CPU %d°C", s.CPUTempC)
	case s.MCUTempC > 0:
		return fmt.Sprintf("Temp: MCU %.1f°C", s.MCUTempC)
	}
	return ""
}

func signalBars(strength int) string {
	bars := [5]string{"▁", "▂", "▄", "▆", "█"}
	var b strings.Builder
	for i := 0; i < 5; i++ {
		if i < strength {
			b.WriteString(bars[i])
		} else {
			b.WriteString("░")
		}
	}
	return b.String()
}

// gaugeBar renders a 0-100 percentage as a 10-slot bar. Used for the
// battery and the monthly data limit.
func gaugeBar(pct int) string {
	filled := pct / 10
	if filled > 10 {
		filled = 10
	}
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < 10; i++ {
		if i < filled {
			b.WriteString("█")
		} else {
			b.WriteString("░")
		}
	}
	b.WriteByte(']')
	return b.String()
}

// sparkline renders RSRP samples on a fixed -125…-75 dBm scale (the same
// span rsrpToSignal maps to 0-5 bars), so the shape is comparable across
// sessions rather than auto-scaled to whatever range the buffer holds.
func sparkline(hist []int) string {
	const lo, hi = -125, -75
	levels := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range hist {
		idx := (v - lo) * (len(levels) - 1) / (hi - lo)
		if idx < 0 {
			idx = 0
		}
		if idx > len(levels)-1 {
			idx = len(levels) - 1
		}
		b.WriteRune(levels[idx])
	}
	return b.String()
}

// humanBytes formats a byte count with a unit that keeps 2-4 significant
// digits: "512 B", "42.7 MB", "13.48 GB".
func humanBytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", b/(1<<10))
	default:
		return fmt.Sprintf("%.0f B", b)
	}
}

func humanRate(bytesPerSec float64) string {
	return humanBytes(bytesPerSec) + "/s"
}

// parseSpeed reads the M7010's txSpeed/rxSpeed decimal strings (bytes/s).
func parseSpeed(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// computeRate derives bytes/sec from two samples of a monotonic traffic
// counter. Returns 0 when there's no previous sample or the counter went
// backwards (reboot / statistics reset).
func computeRate(prevBytes, curBytes float64, prevTime, curTime time.Time) float64 {
	if prevTime.IsZero() || !curTime.After(prevTime) || curBytes < prevBytes {
		return 0
	}
	return (curBytes - prevBytes) / curTime.Sub(prevTime).Seconds()
}
