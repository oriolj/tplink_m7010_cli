package main

// GL.iNet Mudi (GL-E5800) JSON-RPC client.
//
// Wire format is nothing like the M7010's AES+RSA envelope. The Mudi
// (firmware 4.x, based on OpenWrt 22.03) exposes a JSON-RPC endpoint at
// /rpc with two phases:
//
//  1. POST {"method":"challenge","params":{"username":"root"}}
//     → {salt, nonce, alg:5, hash-method:"sha256"}
//
//  2. cryptHash = sha256-crypt(password, salt)   (the $5$salt$… string)
//     loginHash = sha256(username + ":" + cryptHash + ":" + nonce)
//     POST {"method":"login","params":{"username":"root","hash":loginHash}}
//     → {sid, username}
//
//  3. Authenticated calls go through the generic "call" method with
//     params=[sid, service, method, args]. args MUST be an array — a JSON
//     object (`{}`) is rejected with "Invalid params" even when the method
//     takes no arguments, because the GL server position-parses params.
//
// See PROTOCOL_GLINET.md for the full picture and the rationale behind
// the field choices in parseMudi* below.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

const (
	mudiDefaultAddr = "192.168.8.1"
	mudiUsername    = "root" // GL.iNet UI logs in as root
	mudiRPCPath     = "/rpc"
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

// Fetch pulls the home-screen status (system + modem + clients) into a
// Status struct shared with the M7010 path. The two RPCs are independent,
// so they run in parallel — halves wall-clock per tick.
func (m *MudiClient) Fetch() (*Status, error) {
	var (
		sysRaw, modemRaw any
		sysErr, modemErr error
		wg               sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		sysRaw, sysErr = m.callSvc("system", "get_status", nil)
	}()
	go func() {
		defer wg.Done()
		modemRaw, modemErr = m.callSvc("modem", "get_modems_info", nil)
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

	// modem.get_modems_info returns [] when no SIM is active. We still
	// have NetworkType from system.network[]; we just lose RSRP/operator.
	if modemErr == nil {
		s.RawFlowStat = map[string]any{"modems": modemRaw}
		if list, ok := modemRaw.([]any); ok && len(list) > 0 {
			if first, ok := list[0].(map[string]any); ok {
				parseMudiModem(s, first)
			}
		}
	}

	return s, nil
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

func parseMudiModem(s *Status, mm map[string]any) {
	// GL.iNet renames these fields across firmware revisions; add candidates
	// when a new firmware shows up rather than swapping.
	if op := firstStr(mm, "carrier", "operator_name", "operator", "operator_short", "network_name", "isp"); op != "" {
		s.Operator = op
	}
	if nt := firstStr(mm, "network_type", "act", "network_act", "modem_act", "network_mode", "current_network"); nt != "" {
		s.NetworkType = nt
	}
	s.Band = firstInt(mm, "band", "lte_band", "primary_band", "lte_band_num")
	s.RSRP = firstInt(mm, "rsrp", "lte_rsrp")
	s.RSRQ = firstInt(mm, "rsrq", "lte_rsrq")
	s.RSSI = firstInt(mm, "rssi", "lte_rssi", "signal_rssi")
	s.SNR = firstInt(mm, "sinr", "snr", "lte_sinr")
	s.SignalStrength = firstInt(mm, "signal_bars", "signal_strength", "signal_quality", "signal_level")
	if s.SignalStrength == 0 && s.RSRP != 0 {
		s.SignalStrength = rsrpToSignal(s.RSRP)
	}
	if ip := firstStr(mm, "ipv4", "ip", "wan_ipv4", "ipaddr"); ip != "" {
		s.WanIP = ip
	}
	if used := firstFloatish(mm, "total_used", "data_used", "month_used", "rx_tx_total"); used > 0 {
		s.TotalBytes = used
	}
	if day := firstFloatish(mm, "day_used", "today_used", "daily_used"); day > 0 {
		s.DailyBytes = day
	}
	if limit := firstFloatish(mm, "data_limit", "limit", "monthly_limit", "total_limit"); limit > 0 {
		s.MonthLimitBytes = limit
	}
}

// first{Str,Int,Floatish} delegate to the per-key helpers in client.go,
// returning the first non-zero / non-empty value among keys.
func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := jsonStr(m, k); v != "" {
			return v
		}
	}
	return ""
}

func firstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v := jsonInt(m, k); v != 0 {
			return v
		}
	}
	return 0
}

func firstFloatish(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v := jsonFloatStr(m, k); v != 0 {
			return v
		}
	}
	return 0
}
