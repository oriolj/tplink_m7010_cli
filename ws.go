package main

// Minimal receive-only WebSocket client.
//
// The Mudi 7 (GL-E5800) pushes cellular signal / SIM / traffic state as a
// stream of named JSON events over a WebSocket at ws://<addr>/ws?sid=<sid>.
// There's no equivalent RPC under /rpc — the home-screen tile reads this
// stream too — so we need a WebSocket client to surface signal, operator,
// data-used etc. We hand-roll instead of pulling in gorilla/websocket
// because we only need a tiny subset: handshake + read text frames.
//
// We skip the Sec-WebSocket-Accept verification (we're talking to a
// known device on the LAN, not the public internet).

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	wsOpcodeText  = 0x1
	wsOpcodeClose = 0x8
	wsOpcodePing  = 0x9
	wsOpcodePong  = 0xA

	// wsMaxFrameSize caps how much we'll allocate for one frame's payload.
	// The Mudi's event bursts are a few KB; anything near this limit is a
	// corrupt or hostile length field, not real data.
	wsMaxFrameSize = 1 << 20
)

type wsConn struct {
	c net.Conn
	r *bufio.Reader
}

// dialWS opens a WebSocket to ws://addr<path> with the given Cookie
// header. `deadline` is the absolute end-of-time for every operation
// (dial, handshake, and subsequent reads/writes) — set once here, the
// caller does not need to touch SetDeadline again.
func dialWS(addr, path, cookie string, deadline time.Time) (*wsConn, error) {
	// addr is usually a bare IP (default port 80), but honour an explicit
	// host:port — --addr 192.168.8.1:8080 must reach the WS too.
	hostport := addr
	if _, _, err := net.SplitHostPort(addr); err != nil {
		hostport = net.JoinHostPort(addr, "80")
	}
	c, err := net.DialTimeout("tcp", hostport, time.Until(deadline))
	if err != nil {
		return nil, fmt.Errorf("ws dial: %w", err)
	}
	c.SetDeadline(deadline)

	keyBytes := make([]byte, 16)
	rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)

	req := strings.NewReplacer(
		"$PATH", path, "$HOST", addr, "$KEY", key, "$COOKIE", cookie,
	).Replace(
		"GET $PATH HTTP/1.1\r\n" +
			"Host: $HOST\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Key: $KEY\r\n" +
			"Sec-WebSocket-Version: 13\r\n" +
			"Origin: http://$HOST\r\n" +
			"Cookie: $COOKIE\r\n" +
			"\r\n",
	)
	if _, err := c.Write([]byte(req)); err != nil {
		c.Close()
		return nil, fmt.Errorf("ws send handshake: %w", err)
	}

	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, &http.Request{Method: "GET"})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("ws read handshake: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		c.Close()
		return nil, fmt.Errorf("ws handshake: status %d", resp.StatusCode)
	}

	return &wsConn{c: c, r: r}, nil
}

// readMessage returns the next non-control text frame's payload. Control
// frames (ping/close) are handled inline. Returns io.EOF on connection
// close. Returns whatever wrap of net.Error if the read deadline elapses.
func (w *wsConn) readMessage() ([]byte, error) {
	for {
		hdr, err := readN(w.r, 2)
		if err != nil {
			return nil, err
		}
		b1, b2 := hdr[0], hdr[1]
		opcode := b1 & 0x0F
		masked := b2&0x80 != 0
		plen := uint64(b2 & 0x7F)
		switch plen {
		case 126:
			ext, err := readN(w.r, 2)
			if err != nil {
				return nil, err
			}
			plen = uint64(binary.BigEndian.Uint16(ext))
		case 127:
			ext, err := readN(w.r, 8)
			if err != nil {
				return nil, err
			}
			plen = binary.BigEndian.Uint64(ext)
		}
		if plen > wsMaxFrameSize {
			return nil, fmt.Errorf("ws frame payload %d exceeds %d byte cap", plen, wsMaxFrameSize)
		}
		var maskKey [4]byte
		if masked {
			mk, err := readN(w.r, 4)
			if err != nil {
				return nil, err
			}
			copy(maskKey[:], mk)
		}
		payload, err := readN(w.r, int(plen))
		if err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}

		switch opcode {
		case wsOpcodeText:
			return payload, nil
		case wsOpcodeClose:
			return nil, io.EOF
		case wsOpcodePing:
			// Pong must echo the ping payload (RFC 6455 §5.5.3) and be
			// masked like every other client→server frame. Best-effort.
			w.sendFrame(wsOpcodePong, payload)
			continue
		default:
			// Skip continuation, binary, pong frames — we don't expect any.
			continue
		}
	}
}

// sendFrame writes one masked frame. Client→server frames are
// mask-required per RFC 6455 §5.3 — a compliant server MUST drop the
// connection on an unmasked one.
func (w *wsConn) sendFrame(opcode byte, payload []byte) error {
	plen := len(payload)
	var hdr []byte
	hdr = append(hdr, 0x80|opcode)
	switch {
	case plen < 126:
		hdr = append(hdr, 0x80|byte(plen))
	case plen < 65536:
		hdr = append(hdr, 0x80|126, byte(plen>>8), byte(plen))
	default:
		hdr = append(hdr, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(plen))
		hdr = append(hdr, ext[:]...)
	}
	var mask [4]byte
	rand.Read(mask[:])
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, plen)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	_, err := w.c.Write(append(hdr, masked...))
	return err
}

// sendText writes a text frame.
func (w *wsConn) sendText(payload string) error {
	return w.sendFrame(wsOpcodeText, []byte(payload))
}

// close sends a close frame and tears down the connection. Best-effort.
func (w *wsConn) close() error {
	w.sendFrame(wsOpcodeClose, nil)
	return w.c.Close()
}

func readN(r *bufio.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	return buf, err
}
