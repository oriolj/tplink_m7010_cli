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
	mudiWSPath      = "/ws?sid=" // sid is appended

	// mudiWSCollectTimeout bounds how long Fetch waits for the cellular
	// signal/SIM/traffic stream. Server pushes events every ~10s; we
	// usually see the first batch within ~50ms of connecting because nginx
	// is pre-warmed. 2s is a safe cap before we give up and ship whatever
	// we got.
	mudiWSCollectTimeout = 2 * time.Second
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
		cellular, wsErr = m.collectCellular(mudiWSCollectTimeout)
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

// cellularSnapshot is whatever we managed to collect from the WebSocket
// before the timeout fired. Any field can be empty/nil.
type cellularSnapshot struct {
	simsStatus     []map[string]any // cellular.sims_status
	networksInfo   []map[string]any // cellular.networks_info
	networksStatus []map[string]any // cellular.networks_status
	simsInfo       []map[string]any // cellular.sims_info
	raw            map[string]any   // everything we saw, for --raw mode
}

// mudiSubscribeEvents lists the WS event names we ask the server to push.
// Server is silent until we subscribe — see PROTOCOL_GLINET.md for the
// owsd-style {"cmd":"subscribe","name":…} protocol. The first burst
// arrives within ~20ms of subscribing; subsequent updates every ~10s.
var mudiSubscribeEvents = []string{
	"cellular.modems_info",     // Quectel hardware: model, IMEIs, supported bands
	"cellular.modems_status",   // modem-level state
	"cellular.sims_info",       // SIM identity per slot (iccid, imsi, apn_list)
	"cellular.sims_status",     // SIM op state per slot (carrier, strength 0-4, technology)
	"cellular.networks_info",   // cell_info{mode, band, rsrp, rsrq, sinr, dl_bandwidth}, ipv4
	"cellular.networks_status", // traffic_total, dial_status per slot
}

// collectCellular opens the Mudi's event stream, subscribes to the
// cellular topics, and reads until we have a complete picture (or the
// timeout fires).
func (m *MudiClient) collectCellular(timeout time.Duration) (cellularSnapshot, error) {
	var snap cellularSnapshot
	if m.sid == "" {
		return snap, fmt.Errorf("not logged in")
	}
	ws, err := dialWS(m.addr, mudiWSPath+m.sid, "Admin-Token="+m.sid, timeout)
	if err != nil {
		return snap, err
	}
	defer ws.close()

	deadline := time.Now().Add(timeout)
	ws.c.SetWriteDeadline(deadline)
	ws.c.SetReadDeadline(deadline)

	for _, name := range mudiSubscribeEvents {
		if err := ws.sendText(fmt.Sprintf(`{"cmd":"subscribe","name":%q}`, name)); err != nil {
			return snap, fmt.Errorf("ws subscribe %s: %w", name, err)
		}
	}

	snap.raw = map[string]any{}
	for {
		msg, err := ws.readMessage()
		if err != nil {
			// EOF or read timeout — return what we have.
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
		case "cellular.sims_status":
			snap.simsStatus = mapList(ev.Data["sims"])
		case "cellular.sims_info":
			snap.simsInfo = mapList(ev.Data["sims"])
		case "cellular.networks_info":
			snap.networksInfo = mapList(ev.Data["networks"])
		case "cellular.networks_status":
			snap.networksStatus = mapList(ev.Data["networks"])
		}
		if snap.simsStatus != nil && snap.networksInfo != nil && snap.networksStatus != nil {
			break // got a full picture; stop early
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
// see PROTOCOL_GLINET.md for the full table. Unknown modes fall through
// to the raw label so we don't silently drop information.
func friendlyNetworkType(mode string) string {
	switch strings.ToUpper(mode) {
	case "NR5G-SA", "5G-SA", "NR-SA":
		return "5G+"
	case "NR5G-NSA", "5G-NSA", "NR-NSA", "NR5G", "5G":
		return "5G"
	case "LTE-CA", "LTE-A", "LTE-ADVANCED", "4G+":
		return "4G+"
	case "LTE", "4G":
		return "4G"
	case "HSPA+", "HSPAP", "HSPA-PLUS", "DC-HSDPA", "DC-HSPA+", "3G+":
		return "3G+"
	case "WCDMA", "UMTS", "HSPA", "HSDPA", "HSUPA", "3G":
		return "3G"
	case "EDGE", "GPRS-EDGE", "2G+":
		return "2G+"
	case "GSM", "GPRS", "2G":
		return "2G"
	}
	return mode
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
		if ipv4, ok := ni["ipv4"].(map[string]any); ok {
			if ip := jsonStr(ipv4, "ip"); ip != "" {
				s.WanIP = ip
			}
		}
		if cell, ok := ni["cell_info"].(map[string]any); ok {
			if mode := jsonStr(cell, "mode"); mode != "" {
				s.NetworkType = friendlyNetworkType(mode)
			}
			s.Band = jsonInt(cell, "band")
			// rsrp/rsrq/sinr come as decimal strings — reuse jsonFloatStr,
			// then truncate to int for the existing dBm display.
			s.RSRP = int(jsonFloatStr(cell, "rsrp"))
			s.RSRQ = int(jsonFloatStr(cell, "rsrq"))
			s.SNR = int(jsonFloatStr(cell, "sinr"))
			// rsrp_level (0-4) is the same scale as sims_status.strength.
			// If sims_status didn't fire yet, fall back to it.
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
// (dial_status == 0). Falls back to the first slot with any data.
func pickActiveSlot(c cellularSnapshot) string {
	for _, n := range c.networksStatus {
		if jsonInt(n, "dial_status") == 0 && jsonStr(n, "slot") != "" {
			return jsonStr(n, "slot")
		}
	}
	for _, n := range c.networksStatus {
		if slot := jsonStr(n, "slot"); slot != "" {
			return slot
		}
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
