// Command tplink-m7010 reads connection, signal, battery and data-usage state
// from a TP-Link M7010 mobile Wi-Fi hotspot. Three modes:
//
//   - default: interactive Bubble Tea TUI dashboard
//   - --waybar: single JSON line for use as a waybar custom module
//   - --raw:    decrypted raw API responses, for debugging / exploration
//
// Configuration is by flag or by TPLINK_ADDR / TPLINK_PASS env vars.
// See PROTOCOL.md for wire-format details and CLAUDE.md for guidance on
// modifying the crypto.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	flagAddr     = flag.String("addr", "192.168.0.1", "modem IP address")
	flagPass     = flag.String("pass", "admin", "admin password")
	flagWaybar   = flag.Bool("waybar", false, "output waybar JSON and exit")
	flagRaw      = flag.Bool("raw", false, "dump raw API responses and exit")
	flagDebug    = flag.Bool("debug", false, "print debug HTTP traffic")
	flagPoweroff = flag.Bool("poweroff", false, "power the modem off and exit")
	flagReboot   = flag.Bool("reboot", false, "reboot the modem and exit")
	flagRefresh  = flag.Duration("refresh", 10*time.Second, "TUI refresh interval")
)

func main() {
	flag.Parse()

	if p := os.Getenv("TPLINK_PASS"); p != "" {
		*flagPass = p
	}
	if a := os.Getenv("TPLINK_ADDR"); a != "" {
		*flagAddr = a
	}

	switch {
	case *flagPoweroff:
		runPower("shutdown")
	case *flagReboot:
		runPower("reboot")
	case *flagWaybar:
		runWaybar()
	case *flagRaw:
		runRaw()
	default:
		runTUI()
	}
}

func runPower(action string) {
	c := NewClient(*flagAddr, *flagDebug)
	if err := c.Login(*flagPass); err != nil {
		fmt.Fprintf(os.Stderr, "login: %v\n", err)
		os.Exit(1)
	}

	var err error
	switch action {
	case "shutdown":
		err = c.Shutdown()
	case "reboot":
		err = c.Reboot()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
		os.Exit(1)
	}
	fmt.Printf("%s command sent\n", action)
}

// --- Waybar mode ---

type WaybarOutput struct {
	Text    string `json:"text"`
	Tooltip string `json:"tooltip"`
	Class   string `json:"class"`
	Alt     string `json:"alt"`
}

func runWaybar() {
	// One-shot; skip Logout to save a round-trip — the modem expires tokens itself.
	status, err := fetchStatusOneShot(false)
	if err != nil {
		out := WaybarOutput{
			Text:    " --",
			Tooltip: "Error: " + err.Error(),
			Class:   "disconnected",
		}
		json.NewEncoder(os.Stdout).Encode(out)
		os.Exit(1)
	}

	signal := signalBars(status.SignalStrength)
	dataGB := bytesToGB(status.TotalBytes)
	limitGB := bytesToGB(status.MonthLimitBytes)

	text := fmt.Sprintf("%s %s  %d%%  %.1fGB",
		status.NetworkType, signal, status.BatteryPercent, dataGB)

	var tooltip strings.Builder
	fmt.Fprintf(&tooltip, "Connection: %s", status.NetworkType)
	if status.Operator != "" {
		fmt.Fprintf(&tooltip, " (%s)", status.Operator)
	}
	if status.Band > 0 {
		fmt.Fprintf(&tooltip, " B%d", status.Band)
	}
	fmt.Fprintf(&tooltip, "\n")
	fmt.Fprintf(&tooltip, "Signal: %d/5 %s  RSRP: %d dBm\n", status.SignalStrength, signal, status.RSRP)
	if status.BatteryCharging {
		fmt.Fprintf(&tooltip, "Battery: %d%% (charging)\n", status.BatteryPercent)
	} else {
		fmt.Fprintf(&tooltip, "Battery: %d%%\n", status.BatteryPercent)
	}
	fmt.Fprintf(&tooltip, "Data Used: %.2f GB", dataGB)
	if limitGB > 0 {
		fmt.Fprintf(&tooltip, " / %.0f GB", limitGB)
	}
	fmt.Fprintf(&tooltip, "\n")
	if status.WanIP != "" {
		fmt.Fprintf(&tooltip, "WAN IP: %s\n", status.WanIP)
	}
	if status.ConnectedDevices > 0 {
		fmt.Fprintf(&tooltip, "Devices: %d", status.ConnectedDevices)
	}

	class := "good"
	if status.BatteryPercent < 20 {
		class = "critical"
	} else if status.BatteryPercent < 40 {
		class = "warning"
	}
	if status.NetworkType == "" || status.NetworkType == "No Service" {
		class = "disconnected"
	}

	json.NewEncoder(os.Stdout).Encode(WaybarOutput{
		Text:    text,
		Tooltip: tooltip.String(),
		Class:   class,
		Alt:     status.NetworkType,
	})
}

