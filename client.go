package main

// M7010 web API client.
//
// Transport is a two-step envelope:
//
//  1. POST /cgi-bin/auth_cgi with {"data": base64(json)} — returns nonce,
//     RSA pubkey/modulus (hex) and seqNum (base64-wrapped JSON).
//  2. For login + every later call, send {"data": aes_b64, "sign": rsa_hex}
//     where the sign string is a manually-ordered "key=…&iv=…&h=…&s=…"
//     blob (alphabetical ordering breaks the server). Responses are the
//     base64 of AES-CBC ciphertext, decrypt with the same key/iv we
//     generated client-side.
//
// Gotchas: Go's stdlib RSA refuses the 512-bit modulus the M7010 uses, so
// we implement PKCS1v15 (with chunking) against math/big ourselves. See
// PROTOCOL.md for the full picture.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAddr    = "192.168.0.1"
	defaultTimeout = 5 * time.Second

	authPath = "/cgi-bin/auth_cgi"
	webPath  = "/cgi-bin/web_cgi"

	moduleAuth     = "authenticator"
	moduleStatus   = "status"
	moduleFlowStat = "flowstat"
	moduleReboot   = "reboot"

	actionLoad     = 0
	actionGet      = 0
	actionLogin    = 1
	actionLogout   = 3
	actionReboot   = 0
	actionShutdown = 1
)

type Client struct {
	baseURL    string
	password   string
	token      string
	aesKey     []byte
	aesIV      []byte
	rsaMod     *big.Int
	rsaPubKey  *big.Int
	seqNum     int
	httpClient *http.Client
	debug      bool
}

