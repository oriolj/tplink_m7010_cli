package main

// GL.iNet Mudi (GL-E5800) client.
//
// Two transports in one file:
//
//   - /rpc — JSON-RPC for system / clients / qos / network. Auth is
//     challenge → sha256-crypt → sha256(user:cryptHash:nonce) → sid.
//     The sid then rides on every authenticated call as params[0].
//
//   - /ws — WebSocket that pushes named cellular events (operator,
//     signal, traffic, dial state). The home-screen tile reads from
//     here and there is no equivalent /rpc method on the Mudi 7's
//     CPU-integrated 5G modem.
//
// See PROTOCOL_GLINET.md for the full wire format, the JSON-RPC error
// codes, the WS subscribe protocol, and the event schema.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	mudiDefaultAddr = "192.168.8.1"
	mudiUsername    = "root" // GL.iNet UI logs in as root
	mudiRPCPath     = "/rpc"

	// mudiWSCollectTimeout bounds how long Fetch waits for the cellular
	// signal/SIM/traffic stream. Server pushes events every ~10s; the
	// first burst arrives within ~50ms of subscribing. 2s is a safe cap.
	mudiWSCollectTimeout = 2 * time.Second

	evSimsStatus     = "cellular.sims_status"
	evNetworksInfo   = "cellular.networks_info"
	evNetworksStatus = "cellular.networks_status"
)

type MudiClient struct {
	addr       string
	httpClient *http.Client
	debug      bool
	sid        string
}

