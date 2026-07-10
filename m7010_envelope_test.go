package main

// End-to-end test of the M7010 AES+RSA envelope against a fake device.
//
// The fake implements the *server* side of the protocol: it hands out a
// real 512-bit RSA key, decrypts the client's sign string, and validates
// the properties the real firmware position-parses — most importantly the
// key=…&iv=…&h=…&s=… insertion order that once cost a debugging session
// (see DEVELOPMENT.md attempt 2). If someone "simplifies" the client back
// to url.Values.Encode(), this test fails the way the hardware would.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// Fixed 512-bit RSA keypair (openssl genrsa 512), e = 0x10001. Small on
// purpose: the real device ships a 512-bit key, which is exactly why the
// client hand-rolls PKCS1v15.
const (
	testRSAModHex = "be57332b69904b9e8674e23c48d76b1b0d5d9389e9fdd85568b4998bf683b3571ed46786c966b9431f71377e30b36ea133ccbde2be18fc34ce7afa51edb8f463"
	testRSAPrvHex = "91f4ee211916f455c0873ac0bd9eaadc18b8ac1d72981c5f0a268b23ffc9e827cdf8ea0857c5f1242498a4fd4fd8bdcfb51fb092f2e51a5dc08810e83ac23261"
)

type fakeM7010 struct {
	t        *testing.T
	password string
	nonce    string
	seqNum   int
	token    string

	n, d *big.Int

	// Captured from the login sign string, used for the whole session.
	aesKey, aesIV []byte

	// Modules seen on web_cgi, for asserting reboot/shutdown actions.
	lastModule string
	lastAction int
}

func newFakeM7010(t *testing.T, password string) *fakeM7010 {
	n, _ := new(big.Int).SetString(testRSAModHex, 16)
	d, _ := new(big.Int).SetString(testRSAPrvHex, 16)
	return &fakeM7010{
		t:        t,
		password: password,
		nonce:    "6vM96Wg2m-G72XLG",
		seqNum:   77321,
		token:    "SSnASCGDZclim6kp",
		n:        n,
		d:        d,
	}
}

// rsaDecrypt undoes the client's chunked PKCS1v15: split into keyLen-byte
// blocks, m = c^d mod n, strip 0x00 0x02 PS 0x00 per block, concatenate.
func (f *fakeM7010) rsaDecrypt(hexCT string) ([]byte, error) {
	ct, err := hex.DecodeString(hexCT)
	if err != nil {
		return nil, err
	}
	keyLen := (f.n.BitLen() + 7) / 8
	if len(ct)%keyLen != 0 {
		return nil, fmt.Errorf("ciphertext length %d not a multiple of key size %d", len(ct), keyLen)
	}
	var out []byte
	for i := 0; i < len(ct); i += keyLen {
		m := new(big.Int).Exp(new(big.Int).SetBytes(ct[i:i+keyLen]), f.d, f.n)
		mb := m.FillBytes(make([]byte, keyLen))
		if mb[0] != 0x00 || mb[1] != 0x02 {
			return nil, fmt.Errorf("block %d: bad PKCS1v15 header % x", i/keyLen, mb[:2])
		}
		sep := bytes.IndexByte(mb[2:], 0x00)
		if sep < 8 { // padding string must be at least 8 nonzero bytes
			return nil, fmt.Errorf("block %d: padding too short (%d)", i/keyLen, sep)
		}
		out = append(out, mb[2+sep+1:]...)
	}
	return out, nil
}

// aesDecryptStrict decrypts and validates every PKCS#7 padding byte —
// stricter than the client's tolerant unpad, so a client-side padding bug
// can't cancel out.
func (f *fakeM7010) aesDecryptStrict(b64 string) ([]byte, error) {
	ct, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d", len(ct))
	}
	block, err := aes.NewCipher(f.aesKey)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, f.aesIV).CryptBlocks(pt, ct)
	padLen := int(pt[len(pt)-1])
	if padLen == 0 || padLen > aes.BlockSize {
		return nil, fmt.Errorf("bad pad length %d", padLen)
	}
	for _, p := range pt[len(pt)-padLen:] {
		if int(p) != padLen {
			return nil, fmt.Errorf("inconsistent PKCS#7 padding")
		}
	}
	return pt[:len(pt)-padLen], nil
}

func (f *fakeM7010) aesEncryptB64(plaintext []byte) string {
	block, _ := aes.NewCipher(f.aesKey)
	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, f.aesIV).CryptBlocks(ct, padded)
	return base64.StdEncoding.EncodeToString(ct)
}