func NewClient(addr string, debug bool) *Client {
	if addr == "" {
		addr = defaultAddr
	}
	return &Client{
		baseURL:    "http://" + addr,
		debug:      debug,
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
}

// Device interface shims around the M7010 methods below.

func (c *Client) Name() string                  { return "TP-Link M7010" }
func (c *Client) Connect(password string) error { return c.Login(password) }
func (c *Client) Close()                        { c.Logout() }

func (c *Client) Fetch() (*Status, error) {
	s, err := c.GetStatus()
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	if err := c.GetFlowStats(s); err != nil {
		return nil, fmt.Errorf("flowstat: %w", err)
	}
	return s, nil
}

var _ Device = (*Client)(nil)

// probeM7010 checks whether addr actually speaks the M7010 auth protocol,
// not just whether something answers TCP on port 80. It sends the
// unauthenticated step-1 hello and looks for the nonce in the response.
// This is what protects autodetect from the fact that 192.168.0.1 is also
// the most common home-router gateway IP on the planet.
func probeM7010(addr string, timeout time.Duration) bool {
	payload, _ := json.Marshal(map[string]any{
		"module": moduleAuth,
		"action": actionLoad,
	})
	reqBody, _ := json.Marshal(map[string]string{
		"data": base64.StdEncoding.EncodeToString(payload),
	})
	client := &http.Client{Timeout: timeout}
	resp, err := client.Post("http://"+addr+authPath, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return false
	}
	m, err := decodeBase64JSON(data)
	if err != nil {
		return false
	}
	nonce, _ := m["nonce"].(string)
	return nonce != ""
}

func (c *Client) debugf(format string, args ...any) {
	if c.debug {
		fmt.Printf("[DEBUG] "+format, args...)
	}
}

// postRaw sends a raw POST and returns the raw response body.
func (c *Client) postRaw(endpoint string, body []byte) ([]byte, error) {
	c.debugf("POST %s body=%s\n", endpoint, string(body))

	resp, err := c.httpClient.Post(c.baseURL+endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	c.debugf("Response raw: %s\n", string(data))
	return data, nil
}

func decodeBase64JSON(data []byte) (map[string]any, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}

	var result map[string]any
	if err := json.Unmarshal(decoded, &result); err != nil {
		return nil, fmt.Errorf("json unmarshal (%s): %w", string(decoded), err)
	}
	return result, nil
}

// Login authenticates with the modem using the AES+RSA encrypted protocol.
func (c *Client) Login(password string) error {
	c.password = password

	// Step 1: request nonce + RSA keys. Body is {"data": base64(json)}.
	payload, _ := json.Marshal(map[string]any{
		"module": moduleAuth,
		"action": actionLoad,
	})
	reqBody, _ := json.Marshal(map[string]string{
		"data": base64.StdEncoding.EncodeToString(payload),
	})

	respData, err := c.postRaw(authPath, reqBody)
	if err != nil {
		return fmt.Errorf("request nonce: %w", err)
	}

	nonceResp, err := decodeBase64JSON(respData)
	if err != nil {
		return fmt.Errorf("decode nonce response: %w", err)
	}

	c.debugf("Nonce response: %v\n", nonceResp)

	nonce, _ := nonceResp["nonce"].(string)
	rsaPubKeyHex, _ := nonceResp["rsaPubKey"].(string)
	rsaModHex, _ := nonceResp["rsaMod"].(string)

	if nonce == "" || rsaModHex == "" || rsaPubKeyHex == "" {
		return fmt.Errorf("incomplete nonce response (nonce=%q, rsaMod=%q, rsaPubKey=%q): %v",
			nonce, rsaModHex, rsaPubKeyHex, nonceResp)
	}

	// Validate the hex here: a garbage modulus would otherwise leave keyLen
	// at 0 and send rsaEncrypt's chunking loop backwards forever.
	var ok bool
	c.rsaMod, ok = new(big.Int).SetString(rsaModHex, 16)
	if !ok || c.rsaMod.Sign() <= 0 {
		return fmt.Errorf("invalid rsaMod %q in nonce response", rsaModHex)
	}
	c.rsaPubKey, ok = new(big.Int).SetString(rsaPubKeyHex, 16)
	if !ok || c.rsaPubKey.Sign() <= 0 {
		return fmt.Errorf("invalid rsaPubKey %q in nonce response", rsaPubKeyHex)
	}

	c.seqNum = intFrom(nonceResp["seqNum"])

	// Step 2: generate random AES key/IV (ASCII digits — the PHP reference
	// implementation does the same and some firmware paths may not handle
	// high bytes cleanly).
	c.aesKey = make([]byte, 16)
	c.aesIV = make([]byte, 16)
	rand.Read(c.aesKey)
	rand.Read(c.aesIV)
	for i := range c.aesKey {
		c.aesKey[i] = '0' + (c.aesKey[i] % 10)
	}
	for i := range c.aesIV {
		c.aesIV[i] = '0' + (c.aesIV[i] % 10)
	}

	// Step 3: digest = md5(password + ":" + nonce). The colon matters.
	digestHash := md5.Sum([]byte(password + ":" + nonce))
	digest := hex.EncodeToString(digestHash[:])

	loginPayload, _ := json.Marshal(map[string]any{
		"module": moduleAuth,
		"action": actionLogin,
		"digest": digest,
	})

	encryptedData, err := c.aesEncrypt(loginPayload)
	if err != nil {
		return fmt.Errorf("aes encrypt login: %w", err)
	}

	// Sign string order is load-bearing: the firmware position-parses it, so
	// url.Values.Encode() (alphabetical) returns garbage. Build it manually.
	h := md5.Sum([]byte("admin" + password))
	s := c.seqNum + len(encryptedData)
	signStr := fmt.Sprintf("key=%s&iv=%s&h=%s&s=%d",
		url.QueryEscape(string(c.aesKey)),
		url.QueryEscape(string(c.aesIV)),
		hex.EncodeToString(h[:]), s)

	c.debugf("Sign plaintext: %s\n", signStr)
	c.debugf("AES key=%s iv=%s\n", string(c.aesKey), string(c.aesIV))

	sign, err := c.rsaEncrypt([]byte(signStr))
	if err != nil {
		return fmt.Errorf("rsa encrypt sign: %w", err)
	}

	authReqBody, _ := json.Marshal(map[string]string{
		"data": encryptedData,
		"sign": sign,
	})

	authRespData, err := c.postRaw(authPath, authReqBody)
	if err != nil {
		return fmt.Errorf("auth request: %w", err)
	}

	authJSON, err := c.aesDecrypt(strings.TrimSpace(string(authRespData)))
	if err != nil {
		return fmt.Errorf("decrypt auth response: %w", err)
	}

	c.debugf("Auth response decrypted: %s\n", string(authJSON))

	var authResp map[string]any
	if err := json.Unmarshal(authJSON, &authResp); err != nil {
		return fmt.Errorf("parse auth response: %w", err)
	}

	token, _ := authResp["token"].(string)
	if token == "" {
		if code, ok := authResp["result"].(float64); ok && code != 0 {
			return fmt.Errorf("login failed (code %.0f): wrong password?", code)
		}
		return fmt.Errorf("no token in auth response: %v", authResp)
	}

	c.token = token
	return nil
}

// Shutdown powers the modem off. Requires an active session.
func (c *Client) Shutdown() error {
	return c.rebootAction(actionShutdown)
}

// Reboot restarts the modem. Requires an active session.
func (c *Client) Reboot() error {
	return c.rebootAction(actionReboot)
}

func (c *Client) rebootAction(action int) error {
	if c.token == "" {
		return fmt.Errorf("not logged in")
	}
	// Failing to even build the envelope is a real error and must not be
	// swallowed — nothing was sent, so "command sent" would be a lie.
	body, err := c.buildEnvelope(map[string]any{
		"module": moduleReboot,
		"action": action,
		"token":  c.token,
	})
	if err != nil {
		return err
	}
	// From here on the modem often kills the connection before responding;
	// a transport error or an undecryptable tail is the normal outcome.
	respData, err := c.postRaw(webPath, body)
	if err != nil {
		return nil
	}
	decrypted, err := c.aesDecrypt(strings.TrimSpace(string(respData)))
	if err != nil {
		return nil
	}
	var resp map[string]any
	if json.Unmarshal(decrypted, &resp) != nil {
		return nil
	}
	if code, ok := resp["result"].(float64); ok && code != 0 {
		return fmt.Errorf("reboot/shutdown failed (code %.0f)", code)
	}
	return nil
}

// Logout is best-effort — the modem ages out tokens on its own if we don't.
func (c *Client) Logout() {
	if c.token == "" {
		return
	}
	c.encryptedRequest(authPath, map[string]any{
		"module": moduleAuth,
		"action": actionLogout,
		"token":  c.token,
	})
	c.token = ""
}

// buildEnvelope AES-encrypts the payload and RSA-signs it into the
// {data, sign} request body every post-login call uses.
func (c *Client) buildEnvelope(payload map[string]any) ([]byte, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	encryptedData, err := c.aesEncrypt(payloadJSON)
	if err != nil {
		return nil, fmt.Errorf("aes encrypt: %w", err)
	}

	h := md5.Sum([]byte("admin" + c.password))
	s := c.seqNum + len(encryptedData)
	signStr := fmt.Sprintf("h=%s&s=%d", hex.EncodeToString(h[:]), s)

	sign, err := c.rsaEncrypt([]byte(signStr))
	if err != nil {
		return nil, fmt.Errorf("rsa encrypt: %w", err)
	}

	reqBody, _ := json.Marshal(map[string]string{
		"data": encryptedData,
		"sign": sign,
	})
	return reqBody, nil
}

func (c *Client) encryptedRequest(endpoint string, payload map[string]any) (map[string]any, error) {
	reqBody, err := c.buildEnvelope(payload)
	if err != nil {
		return nil, err
	}

	respData, err := c.postRaw(endpoint, reqBody)
	if err != nil {
		return nil, err
	}

	decrypted, err := c.aesDecrypt(strings.TrimSpace(string(respData)))
	if err != nil {
		return nil, fmt.Errorf("decrypt response: %w", err)
	}

	c.debugf("Decrypted response: %s\n", string(decrypted))

	var result map[string]any
	if err := json.Unmarshal(decrypted, &result); err != nil {
		return nil, fmt.Errorf("parse response (%s): %w", string(decrypted), err)
	}
	return result, nil
}

// --- AES-128-CBC ---

func pkcs7Pad(b []byte, blockSize int) []byte {
	padLen := blockSize - (len(b) % blockSize)
	padded := make([]byte, len(b)+padLen)
	copy(padded, b)
	for i := len(b); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	return padded
}

func pkcs7Unpad(b []byte, blockSize int) []byte {
	if len(b) == 0 {
		return b
	}
	padLen := int(b[len(b)-1])
	if padLen <= 0 || padLen > blockSize {
		return b
	}
	return b[:len(b)-padLen]
}

func (c *Client) aesEncrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, c.aesIV).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (c *Client) aesDecrypt(ciphertextB64 string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode ciphertext: %w", err)
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d not multiple of block size", len(ciphertext))
	}
	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return nil, err
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, c.aesIV).CryptBlocks(plaintext, ciphertext)
	return pkcs7Unpad(plaintext, aes.BlockSize), nil
}