func NewMudiClient(addr string, debug bool) Device {
	if addr == "" {
		addr = mudiDefaultAddr
	}
	return &MudiClient{
		addr:       addr,
		debug:      debug,
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
}

func (m *MudiClient) Name() string { return "GL.iNet Mudi (GL-E5800)" }

var _ Device = (*MudiClient)(nil)

func (m *MudiClient) debugf(format string, args ...any) {
	if m.debug {
		fmt.Printf("[DEBUG] mudi "+format, args...)
	}
}

// rpc returns the raw "result" field. Callers type-assert it — different
// methods return objects vs. lists, so a single typed return is wrong.
func (m *MudiClient) rpc(method string, params any) (any, error) {
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	}
	body, _ := json.Marshal(envelope)
	m.debugf("POST %s body=%s\n", mudiRPCPath, string(body))

	resp, err := m.httpClient.Post("http://"+m.addr+mudiRPCPath,
		"application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", mudiRPCPath, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	m.debugf("resp=%s\n", string(data))

	var env struct {
		Result any            `json:"result"`
		Error  map[string]any `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode rpc envelope (%s): %w", data, err)
	}
	if env.Error != nil {
		msg, _ := env.Error["message"].(string)
		return nil, fmt.Errorf("rpc error: %s", msg)
	}
	return env.Result, nil
}

func (m *MudiClient) Connect(password string) error {
	challengeRaw, err := m.rpc("challenge", map[string]string{"username": mudiUsername})
	if err != nil {
		return fmt.Errorf("challenge: %w", err)
	}
	challenge, ok := challengeRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("challenge: unexpected shape %T", challengeRaw)
	}
	salt, _ := challenge["salt"].(string)
	nonce, _ := challenge["nonce"].(string)
	hashMethod, _ := challenge["hash-method"].(string)
	if salt == "" || nonce == "" {
		return fmt.Errorf("challenge incomplete: %v", challenge)
	}

	cryptHash := sha256Crypt(password, salt)
	inner := mudiUsername + ":" + cryptHash + ":" + nonce
	var loginHash string
	switch hashMethod {
	case "", "sha256":
		h := sha256.Sum256([]byte(inner))
		loginHash = hex.EncodeToString(h[:])
	default:
		return fmt.Errorf("unsupported hash-method %q (only sha256 is implemented)", hashMethod)
	}

	loginRaw, err := m.rpc("login", map[string]string{
		"username": mudiUsername,
		"hash":     loginHash,
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	login, ok := loginRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("login: unexpected shape %T", loginRaw)
	}
	sid, _ := login["sid"].(string)
	if sid == "" {
		return fmt.Errorf("login: no sid in response (wrong password?)")
	}
	m.sid = sid
	return nil
}

// callSvc invokes a JSON-RPC "call" with the standard [sid, service,
// method, args] params shape. args must be a slice (an empty []any{} for
// nullary methods); a JSON object is rejected by the firmware.
func (m *MudiClient) callSvc(service, method string, args []any) (any, error) {
	if m.sid == "" {
		return nil, fmt.Errorf("not logged in")
	}
	if args == nil {
		args = []any{}
	}
	return m.rpc("call", []any{m.sid, service, method, args})
}

func (m *MudiClient) Close() {
	if m.sid == "" {
		return
	}
	_, _ = m.rpc("logout", map[string]string{"sid": m.sid})
	m.sid = ""
}

// Shutdown and Reboot swallow any error — the router drops the connection
// before responding, matching the M7010 reboot path.
func (m *MudiClient) Shutdown() error {
	_, _ = m.callSvc("system", "poweroff", nil)
	return nil
}

func (m *MudiClient) Reboot() error {
	_, _ = m.callSvc("system", "reboot", nil)
	return nil
}

// Fetch pulls system state via /rpc and cellular state via the /ws
// event stream, in parallel. The Mudi 7's CPU-integrated 5G modem is not
// exposed under modem.get_modems_info — its signal, operator, and
// traffic counters only show up on the WebSocket (see PROTOCOL_GLINET.md
// for the schema and the discovery story).
func (m *MudiClient) Fetch() (*Status, error) {
	var (
		sysRaw   any
		sysErr   error
		cellular cellularSnapshot
		wsErr    error
		wg       sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		sysRaw, sysErr = m.callSvc("system", "get_status", nil)
	}()
	go func() {
		defer wg.Done()
		cellular, wsErr = m.collectCellular()
	}()
	wg.Wait()

	if sysErr != nil {
		return nil, fmt.Errorf("system.get_status: %w", sysErr)
	}
	sysMap, ok := sysRaw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("system.get_status: unexpected shape %T", sysRaw)
	}

	s := &Status{Model: m.Name(), RawStatus: sysMap}
	parseMudiSystem(s, sysMap)

	if wsErr == nil {
		s.RawFlowStat = cellular.raw
		applyCellular(s, cellular)
	}
	// wsErr is non-fatal: battery + connectivity from system.get_status
	// already populated. The widget will just show empty signal fields.

	return s, nil
}

// cellularSnapshot holds whatever we collected from the WS before the
// deadline. Any field can be nil. Only the three populated here are
// consumed downstream; sims_info / modems_* topics carry useful data
// (iccid, IMEI, supported bands) but the widget doesn't surface them,
// so we don't subscribe to them — see PROTOCOL_GLINET.md.
type cellularSnapshot struct {
	simsStatus     []map[string]any
	networksInfo   []map[string]any
	networksStatus []map[string]any
	raw            map[string]any
}

// collectCellular opens the Mudi's event stream, subscribes to the
// cellular topics we need, and reads until we have a complete picture
// (or mudiWSCollectTimeout elapses). The deadline is set once on the
// connection and covers dial + handshake + subscribe + read.
func (m *MudiClient) collectCellular() (cellularSnapshot, error) {
	var snap cellularSnapshot
	if m.sid == "" {
		return snap, fmt.Errorf("not logged in")
	}
	deadline := time.Now().Add(mudiWSCollectTimeout)
	ws, err := dialWS(m.addr, "/ws?sid="+m.sid, "Admin-Token="+m.sid, deadline)
	if err != nil {
		return snap, err
	}
	defer ws.close()

	for _, name := range [...]string{evSimsStatus, evNetworksInfo, evNetworksStatus} {
		if err := ws.sendText(fmt.Sprintf(`{"cmd":"subscribe","name":%q}`, name)); err != nil {
			return snap, fmt.Errorf("ws subscribe %s: %w", name, err)
		}
	}

	snap.raw = map[string]any{}
	for snap.simsStatus == nil || snap.networksInfo == nil || snap.networksStatus == nil {
		msg, err := ws.readMessage()
		if err != nil {
			break
		}
		var ev struct {
			Name string         `json:"name"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}
		m.debugf("ws %s\n", ev.Name)
		snap.raw[ev.Name] = ev.Data
		switch ev.Name {
		case evSimsStatus:
			snap.simsStatus = mapList(ev.Data["sims"])
		case evNetworksInfo:
			snap.networksInfo = mapList(ev.Data["networks"])
		case evNetworksStatus:
			snap.networksStatus = mapList(ev.Data["networks"])
		}
	}
	return snap, nil
}