// checkSessionSign validates the post-login "h=…&s=…" sign string. The
// firmware position-parses, so we do too: exactly two fields, this order.
func (f *fakeM7010) checkSessionSign(signStr, dataB64 string) bool {
	parts := strings.Split(signStr, "&")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "h=") || !strings.HasPrefix(parts[1], "s=") {
		f.t.Errorf("session sign string malformed or misordered: %q", signStr)
		return false
	}
	h := md5.Sum([]byte("admin" + f.password))
	if parts[0][2:] != hex.EncodeToString(h[:]) {
		f.t.Errorf("session sign h mismatch: %q", signStr)
		return false
	}
	s, err := strconv.Atoi(parts[1][2:])
	if err != nil || s != f.seqNum+len(dataB64) {
		f.t.Errorf("session sign s = %q, want %d", parts[1][2:], f.seqNum+len(dataB64))
		return false
	}
	return true
}

func (f *fakeM7010) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(authPath, f.handleAuth)
	mux.HandleFunc(webPath, f.handleWeb)
	return mux
}

func (f *fakeM7010) handleAuth(w http.ResponseWriter, r *http.Request) {
	var req struct{ Data, Sign string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("auth_cgi: bad request body: %v", err)
		return
	}

	if req.Sign == "" {
		// Step-1 hello: body carries base64 JSON, response is base64 JSON.
		inner, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			f.t.Errorf("hello data not base64: %v", err)
			return
		}
		var hello struct {
			Module string `json:"module"`
			Action int    `json:"action"`
		}
		if json.Unmarshal(inner, &hello) != nil || hello.Module != moduleAuth {
			f.t.Errorf("unexpected hello payload: %s", inner)
			return
		}
		resp, _ := json.Marshal(map[string]any{
			"nonce":     f.nonce,
			"rsaPubKey": "010001",
			"rsaMod":    testRSAModHex,
			"seqNum":    f.seqNum,
			"authedIP":  "0.0.0.0",
			"result":    1, // the real device reports 1 with no session; fields still valid
		})
		w.Write([]byte(base64.StdEncoding.EncodeToString(resp)))
		return
	}

	signBytes, err := f.rsaDecrypt(req.Sign)
	if err != nil {
		f.t.Errorf("sign RSA decrypt failed: %v", err)
		return
	}
	signStr := string(signBytes)

	if strings.HasPrefix(signStr, "key=") {
		f.handleLogin(w, signStr, req.Data)
		return
	}
	// Post-login call on auth_cgi (logout). Validate and ack.
	if !f.checkSessionSign(signStr, req.Data) {
		return
	}
	resp, _ := json.Marshal(map[string]any{"result": 0})
	w.Write([]byte(f.aesEncryptB64(resp)))
}

func (f *fakeM7010) handleLogin(w http.ResponseWriter, signStr, dataB64 string) {
	// The order key,iv,h,s is load-bearing — validate positionally, like
	// the firmware does. Alphabetical (h,iv,key,s) must be rejected.
	parts := strings.Split(signStr, "&")
	if len(parts) != 4 ||
		!strings.HasPrefix(parts[0], "key=") ||
		!strings.HasPrefix(parts[1], "iv=") ||
		!strings.HasPrefix(parts[2], "h=") ||
		!strings.HasPrefix(parts[3], "s=") {
		f.t.Errorf("login sign string misordered (want key,iv,h,s): %q", signStr)
		return
	}
	key, iv := parts[0][4:], parts[1][3:]
	if len(key) != 16 || len(iv) != 16 {
		f.t.Errorf("AES key/iv length: key=%d iv=%d", len(key), len(iv))
		return
	}
	s, err := strconv.Atoi(parts[3][2:])
	if err != nil || s != f.seqNum+len(dataB64) {
		f.t.Errorf("login sign s = %q, want %d", parts[3][2:], f.seqNum+len(dataB64))
		return
	}
	f.aesKey, f.aesIV = []byte(key), []byte(iv)

	// A wrong password shows up here as an h mismatch — that's an auth
	// failure (respond like the firmware does), not a client bug.
	h := md5.Sum([]byte("admin" + f.password))
	if parts[2][2:] != hex.EncodeToString(h[:]) {
		resp, _ := json.Marshal(map[string]any{"result": 1})
		w.Write([]byte(f.aesEncryptB64(resp)))
		return
	}

	inner, err := f.aesDecryptStrict(dataB64)
	if err != nil {
		f.t.Errorf("login data AES decrypt: %v", err)
		return
	}
	var login struct {
		Module string `json:"module"`
		Action int    `json:"action"`
		Digest string `json:"digest"`
	}
	if json.Unmarshal(inner, &login) != nil || login.Module != moduleAuth || login.Action != actionLogin {
		f.t.Errorf("unexpected login payload: %s", inner)
		return
	}
	digest := md5.Sum([]byte(f.password + ":" + f.nonce))
	var resp []byte
	if login.Digest == hex.EncodeToString(digest[:]) {
		resp, _ = json.Marshal(map[string]any{"token": f.token, "result": 0})
	} else {
		resp, _ = json.Marshal(map[string]any{"result": 1}) // DontMatch
	}
	w.Write([]byte(f.aesEncryptB64(resp)))
}

