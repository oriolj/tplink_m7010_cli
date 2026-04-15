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
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAddr    = "192.168.0.1"
	defaultTimeout = 5 * time.Second
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
		baseURL: "http://" + addr,
		debug:   debug,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// postRaw sends a raw POST and returns the raw response body.
func (c *Client) postRaw(endpoint string, body []byte) ([]byte, error) {
	if c.debug {
		fmt.Printf("[DEBUG] POST %s body=%s\n", endpoint, string(body))
	}

	resp, err := c.httpClient.Post(c.baseURL+endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if c.debug {
		fmt.Printf("[DEBUG] Response raw: %s\n", string(data))
	}

	return data, nil
}

// decodeBase64JSON decodes a base64-encoded JSON response.
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

	// Step 1: Request nonce + RSA keys.
	// The M7010 expects {"data": base64(json_payload)}.
	payload, _ := json.Marshal(map[string]any{
		"module": "authenticator",
		"action": 0,
	})
	reqBody, _ := json.Marshal(map[string]string{
		"data": base64.StdEncoding.EncodeToString(payload),
	})

	respData, err := c.postRaw("/cgi-bin/auth_cgi", reqBody)
	if err != nil {
		return fmt.Errorf("request nonce: %w", err)
	}

	nonceResp, err := decodeBase64JSON(respData)
	if err != nil {
		return fmt.Errorf("decode nonce response: %w", err)
	}

	if c.debug {
		fmt.Printf("[DEBUG] Nonce response: %v\n", nonceResp)
	}

	nonce, _ := nonceResp["nonce"].(string)
	rsaPubKeyHex, _ := nonceResp["rsaPubKey"].(string)
	rsaModHex, _ := nonceResp["rsaMod"].(string)
	seqNumStr, _ := nonceResp["seqNum"].(string)

	if nonce == "" || rsaModHex == "" || rsaPubKeyHex == "" {
		return fmt.Errorf("incomplete nonce response (nonce=%q, rsaMod=%q, rsaPubKey=%q): %v",
			nonce, rsaModHex, rsaPubKeyHex, nonceResp)
	}

	// Parse RSA keys from hex
	c.rsaMod = new(big.Int)
	c.rsaMod.SetString(rsaModHex, 16)
	c.rsaPubKey = new(big.Int)
	c.rsaPubKey.SetString(rsaPubKeyHex, 16)

	// Parse sequence number (comes as float64 from JSON or as string)
	if seqNumStr != "" {
		fmt.Sscanf(seqNumStr, "%d", &c.seqNum)
	}
	if c.seqNum == 0 {
		// Might be a float64 in the map directly
		if sn, ok := nonceResp["seqNum"].(float64); ok {
			c.seqNum = int(sn)
		}
	}

	// Step 2: Generate random AES key and IV (16 bytes each)
	c.aesKey = make([]byte, 16)
	c.aesIV = make([]byte, 16)
	rand.Read(c.aesKey)
	rand.Read(c.aesIV)
	// Use only printable ASCII chars for compatibility (like the PHP impl uses digits)
	for i := range c.aesKey {
		c.aesKey[i] = '0' + (c.aesKey[i] % 10)
	}
	for i := range c.aesIV {
		c.aesIV[i] = '0' + (c.aesIV[i] % 10)
	}

	// Step 3: Compute digest = md5(password + ":" + nonce)
	digestHash := md5.Sum([]byte(password + ":" + nonce))
	digest := fmt.Sprintf("%x", digestHash)

	// Step 4: AES encrypt the login payload
	loginPayload, _ := json.Marshal(map[string]any{
		"module": "authenticator",
		"action": 1,
		"digest": digest,
	})

	encryptedData, err := c.aesEncrypt(loginPayload)
	if err != nil {
		return fmt.Errorf("aes encrypt login: %w", err)
	}

	// Step 5: Build and RSA encrypt the signature
	// Must match PHP's http_build_query insertion order: key, iv, h, s
	h := md5.Sum([]byte("admin" + password))
	s := c.seqNum + len(encryptedData)

	signStr := fmt.Sprintf("key=%s&iv=%s&h=%x&s=%d",
		url.QueryEscape(string(c.aesKey)),
		url.QueryEscape(string(c.aesIV)),
		h, s)

	if c.debug {
		fmt.Printf("[DEBUG] Sign plaintext: %s\n", signStr)
		fmt.Printf("[DEBUG] AES key=%s iv=%s\n", string(c.aesKey), string(c.aesIV))
	}

	sign, err := c.rsaEncrypt([]byte(signStr))
	if err != nil {
		return fmt.Errorf("rsa encrypt sign: %w", err)
	}

	// Step 6: Send encrypted auth request
	authReqBody, _ := json.Marshal(map[string]string{
		"data": encryptedData,
		"sign": sign,
	})

	authRespData, err := c.postRaw("/cgi-bin/auth_cgi", authReqBody)
	if err != nil {
		return fmt.Errorf("auth request: %w", err)
	}

	// Response is AES encrypted
	authJSON, err := c.aesDecrypt(strings.TrimSpace(string(authRespData)))
	if err != nil {
		return fmt.Errorf("decrypt auth response: %w", err)
	}

	if c.debug {
		fmt.Printf("[DEBUG] Auth response decrypted: %s\n", string(authJSON))
	}

	var authResp map[string]any
	if err := json.Unmarshal(authJSON, &authResp); err != nil {
		return fmt.Errorf("parse auth response: %w", err)
	}

	token, ok := authResp["token"].(string)
	if !ok || token == "" {
		if code, ok := authResp["result"].(float64); ok && code != 0 {
			return fmt.Errorf("login failed (code %.0f): wrong password?", code)
		}
		return fmt.Errorf("no token in auth response: %v", authResp)
	}

	c.token = token
	return nil
}

