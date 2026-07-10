package main

// End-to-end test of the Mudi client against a fake GL.iNet device:
// challenge/login over /rpc (real sha256-crypt verification), then a
// Fetch that exercises both collectors — system.get_status over /rpc and
// the cellular events over a real WebSocket handshake on /ws.
//
// The fake enforces the two firmware landmines documented in
// PROTOCOL_GLINET.md: `args` must be a JSON array (an object gets
// -32602 like the real dispatcher), and client WS frames must be masked.

import (
	"bufio"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeMudi struct {
	t        *testing.T
	password string
	salt     string
	nonce    string
	sid      string
}

func newFakeMudi(t *testing.T, password string) *fakeMudi {
	return &fakeMudi{
		t:        t,
		password: password,
		salt:     "8cZ9zPFLRxER8bEK",
		nonce:    "vPiFi2f7tyLd9q0sRlUio655eylETvKn",
		sid:      "TrOZShEm34hgSPHzPWJZEz0ecbDnQAZS",
	}
}

func (f *fakeMudi) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(mudiRPCPath, f.handleRPC)
	mux.HandleFunc("/ws", f.handleWS)
	return mux
}

func rpcResult(w http.ResponseWriter, id any, result any) {
	json.NewEncoder(w).Encode(map[string]any{"id": id, "jsonrpc": "2.0", "result": result})
}

func rpcError(w http.ResponseWriter, id any, code int, msg string) {
	json.NewEncoder(w).Encode(map[string]any{
		"id": id, "jsonrpc": "2.0",
		"error": map[string]any{"code": code, "message": msg},
	})
}

func (f *fakeMudi) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("rpc: bad request body: %v", err)
		return
	}

	switch req.Method {
	case "challenge":
		rpcResult(w, req.ID, map[string]any{
			"alg": 5, "salt": f.salt, "nonce": f.nonce, "hash-method": "sha256",
		})

	case "login":
		p, _ := req.Params.(map[string]any)
		user, _ := p["username"].(string)
		hash, _ := p["hash"].(string)
		inner := user + ":" + sha256Crypt(f.password, f.salt) + ":" + f.nonce
		want := sha256.Sum256([]byte(inner))
		if user != mudiUsername || hash != hex.EncodeToString(want[:]) {
			rpcError(w, req.ID, -32000, "Access denied")
			return
		}
		rpcResult(w, req.ID, map[string]any{"username": user, "sid": f.sid})

	case "logout":
		rpcResult(w, req.ID, map[string]any{})

	case "call":
		params, ok := req.Params.([]any)
		if !ok || len(params) != 4 {
			rpcError(w, req.ID, -32602, "Invalid params")
			return
		}
		sid, _ := params[0].(string)
		service, _ := params[1].(string)
		method, _ := params[2].(string)
		if sid != f.sid {
			rpcError(w, req.ID, -32000, "Access denied")
			return
		}
		// The real dispatcher rejects object args even for nullary methods.
		if _, isArray := params[3].([]any); !isArray {
			rpcError(w, req.ID, -32602, "Invalid params")
			return
		}
		switch service + "." + method {
		case "system.get_status":
			var status map[string]any
			json.Unmarshal([]byte(fakeMudiSystemStatus), &status)
			rpcResult(w, req.ID, status)
		case "system.reboot", "system.poweroff":
			rpcResult(w, req.ID, []any{})
		default:
			rpcError(w, req.ID, -32601, "Method not found")
		}

	default:
		rpcError(w, req.ID, -32601, "Method not found")
	}
}

const fakeMudiSystemStatus = `{
  "network": [
    {"interface":"modem_cpu","online":true,"up":true},
    {"interface":"wan","online":false,"up":false}
  ],
  "wifi": [],
  "service": [],
  "client": [{"cable_total":0,"usbeth_total":1,"wireless_total":2}],
  "system": {
    "uptime": 4532.5,
    "cpu": {"temperature": 43},
    "mcu": {"temperature": 35.5, "charging_status": 1, "charge_percent": 88},
    "load_average": [1.36, 1.39, 0.79]
  }
}`

var fakeMudiEvents = map[string]string{
	evSimsStatus: `{"sims":[
	  {"slot":"1","bus":"cpu","carrier":"Movistar","strength":4,"technology":51,"status":6},
	  {"slot":"2","bus":"cpu","strength":0,"status":0}
	]}`,
	evNetworksInfo: `{"networks":[
	  {"slot":"1","bus":"cpu",
	   "cell_info":{"mode":"NR5G-NSA","band":78,"dl_bandwidth":"100MHz",
	     "rsrp":"-79","rsrp_level":4,"rsrq":"-10","sinr":"12"},
	   "ipv4":{"ip":"10.112.6.17"}},
	  {"slot":"2","bus":"cpu"}
	]}`,
	evNetworksStatus: `{"networks":[
	  {"slot":"1","bus":"cpu","traffic_total":"234770803","dial_status":0},
	  {"slot":"2","bus":"cpu","traffic_total":"0","dial_status":1}
	]}`,
}

