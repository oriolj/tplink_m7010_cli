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
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	flagAddr    = flag.String("addr", "192.168.0.1", "modem IP address")
	flagPass    = flag.String("pass", "admin", "admin password")
	flagWaybar  = flag.Bool("waybar", false, "output waybar JSON and exit")
	flagRaw     = flag.Bool("raw", false, "dump raw API responses and exit")
	flagDebug   = flag.Bool("debug", false, "print debug HTTP traffic")
	flagRefresh = flag.Duration("refresh", 10*time.Second, "TUI refresh interval")
)

func main() {
	flag.Parse()

	// Allow password from env var
	if p := os.Getenv("TPLINK_PASS"); p != "" {
		*flagPass = p
	}
	if a := os.Getenv("TPLINK_ADDR"); a != "" {
		*flagAddr = a
	}

	if *flagWaybar {
		runWaybar()
		return
	}

	if *flagRaw {
		runRaw()
		return
	}

	runTUI()
}

// --- Waybar mode ---

type WaybarOutput struct {
	Text    string `json:"text"`
	Tooltip string `json:"tooltip"`
	Class   string `json:"class"`
	Alt     string `json:"alt"`
}

func runWaybar() {
	status, err := fetchStatus()
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

	out := WaybarOutput{
		Text:    text,
		Tooltip: tooltip.String(),
		Class:   class,
		Alt:     status.NetworkType,
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

// --- Raw dump mode ---

func runRaw() {
	status, err := fetchStatus()
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

// --- Shared fetch logic ---

func fetchStatus() (*Status, error) {
	c := NewClient(*flagAddr, *flagDebug)
	if err := c.Login(*flagPass); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	defer c.Logout()

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

type model struct {
	status  *Status
	err     error
	loading bool
	width   int
	height  int
	refresh time.Duration
}

type statusMsg struct {
	status *Status
	err    error
}

type tickMsg time.Time

func initialModel(refresh time.Duration) model {
	return model{loading: true, refresh: refresh}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchCmd, tickCmd(m.refresh))
}

func fetchCmd() tea.Msg {
	s, err := fetchStatus()
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
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			m.loading = true
			return m, fetchCmd
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case statusMsg:
		m.loading = false
		m.status = msg.status
		m.err = msg.err
	case tickMsg:
		m.loading = true
		return m, tea.Batch(fetchCmd, tickCmd(m.refresh))
	}
	return m, nil
}

func (m model) View() string {
	// Styles
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#00d4aa")).
		MarginBottom(1)

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#555")).
		Padding(1, 2).
		Width(46)

	labelStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#888")).
		Width(14)

	valueStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#fff"))

	goodStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#00d4aa"))

	warnStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#ffaa00"))

	critStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#ff4444"))

	dimStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#555"))

	var content strings.Builder

	content.WriteString(titleStyle.Render("TP-Link M7010"))
	content.WriteString("\n")

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

	// Connection type
	netStr := s.NetworkType
	if netStr == "" {
		netStr = "Unknown"
	}
	netStyle := goodStyle
	if netStr == "No Service" || netStr == "" {
		netStyle = critStyle
	}
	row := labelStyle.Render("Connection") + netStyle.Render(netStr)
	content.WriteString(row + "\n")

	// Signal
	bars := signalBars(s.SignalStrength)
	sigStyle := goodStyle
	if s.SignalStrength <= 1 {
		sigStyle = critStyle
	} else if s.SignalStrength <= 2 {
		sigStyle = warnStyle
	}
	row = labelStyle.Render("Signal") + sigStyle.Render(fmt.Sprintf("%s  %d/5", bars, s.SignalStrength))
	content.WriteString(row + "\n")

	// Battery
	batBar := batteryBar(s.BatteryPercent)
	batStyle := goodStyle
	if s.BatteryPercent < 20 {
		batStyle = critStyle
	} else if s.BatteryPercent < 40 {
		batStyle = warnStyle
	}
	charging := ""
	if s.BatteryCharging {
		charging = " [charging]"
	}
	row = labelStyle.Render("Battery") + batStyle.Render(fmt.Sprintf("%s %d%%%s", batBar, s.BatteryPercent, charging))
	content.WriteString(row + "\n")

	// Data usage
	dataGB := bytesToGB(s.TotalBytes)
	limitGB := bytesToGB(s.MonthLimitBytes)
	dataStr := fmt.Sprintf("%.2f GB", dataGB)
	dataStyle := valueStyle
	if limitGB > 0 {
		pct := (dataGB / limitGB) * 100
		dataStr = fmt.Sprintf("%.2f / %.0f GB (%.0f%%)", dataGB, limitGB, pct)
		if pct > 90 {
			dataStyle = critStyle
		} else if pct > 70 {
			dataStyle = warnStyle
		}
	}
	row = labelStyle.Render("Data Used") + dataStyle.Render(dataStr)
	content.WriteString(row + "\n")

	// Operator / Band
	if s.Operator != "" {
		opStr := s.Operator
		if s.Band > 0 {
			opStr += fmt.Sprintf("  B%d", s.Band)
		}
		row = labelStyle.Render("Operator") + valueStyle.Render(opStr)
		content.WriteString(row + "\n")
	}

	// WAN IP
	if s.WanIP != "" {
		row = labelStyle.Render("WAN IP") + valueStyle.Render(s.WanIP)
		content.WriteString(row + "\n")
	}

	// Connected devices
	if s.ConnectedDevices > 0 {
		row = labelStyle.Render("Devices") + valueStyle.Render(fmt.Sprintf("%d", s.ConnectedDevices))
		content.WriteString(row + "\n")
	}

	// Footer
	content.WriteString("\n")
	loadingStr := ""
	if m.loading {
		loadingStr = " refreshing..."
	}
	content.WriteString(dimStyle.Render(fmt.Sprintf("r = refresh  q = quit%s", loadingStr)))

	return boxStyle.Render(content.String())
}

func runTUI() {
	p := tea.NewProgram(initialModel(*flagRefresh), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// --- Helpers ---

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
	b.WriteString("[")
	for i := 0; i < 10; i++ {
		if i < filled {
			b.WriteString("█")
		} else {
			b.WriteString("░")
		}
	}
	b.WriteString("]")
	return b.String()
}