func (c *Client) Logout() {
	if c.token == "" {
		return
	}
	c.encryptedRequest("/cgi-bin/auth_cgi", map[string]any{
		"module": "authenticator",
		"action": 3,
		"token":  c.token,
	})
	c.token = ""
}

// encryptedRequest sends an AES+RSA encrypted request and returns the decrypted JSON response.
func (c *Client) encryptedRequest(endpoint string, payload map[string]any) (map[string]any, error) {
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

	signStr := fmt.Sprintf("h=%x&s=%d", h, s)

	sign, err := c.rsaEncrypt([]byte(signStr))
	if err != nil {
		return nil, fmt.Errorf("rsa encrypt: %w", err)
	}

	reqBody, _ := json.Marshal(map[string]string{
		"data": encryptedData,
		"sign": sign,
	})

	respData, err := c.postRaw(endpoint, reqBody)
	if err != nil {
		return nil, err
	}

	decrypted, err := c.aesDecrypt(strings.TrimSpace(string(respData)))
	if err != nil {
		return nil, fmt.Errorf("decrypt response: %w", err)
	}

	if c.debug {
		fmt.Printf("[DEBUG] Decrypted response: %s\n", string(decrypted))
	}

	var result map[string]any
	if err := json.Unmarshal(decrypted, &result); err != nil {
		return nil, fmt.Errorf("parse response (%s): %w", string(decrypted), err)
	}
	return result, nil
}

// --- AES-128-CBC ---

func (c *Client) aesEncrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return "", err
	}

	// PKCS7 padding
	padLen := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}

	ciphertext := make([]byte, len(padded))
	mode := cipher.NewCBCEncrypter(block, c.aesIV)
	mode.CryptBlocks(ciphertext, padded)

	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (c *Client) aesDecrypt(ciphertextB64 string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode ciphertext: %w", err)
	}

	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return nil, err
	}

	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d not multiple of block size", len(ciphertext))
	}

	plaintext := make([]byte, len(ciphertext))
	mode := cipher.NewCBCDecrypter(block, c.aesIV)
	mode.CryptBlocks(plaintext, ciphertext)

	// Remove PKCS7 padding
	if len(plaintext) > 0 {
		padLen := int(plaintext[len(plaintext)-1])
		if padLen > 0 && padLen <= aes.BlockSize {
			plaintext = plaintext[:len(plaintext)-padLen]
		}
	}

	return plaintext, nil
}

// --- RSA PKCS1v15 (manual, the M7010 uses a 512-bit key which Go rejects) ---
// phpseclib auto-chunks messages longer than keyLen-11, so we do the same.

func (c *Client) rsaEncrypt(plaintext []byte) (string, error) {
	n := c.rsaMod
	e := c.rsaPubKey
	keyLen := (n.BitLen() + 7) / 8
	chunkSize := keyLen - 11 // max plaintext per PKCS1v15 block

	var result []byte
	for i := 0; i < len(plaintext); i += chunkSize {
		end := i + chunkSize
		if end > len(plaintext) {
			end = len(plaintext)
		}
		chunk := plaintext[i:end]

		encrypted, err := rsaEncryptBlock(chunk, n, e, keyLen)
		if err != nil {
			return "", err
		}
		result = append(result, encrypted...)
	}

	return fmt.Sprintf("%x", result), nil
}