func (f *fakeM7010) handleWeb(w http.ResponseWriter, r *http.Request) {
	var req struct{ Data, Sign string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("web_cgi: bad request body: %v", err)
		return
	}
	signBytes, err := f.rsaDecrypt(req.Sign)
	if err != nil {
		f.t.Errorf("web_cgi sign RSA decrypt: %v", err)
		return
	}
	// Post-login signs must have dropped key/iv.
	if strings.Contains(string(signBytes), "key=") {
		f.t.Errorf("web_cgi sign still carries key/iv: %q", signBytes)
		return
	}
	if !f.checkSessionSign(string(signBytes), req.Data) {
		return
	}
	inner, err := f.aesDecryptStrict(req.Data)
	if err != nil {
		f.t.Errorf("web_cgi data AES decrypt: %v", err)
		return
	}
	var call struct {
		Module string `json:"module"`
		Action int    `json:"action"`
		Token  string `json:"token"`
	}
	if json.Unmarshal(inner, &call) != nil {
		f.t.Errorf("web_cgi payload not JSON: %s", inner)
		return
	}
	if call.Token != f.token {
		f.t.Errorf("web_cgi token = %q, want %q", call.Token, f.token)
		return
	}
	f.lastModule, f.lastAction = call.Module, call.Action

	var resp string
	switch call.Module {
	case moduleStatus:
		resp = `{
		  "deviceInfo": {"model":"M7010","firmwareVer":"3.0.3 Build 250814 Rel.1021n"},
		  "battery":    {"connected":true,"charging":true,"voltage":93},
		  "wan": {
		    "connectStatus":4,"ipv4":"5.205.144.6","networkType":3,
		    "signalStrength":0,
		    "totalStatistics":"14473800628.000000","dailyStatistics":"42744727.000000",
		    "txSpeed":"2964","rxSpeed":"2202",
		    "operatorName":"Movistar","band":20,
		    "rsrp":-85,"rsrq":-12,"rssi":-60,"snr":13
		  },
		  "connectedDevices": {"number":2},
		  "result": 0
		}`
	case moduleFlowStat:
		resp = `{
		  "settings": {
		    "enableDataLimit": true,
		    "limitation": "107374182400.000000",
		    "paymentDay": 1,
		    "adjustStatistics": "14473800628.000000",
		    "warningPercent": 90
		  },
		  "result": 0
		}`
	case moduleReboot:
		resp = `{"result":0}`
	default:
		f.t.Errorf("web_cgi unexpected module %q", call.Module)
		resp = `{"result":1}`
	}
	w.Write([]byte(f.aesEncryptB64([]byte(resp))))
}

func TestM7010LoginAndFetch(t *testing.T) {
	fake := newFakeM7010(t, "secretpw")
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	c := NewClient(testAddr(ts), false)
	if err := c.Login("secretpw"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	s, err := c.Fetch()
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Model", s.Model, "M7010"},
		{"NetworkType", s.NetworkType, "4G"},
		{"SignalStrength (derived from RSRP)", s.SignalStrength, 4},
		{"RSRP", s.RSRP, -85},
		{"Operator", s.Operator, "Movistar"},
		{"Band", s.Band, 20},
		{"WanIP", s.WanIP, "5.205.144.6"},
		{"BatteryPercent (from 'voltage')", s.BatteryPercent, 93},
		{"BatteryCharging", s.BatteryCharging, true},
		{"TotalBytes", s.TotalBytes, float64(14473800628)},
		{"DailyBytes", s.DailyBytes, float64(42744727)},
		{"MonthLimitBytes", s.MonthLimitBytes, float64(107374182400)},
		{"PaymentDay", s.PaymentDay, 1},
		{"ConnectedDevices", s.ConnectedDevices, 2},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestM7010WrongPassword(t *testing.T) {
	fake := newFakeM7010(t, "rightpw")
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	c := NewClient(testAddr(ts), false)
	err := c.Login("wrongpw")
	if err == nil || !strings.Contains(err.Error(), "login failed") {
		t.Fatalf("Login with wrong password: err = %v, want 'login failed'", err)
	}
}

func TestM7010RebootAndShutdownActions(t *testing.T) {
	fake := newFakeM7010(t, "secretpw")
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()

	c := NewClient(testAddr(ts), false)
	if err := c.Login("secretpw"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := c.Reboot(); err != nil {
		t.Errorf("Reboot: %v", err)
	}
	if fake.lastModule != moduleReboot || fake.lastAction != actionReboot {
		t.Errorf("Reboot sent (%s, %d), want (%s, %d)",
			fake.lastModule, fake.lastAction, moduleReboot, actionReboot)
	}
	if err := c.Shutdown(); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if fake.lastModule != moduleReboot || fake.lastAction != actionShutdown {
		t.Errorf("Shutdown sent (%s, %d), want (%s, %d)",
			fake.lastModule, fake.lastAction, moduleReboot, actionShutdown)
	}
}