// --- RSA PKCS1v15 ---
//
// Hand-rolled because Go's crypto/rsa refuses keys <1024 bits (and
// GODEBUG=rsa1024min=0 only relaxes the 1024 floor, not below it) — the
// M7010 ships a 512-bit key. phpseclib auto-chunks messages longer than
// keyLen-11, so we mirror that.

func (c *Client) rsaEncrypt(plaintext []byte) (string, error) {
	n := c.rsaMod
	e := c.rsaPubKey
	keyLen := (n.BitLen() + 7) / 8
	chunkSize := keyLen - 11
	if chunkSize <= 0 {
		return "", fmt.Errorf("RSA modulus too small (%d bits)", n.BitLen())
	}

	var result []byte
	for i := 0; i < len(plaintext); i += chunkSize {
		end := i + chunkSize
		if end > len(plaintext) {
			end = len(plaintext)
		}
		encrypted, err := rsaEncryptBlock(plaintext[i:end], n, e, keyLen)
		if err != nil {
			return "", err
		}
		result = append(result, encrypted...)
	}

	return hex.EncodeToString(result), nil
}

func rsaEncryptBlock(msg []byte, n, e *big.Int, keyLen int) ([]byte, error) {
	// PKCS1v15: 0x00 || 0x02 || random_nonzero_padding || 0x00 || message
	padded := make([]byte, keyLen)
	padded[1] = 0x02
	psLen := keyLen - len(msg) - 3
	ps := padded[2 : 2+psLen]
	for i := range ps {
		for {
			rand.Read(ps[i : i+1])
			if ps[i] != 0 {
				break
			}
		}
	}
	copy(padded[3+psLen:], msg)

	m := new(big.Int).SetBytes(padded)
	ct := new(big.Int).Exp(m, e, n)

	ctBytes := ct.Bytes()
	if len(ctBytes) < keyLen {
		out := make([]byte, keyLen)
		copy(out[keyLen-len(ctBytes):], ctBytes)
		ctBytes = out
	}
	return ctBytes, nil
}