// handleWS speaks just enough server-side RFC 6455 for collectCellular:
// handshake, read masked subscribe frames, push unmasked event frames.
func (f *fakeMudi) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("sid") != f.sid {
		f.t.Errorf("ws: sid query = %q, want %q", r.URL.Query().Get("sid"), f.sid)
		http.Error(w, "bad sid", http.StatusForbidden)
		return
	}
	if !strings.Contains(r.Header.Get("Cookie"), "Admin-Token="+f.sid) {
		f.t.Errorf("ws: Admin-Token cookie missing: %q", r.Header.Get("Cookie"))
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		f.t.Fatalf("ws: response writer can't hijack")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		f.t.Errorf("ws hijack: %v", err)
		return
	}
	defer conn.Close()

	acceptHash := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(acceptHash[:]))
	brw.Flush()

	// Collect the three subscribes, then push one event per topic.
	var topics []string
	for len(topics) < 3 {
		payload, err := readClientFrame(f.t, brw.Reader)
		if err != nil {
			f.t.Errorf("ws: reading subscribe frame: %v", err)
			return
		}
		var sub struct{ Cmd, Name string }
		if json.Unmarshal(payload, &sub) != nil || sub.Cmd != "subscribe" || sub.Name == "" {
			f.t.Errorf("ws: unexpected client message %q", payload)
			return
		}
		topics = append(topics, sub.Name)
	}
	for _, name := range topics {
		data, ok := fakeMudiEvents[name]
		if !ok {
			// Real server stays silent for unknown topics.
			continue
		}
		event := fmt.Sprintf(`{"name":%q,"data":%s}`, name, data)
		if err := writeServerFrame(brw, wsOpcodeText, []byte(event)); err != nil {
			f.t.Errorf("ws: writing event: %v", err)
			return
		}
	}
	brw.Flush()
	// Drain until the client closes (it sends a close frame in ws.close()).
	io.Copy(io.Discard, brw.Reader)
}

// readClientFrame parses one client→server frame, asserting it's masked
// (RFC 6455 §5.3 — a real server drops unmasked client frames).
func readClientFrame(t *testing.T, br *bufio.Reader) ([]byte, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return nil, err
	}
	if hdr[1]&0x80 == 0 {
		t.Errorf("ws: client frame not masked (opcode %#x)", hdr[0]&0x0F)
	}
	plen := uint64(hdr[1] & 0x7F)
	switch plen {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(br, ext); err != nil {
			return nil, err
		}
		plen = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(br, ext); err != nil {
			return nil, err
		}
		plen = binary.BigEndian.Uint64(ext)
	}
	mask := make([]byte, 4)
	if _, err := io.ReadFull(br, mask); err != nil {
		return nil, err
	}
	payload := make([]byte, plen)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return payload, nil
}

func writeServerFrame(w io.Writer, opcode byte, payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x80|opcode)
	switch {
	case len(payload) < 126:
		hdr = append(hdr, byte(len(payload)))
	case len(payload) < 65536:
		hdr = append(hdr, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		hdr = append(hdr, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(len(payload)))
		hdr = append(hdr, ext[:]...)
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func TestMudiLoginAndFetch(t *testing.T) {
	fake := newFakeMudi(t, "mudipw")
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	dev := NewMudiClient(testAddr(ts), false)
	if err := dev.Connect("mudipw"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	s, err := dev.Fetch()
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		// From system.get_status over /rpc.
		{"BatteryPercent", s.BatteryPercent, 88},
		{"BatteryCharging", s.BatteryCharging, true},
		{"CPUTempC", s.CPUTempC, 43},
		{"MCUTempC", s.MCUTempC, 35.5},
		{"UptimeSec", s.UptimeSec, 4532.5},
		{"LoadAvg[0]", s.LoadAvg[0], 1.36},
		{"ConnectedDevices", s.ConnectedDevices, 3},
		// From the WS cellular events (active SIM = slot 1, dial_status 0).
		{"Operator", s.Operator, "Movistar"},
		{"SignalStrength (0-4 strength + 1)", s.SignalStrength, 5},
		{"NetworkType", s.NetworkType, "5G"},
		{"NetworkTypeRaw", s.NetworkTypeRaw, "NR5G-NSA"},
		{"Band", s.Band, 78},
		{"DLBandwidth", s.DLBandwidth, "100MHz"},
		{"RSRP (decimal string)", s.RSRP, -79},
		{"RSRQ (decimal string)", s.RSRQ, -10},
		{"SNR (decimal string)", s.SNR, 12},
		{"WanIP", s.WanIP, "10.112.6.17"},
		{"TotalBytes (decimal string)", s.TotalBytes, float64(234770803)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestMudiWrongPassword(t *testing.T) {
	fake := newFakeMudi(t, "rightpw")
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	dev := NewMudiClient(testAddr(ts), false)
	if err := dev.Connect("wrongpw"); err == nil {
		t.Fatal("Connect with wrong password succeeded; want error")
	}
}