// --- Raw dump mode ---

func runRaw() {
	status, err := fetchStatusOneShot(true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	fmt.Println("=== Status Response ===")
	enc.Encode(status.RawStatus)
	fmt.Println("\n=== FlowStat Response ===")
	enc.Encode(status.RawFlowStat)
}

// fetchStatusOneShot logs in, reads, and optionally logs out. Logout is
// skipped in waybar mode (one-shot, fire-and-forget) to save a round-trip.
func fetchStatusOneShot(doLogout bool) (*Status, error) {
	c := NewClient(*flagAddr, *flagDebug)
	if err := c.Login(*flagPass); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	if doLogout {
		defer c.Logout()
	}
	return fetchInto(c)
}

func fetchInto(c *Client) (*Status, error) {
	status, err := c.GetStatus()
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	if err := c.GetFlowStats(status); err != nil {
		return nil, fmt.Errorf("flowstat: %w", err)
	}
	return status, nil
}

// --- TUI mode ---
//
// The TUI keeps a single logged-in Client across ticks instead of
// re-authenticating every refresh. On any error the client is dropped and
// the next tick re-logs in.

var (
	tuiClient   *Client
	tuiClientMu sync.Mutex
)

func tuiFetch() (*Status, error) {
	tuiClientMu.Lock()
	defer tuiClientMu.Unlock()

	if tuiClient == nil {
		c := NewClient(*flagAddr, *flagDebug)
		if err := c.Login(*flagPass); err != nil {
			return nil, fmt.Errorf("login: %w", err)
		}
		tuiClient = c
	}

	status, err := fetchInto(tuiClient)
	if err != nil {
		tuiClient = nil
		return nil, err
	}
	return status, nil
}

type model struct {
	status  *Status
	err     error
	loading bool
	refresh time.Duration

	// pendingAction is set to "poweroff" or "reboot" after first keypress,
	// cleared when confirmed (second press) or cancelled (any other key).
	pendingAction string
	actionResult  string
}

type statusMsg struct {
	status *Status
	err    error
}

type tickMsg time.Time

type actionMsg struct {
	action string
	err    error
}

func initialModel(refresh time.Duration) model {
	return model{loading: true, refresh: refresh}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchCmd, tickCmd(m.refresh))
}

func fetchCmd() tea.Msg {
	s, err := tuiFetch()
	return statusMsg{status: s, err: err}
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

		// Second press on the same action key confirms and fires it.
		if m.pendingAction != "" {
			switch {
			case m.pendingAction == "poweroff" && key == "p",
				m.pendingAction == "reboot" && key == "R":
				action := m.pendingAction
				m.pendingAction = ""
				m.actionResult = action + "…"
				return m, actionCmd(action)
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
			return m, fetchCmd
		case "p":
			m.pendingAction = "poweroff"
		case "R":
			m.pendingAction = "reboot"
		}
	case statusMsg:
		m.loading = false
		m.status = msg.status
		m.err = msg.err
		m.actionResult = "" // fresh data — drop stale "reboot sent" notice
	case tickMsg:
		m.loading = true
		return m, tea.Batch(fetchCmd, tickCmd(m.refresh))
	case actionMsg:
		if msg.err != nil {
			m.actionResult = msg.action + " failed: " + msg.err.Error()
		} else {
			m.actionResult = msg.action + " sent"
		}
	}
	return m, nil
}

func actionCmd(action string) tea.Cmd {
	return func() tea.Msg {
		tuiClientMu.Lock()
		defer tuiClientMu.Unlock()

		if tuiClient == nil {
			c := NewClient(*flagAddr, *flagDebug)
			if err := c.Login(*flagPass); err != nil {
				return actionMsg{action: action, err: err}
			}
			tuiClient = c
		}

		var err error
		switch action {
		case "poweroff":
			err = tuiClient.Shutdown()
		case "reboot":
			err = tuiClient.Reboot()
		}
		// The modem drops the connection after either, so the client is stale.
		tuiClient = nil
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
	content.WriteString(titleStyle.Render("TP-Link M7010"))
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
	case m.pendingAction == "poweroff":
		content.WriteString(warnStyle.Render("Press p again to POWER OFF, any other key to cancel"))
	case m.pendingAction == "reboot":
		content.WriteString(warnStyle.Render("Press R again to REBOOT, any other key to cancel"))
	case m.actionResult != "":
		content.WriteString(goodStyle.Render(m.actionResult))
	default:
		footer := "r refresh  p poweroff  R reboot  q quit"
		if m.loading {
			footer += "  (refreshing…)"
		}
		content.WriteString(dimStyle.Render(footer))
	}

	return boxStyle.Render(content.String())
}

func runTUI() {
	p := tea.NewProgram(initialModel(*flagRefresh), tea.WithAltScreen())
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