func rsaEncryptBlock(msg []byte, n, e *big.Int, keyLen int) ([]byte, error) {
	// PKCS1v15: 0x00 || 0x02 || random_nonzero_padding || 0x00 || message
	padded := make([]byte, keyLen)
	padded[0] = 0x00
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
	padded[2+psLen] = 0x00
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

type Status struct {
	// Device
	Model    string
	Firmware string
	Operator string

	// Network
	NetworkType    string // "4G", "3G", "2G", "No Service"
	NetworkTypeRaw int
	Band           int
	ConnectStatus  int
	SignalStrength int // 0-5 (from API, but M7010 reports 0 always)
	RSRP           int // dBm, actual signal metric
	RSRQ           int // dB
	RSSI           int
	SNR            int
	WanIP          string

	// Battery
	BatteryPercent  int
	BatteryCharging bool

	// Speed
	TxSpeed string
	RxSpeed string

	// Data usage
	TotalBytes      float64 // total since last reset
	DailyBytes      float64
	AdjustedBytes   float64 // adjusted statistics from flowstat
	MonthLimitBytes float64
	PaymentDay      int
	ConnectedDevices int

	// Raw responses for debugging
	RawStatus   map[string]any
	RawFlowStat map[string]any
}

// networkTypeStr maps the numeric networkType to a human-readable string.
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
		return "4G+"
	default:
		return fmt.Sprintf("Unknown(%d)", t)
	}
}

// rsrpToSignal converts RSRP (dBm) to a 0-5 signal bar scale.
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

	resp, err := c.encryptedRequest("/cgi-bin/web_cgi", map[string]any{
		"module": "status",
		"action": 0,
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
	// Device info (nested under "deviceInfo")
	if di, ok := resp["deviceInfo"].(map[string]any); ok {
		s.Model = firstStr(di, "model")
		s.Firmware = firstStr(di, "firmwareVer")
	}

	// WAN info (nested under "wan")
	if wan, ok := resp["wan"].(map[string]any); ok {
		s.NetworkTypeRaw = firstInt(wan, "networkType")
		s.NetworkType = networkTypeStr(s.NetworkTypeRaw)
		s.Band = firstInt(wan, "band")
		s.ConnectStatus = firstInt(wan, "connectStatus")
		s.SignalStrength = firstInt(wan, "signalStrength")
		s.RSRP = firstInt(wan, "rsrp")
		s.RSRQ = firstInt(wan, "rsrq")
		s.RSSI = firstInt(wan, "rssi")
		s.SNR = firstInt(wan, "snr")
		s.WanIP = firstStr(wan, "ipv4")
		s.Operator = firstStr(wan, "operatorName")
		s.TxSpeed = firstStr(wan, "txSpeed")
		s.RxSpeed = firstStr(wan, "rxSpeed")

		// Data stats from status (bytes as float strings)
		s.TotalBytes = parseFloatStr(wan, "totalStatistics")
		s.DailyBytes = parseFloatStr(wan, "dailyStatistics")

		// If signalStrength is 0 (M7010 bug), derive from RSRP
		if s.SignalStrength == 0 && s.RSRP != 0 {
			s.SignalStrength = rsrpToSignal(s.RSRP)
		}
	}

	// Battery (nested under "battery")
	if bat, ok := resp["battery"].(map[string]any); ok {
		s.BatteryPercent = firstInt(bat, "voltage") // M7010 uses "voltage" for percentage
		s.BatteryCharging = firstBool(bat, "charging")
	}

	// Connected devices
	if cd, ok := resp["connectedDevices"].(map[string]any); ok {
		s.ConnectedDevices = firstInt(cd, "number")
	}
}

func (c *Client) GetFlowStats(s *Status) error {
	if c.token == "" {
		return fmt.Errorf("not logged in")
	}

	resp, err := c.encryptedRequest("/cgi-bin/web_cgi", map[string]any{
		"module": "flowstat",
		"action": 0,
		"token":  c.token,
	})
	if err != nil {
		return fmt.Errorf("get flow stats: %w", err)
	}

	s.RawFlowStat = resp

	if settings, ok := resp["settings"].(map[string]any); ok {
		s.AdjustedBytes = parseFloatStr(settings, "adjustStatistics")
		s.PaymentDay = firstInt(settings, "paymentDay")

		limitStr := firstStr(settings, "limitation")
		if limitStr != "" && limitStr != "0.000000" {
			fmt.Sscanf(limitStr, "%f", &s.MonthLimitBytes)
		}
	}

	return nil
}

// parseFloatStr extracts a float from a string value in the map (e.g. "14473800628.000000").
func parseFloatStr(m map[string]any, key string) float64 {
	if s := firstStr(m, key); s != "" {
		var f float64
		fmt.Sscanf(s, "%f", &f)
		return f
	}
	return firstFloat(m, key)
}

// --- JSON helpers ---

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func firstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				if int(n) != 0 {
					return int(n)
				}
			case int:
				if n != 0 {
					return n
				}
			}
		}
	}
	return 0
}

func firstFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				if n != 0 {
					return n
				}
			case int:
				if n != 0 {
					return float64(n)
				}
			}
		}
	}
	return 0
}

func firstBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if b, ok := v.(bool); ok && b {
				return true
			}
		}
	}
	return false
}