// mapList unwraps a JSON array-of-objects.
func mapList(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// friendlyNetworkType maps Quectel's 3GPP mode strings onto the short
// generation labels phone UIs use. We treat 5G-SA as "5G+" (true 5G core)
// and 5G-NSA as "5G" (anchored to LTE), matching Android's 5G_PLUS vs 5G
// icon convention. LTE-CA / LTE-A becomes "4G+", HSPA+/DC-HSDPA "3G+";
// see PROTOCOL_GLINET.md for the full table.
//
// The Quectel modem decorates the mode with the duplex scheme
// ("LTE FDD", "LTE TDD") and sometimes with separator variants. We
// normalise both before matching: duplex is orthogonal to the
// generation tier (a fast 4G+ link can be FDD or TDD), so we strip it.
// Unknown modes fall through to the raw label.
func friendlyNetworkType(mode string) string {
	switch normalizeNetworkMode(mode) {
	case "NR5G-SA", "5G-SA", "NR-SA":
		return "5G+"
	case "NR5G-NSA", "5G-NSA", "NR-NSA", "NR5G":
		return "5G"
	case "LTE-CA", "LTE-A", "LTE-ADVANCED":
		return "4G+"
	case "LTE":
		return "4G"
	case "HSPA+", "HSPAP", "DC-HSDPA", "DC-HSPA+":
		return "3G+"
	case "WCDMA", "UMTS", "HSPA", "HSDPA", "HSUPA":
		return "3G"
	case "EDGE", "GPRS-EDGE":
		return "2G+"
	case "GSM", "GPRS":
		return "2G"
	}
	return mode
}

// normalizeNetworkMode uppercases the mode, swaps space for dash, and
// strips the duplex suffix so "LTE FDD" / "LTE-TDD" / "LTE-FDD-CA" all
// collapse onto a single canonical key.
func normalizeNetworkMode(mode string) string {
	m := strings.ToUpper(mode)
	m = strings.ReplaceAll(m, " ", "-")
	m = strings.ReplaceAll(m, "-FDD", "")
	m = strings.ReplaceAll(m, "-TDD", "")
	return strings.Trim(m, "-")
}

// applyCellular maps the WebSocket-derived snapshot onto the Status
// struct shared with the M7010 path. We pick the SIM that's actually
// dialled — `dial_status: 0` (success) — and use its slot to match up
// the other event types.
func applyCellular(s *Status, c cellularSnapshot) {
	activeSlot := pickActiveSlot(c)
	if activeSlot == "" {
		return
	}

	if ss := findBySlot(c.simsStatus, activeSlot); ss != nil {
		// strength is 0-4; map to our 0-5 scale by adding 1 when present.
		if v := jsonInt(ss, "strength"); v > 0 {
			s.SignalStrength = v + 1
		}
		if op := jsonStr(ss, "carrier"); op != "" {
			s.Operator = op
		}
	}

	if ni := findBySlot(c.networksInfo, activeSlot); ni != nil {
		if ip := jsonStr(subMap(ni, "ipv4"), "ip"); ip != "" {
			s.WanIP = ip
		}
		if cell := subMap(ni, "cell_info"); cell != nil {
			if mode := jsonStr(cell, "mode"); mode != "" {
				s.NetworkType = friendlyNetworkType(mode)
				s.NetworkTypeRaw = mode
			}
			s.Band = jsonInt(cell, "band")
			// rsrp/rsrq/sinr arrive as decimal strings — jsonInt would
			// reject the sign and the decimal point.
			s.RSRP = int(jsonFloatStr(cell, "rsrp"))
			s.RSRQ = int(jsonFloatStr(cell, "rsrq"))
			s.SNR = int(jsonFloatStr(cell, "sinr"))
			if s.SignalStrength == 0 {
				if lvl := jsonInt(cell, "rsrp_level"); lvl > 0 {
					s.SignalStrength = lvl + 1
				}
			}
		}
	}

	if ns := findBySlot(c.networksStatus, activeSlot); ns != nil {
		s.TotalBytes = jsonFloatStr(ns, "traffic_total")
	}
	if s.SignalStrength == 0 && s.RSRP != 0 {
		s.SignalStrength = rsrpToSignal(s.RSRP)
	}
}

// pickActiveSlot returns the slot ("1" / "2") whose dial succeeded
// (dial_status == 0). Falls back to any slot with data so the widget
// still shows something during dial-in or after dial failure.
func pickActiveSlot(c cellularSnapshot) string {
	var fallback string
	for _, n := range c.networksStatus {
		slot := jsonStr(n, "slot")
		if slot == "" {
			continue
		}
		if jsonInt(n, "dial_status") == 0 {
			return slot
		}
		if fallback == "" {
			fallback = slot
		}
	}
	if fallback != "" {
		return fallback
	}
	for _, s := range c.simsStatus {
		if slot := jsonStr(s, "slot"); slot != "" {
			return slot
		}
	}
	return ""
}

func findBySlot(list []map[string]any, slot string) map[string]any {
	for _, m := range list {
		if jsonStr(m, "slot") == slot {
			return m
		}
	}
	return nil
}

// subMap returns m[key] as a nested object, or nil if it isn't one.
// jsonStr / jsonInt are nil-safe so callers can pipeline subMap through
// them without an extra ok-check.
func subMap(m map[string]any, key string) map[string]any {
	sub, _ := m[key].(map[string]any)
	return sub
}

func parseMudiSystem(s *Status, root map[string]any) {
	// Battery on the Mudi 7 lives at system.mcu, not in a dedicated service.
	// charging_status is 0 on battery, >0 when the charger is connected.
	if sys, ok := root["system"].(map[string]any); ok {
		if mcu, ok := sys["mcu"].(map[string]any); ok {
			s.BatteryPercent = jsonInt(mcu, "charge_percent")
			s.BatteryCharging = jsonInt(mcu, "charging_status") > 0
		}
	}

	if netArr, ok := root["network"].([]any); ok {
		for _, ifAny := range netArr {
			iface, ok := ifAny.(map[string]any)
			if !ok {
				continue
			}
			name := jsonStr(iface, "interface")
			online := jsonBool(iface, "online")
			if !online {
				continue
			}
			switch name {
			case "modem_cpu", "modem_cpu_6", "wwan", "wwan6":
				if s.NetworkType == "" {
					s.NetworkType = "Cellular"
				}
			case "tethering", "tethering6":
				if s.NetworkType == "" {
					s.NetworkType = "Tethered"
				}
			case "wan", "wan6":
				if s.NetworkType == "" {
					s.NetworkType = "Wired"
				}
			}
		}
	}

	if cliArr, ok := root["client"].([]any); ok && len(cliArr) > 0 {
		if c0, ok := cliArr[0].(map[string]any); ok {
			s.ConnectedDevices = jsonInt(c0, "cable_total") +
				jsonInt(c0, "wireless_total") +
				jsonInt(c0, "usbeth_total")
		}
	}
}