// --- Status ---

// Status is the shared data model both device clients fill. The JSON tags
// define the stable `--json` scripting output; the raw maps are excluded
// there (use --raw for those).
type Status struct {
	Model    string `json:"model,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	Operator string `json:"operator,omitempty"`

	NetworkType    string `json:"network_type,omitempty"`     // short label like "5G", "4G+"
	NetworkTypeRaw string `json:"network_type_raw,omitempty"` // technical mode from the modem (e.g. "LTE FDD", "NR5G-NSA"); empty for M7010
	Band           int    `json:"band,omitempty"`
	DLBandwidth    string `json:"dl_bandwidth,omitempty"` // channel bandwidth label, e.g. "100MHz"; Mudi-only
	ConnectStatus  int    `json:"connect_status,omitempty"`
	SignalStrength int    `json:"signal_strength"` // 0-5; firmware reports 0, we derive from RSRP
	RSRP           int    `json:"rsrp,omitempty"`  // dBm
	RSRQ           int    `json:"rsrq,omitempty"`
	RSSI           int    `json:"rssi,omitempty"`
	SNR            int    `json:"snr,omitempty"`
	WanIP          string `json:"wan_ip,omitempty"`

	BatteryPercent  int  `json:"battery_percent"`
	BatteryCharging bool `json:"battery_charging"`

	// Mudi-only host metrics (populated from system.get_status.system).
	UptimeSec float64    `json:"uptime_sec,omitempty"`
	CPUTempC  int        `json:"cpu_temp_c,omitempty"`
	MCUTempC  float64    `json:"mcu_temp_c,omitempty"`
	LoadAvg   [3]float64 `json:"load_avg"` // 1m / 5m / 15m

	TxSpeed string `json:"tx_speed,omitempty"`
	RxSpeed string `json:"rx_speed,omitempty"`

	TotalBytes       float64 `json:"total_bytes"`
	DailyBytes       float64 `json:"daily_bytes,omitempty"`
	AdjustedBytes    float64 `json:"adjusted_bytes,omitempty"`
	MonthLimitBytes  float64 `json:"month_limit_bytes,omitempty"`
	PaymentDay       int     `json:"payment_day,omitempty"`
	ConnectedDevices int     `json:"connected_devices,omitempty"`

	RawStatus   map[string]any `json:"-"`
	RawFlowStat map[string]any `json:"-"`
}

func networkTypeStr(t int) string {
	switch t {
	case 0:
		return "No Service"
	case 1:
		return "2G"
	case 2:
		return "3G"
	case 3:
		return "4G"
	case 4:
		return "3G (TD-SCDMA)"
	case 5:
		return "2G (CDMA1x)"
	case 6:
		return "3G (CDMA EV-DO)"
	case 7:
		return "4G+"
	default:
		return fmt.Sprintf("Unknown(%d)", t)
	}
}

func rsrpToSignal(rsrp int) int {
	switch {
	case rsrp >= -80:
		return 5
	case rsrp >= -90:
		return 4
	case rsrp >= -100:
		return 3
	case rsrp >= -110:
		return 2
	case rsrp >= -120:
		return 1
	default:
		return 0
	}
}

func (c *Client) GetStatus() (*Status, error) {
	if c.token == "" {
		return nil, fmt.Errorf("not logged in")
	}

	resp, err := c.encryptedRequest(webPath, map[string]any{
		"module": moduleStatus,
		"action": actionGet,
		"token":  c.token,
	})
	if err != nil {
		return nil, fmt.Errorf("get status: %w", err)
	}

	s := &Status{RawStatus: resp}
	parseStatus(s, resp)
	return s, nil
}

func parseStatus(s *Status, resp map[string]any) {
	if di, ok := resp["deviceInfo"].(map[string]any); ok {
		s.Model = jsonStr(di, "model")
		s.Firmware = jsonStr(di, "firmwareVer")
	}

	if wan, ok := resp["wan"].(map[string]any); ok {
		s.NetworkType = networkTypeStr(jsonInt(wan, "networkType"))
		s.Band = jsonInt(wan, "band")
		s.ConnectStatus = jsonInt(wan, "connectStatus")
		s.SignalStrength = jsonInt(wan, "signalStrength")
		s.RSRP = jsonInt(wan, "rsrp")
		s.RSRQ = jsonInt(wan, "rsrq")
		s.RSSI = jsonInt(wan, "rssi")
		s.SNR = jsonInt(wan, "snr")
		s.WanIP = jsonStr(wan, "ipv4")
		s.Operator = jsonStr(wan, "operatorName")
		s.TxSpeed = jsonStr(wan, "txSpeed")
		s.RxSpeed = jsonStr(wan, "rxSpeed")
		s.TotalBytes = jsonFloatStr(wan, "totalStatistics")
		s.DailyBytes = jsonFloatStr(wan, "dailyStatistics")

		if s.SignalStrength == 0 && s.RSRP != 0 {
			s.SignalStrength = rsrpToSignal(s.RSRP)
		}
	}

	if bat, ok := resp["battery"].(map[string]any); ok {
		s.BatteryPercent = jsonInt(bat, "voltage") // the "voltage" field actually holds a percent
		s.BatteryCharging = jsonBool(bat, "charging")
	}

	if cd, ok := resp["connectedDevices"].(map[string]any); ok {
		s.ConnectedDevices = jsonInt(cd, "number")
	}
}

func (c *Client) GetFlowStats(s *Status) error {
	if c.token == "" {
		return fmt.Errorf("not logged in")
	}

	resp, err := c.encryptedRequest(webPath, map[string]any{
		"module": moduleFlowStat,
		"action": actionGet,
		"token":  c.token,
	})
	if err != nil {
		return fmt.Errorf("get flow stats: %w", err)
	}

	s.RawFlowStat = resp

	if settings, ok := resp["settings"].(map[string]any); ok {
		s.AdjustedBytes = jsonFloatStr(settings, "adjustStatistics")
		s.PaymentDay = jsonInt(settings, "paymentDay")

		limitStr := jsonStr(settings, "limitation")
		if limitStr != "" && limitStr != "0.000000" {
			s.MonthLimitBytes, _ = strconv.ParseFloat(limitStr, 64)
		}
	}

	return nil
}

// --- JSON helpers ---

func jsonStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func jsonInt(m map[string]any, key string) int {
	return intFrom(m[key])
}

func jsonBool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// jsonFloatStr reads a value stored as a decimal string like
// "14473800628.000000", falling back to a JSON number if present.
func jsonFloatStr(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	case float64:
		return v
	}
	return 0
}

// intFrom handles the two ways a JSON integer may arrive in a
// map[string]any: as float64 (the default) or as a numeric string.
func intFrom(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}
