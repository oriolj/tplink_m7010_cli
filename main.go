// Command tplink-m7010 reads connection, signal, battery and data-usage state
// from a mobile Wi-Fi hotspot and renders it as one of:
//
//   - default:    interactive Bubble Tea TUI dashboard
//   - --waybar:   single JSON line for use as a waybar custom module
//   - --noctalia: single JSON line for the noctalia-shell CustomButton widget
//   - --raw:      raw API responses, for debugging / exploration
//
// Two device families are supported in the same binary:
//
//   - TP-Link M7010 (AES+RSA envelope; see PROTOCOL.md, client.go)
//   - GL.iNet Mudi GL-E5800 (OpenWrt JSON-RPC; see PROTOCOL_GLINET.md, mudi.go)
//
// By default the daemon TCP-probes both default addresses in parallel and
// talks only to whichever answers first — if neither is reachable, widget
// modes emit empty output and exit so the laptop battery isn't burned on
// pointless retries. Use `--device m7010|mudi` to skip autodetect, or
// `--addr` to override the address for the selected device.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
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

// reachable returns true if a TCP connection to addr:80 succeeds within
// timeout. Used by detectDevice's fallback path.
func reachable(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr, "80"), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
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
		enc.Encode(noctaliaOutput{})
		return
	}
	text, tooltip, class := formatStatusLine(d, status)
	enc.Encode(noctaliaOutput{
		Text:      text,
		Tooltip:   strings.TrimRight(tooltip, "\n"),
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
	fmt.Fprintf(&tb, "\n")
	fmt.Fprintf(&tb, "Signal: %d/5 %s", s.SignalStrength, signal)
	if s.RSRP != 0 {
		fmt.Fprintf(&tb, "  RSRP: %d dBm", s.RSRP)
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
	if s.WanIP != "" {
		fmt.Fprintf(&tb, "WAN IP: %s\n", s.WanIP)
	}
	if s.ConnectedDevices > 0 {
		fmt.Fprintf(&tb, "Devices: %d", s.ConnectedDevices)
	}
	tooltip = tb.String()

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

	pendingAction powerAction // "" when no action armed
	actionResult  string
}

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
		m.status = msg.status
		m.err = msg.err
		m.actionResult = ""
	case tickMsg:
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

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#00d4aa")).
			MarginBottom(1)

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#555")).
			Padding(1, 2).
			Width(46)

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888")).
			Width(14)

	valueStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#fff"))

	goodStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#00d4aa"))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffaa00"))
	critStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff4444"))
	dimStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#555"))
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

	if m.err != nil {
		content.WriteString(critStyle.Render("Error: " + m.err.Error()))
		content.WriteString("\n\n")
		content.WriteString(dimStyle.Render("r = retry  q = quit"))
		return boxStyle.Render(content.String())
	}

	if m.status == nil {
		content.WriteString(dimStyle.Render("Connecting..."))
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

	batBar := batteryBar(s.BatteryPercent)
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
	if limitGB > 0 {
		pct := (dataGB / limitGB) * 100
		dataStr = fmt.Sprintf("%.2f / %.0f GB (%.0f%%)", dataGB, limitGB, pct)
		switch {
		case pct > 90:
			dataStyle = critStyle
		case pct > 70:
			dataStyle = warnStyle
		}
	}
	writeRow(&content, "Data Used", dataStyle, dataStr)

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

	content.WriteByte('\n')
	switch {
	case m.pendingAction == powerShutdown:
		content.WriteString(warnStyle.Render("Press p again to POWER OFF, any other key to cancel"))
	case m.pendingAction == powerReboot:
		content.WriteString(warnStyle.Render("Press R again to REBOOT, any other key to cancel"))
	case m.actionResult != "":
		content.WriteString(goodStyle.Render(m.actionResult))
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

func batteryBar(pct int) string {
	filled := pct / 10
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
