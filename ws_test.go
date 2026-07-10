package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// fakeConn satisfies net.Conn for readMessage tests: reads come from the
// wsConn's bufio.Reader (fed separately), writes are captured.
type fakeConn struct {
	wrote bytes.Buffer
}

func (f *fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (f *fakeConn) Write(b []byte) (int, error)      { return f.wrote.Write(b) }
func (f *fakeConn) Close() error                     { return nil }
func (f *fakeConn) LocalAddr() net.Addr              { return nil }
func (f *fakeConn) RemoteAddr() net.Addr             { return nil }
func (f *fakeConn) SetDeadline(time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func newTestWS(stream []byte) (*wsConn, *fakeConn) {
	fc := &fakeConn{}
	return &wsConn{c: fc, r: bufio.NewReader(bytes.NewReader(stream))}, fc
}

// serverFrame builds an unmasked server→client frame.
func serverFrame(opcode byte, payload []byte) []byte {
	var b []byte
	b = append(b, 0x80|opcode)
	switch {
	case len(payload) < 126:
		b = append(b, byte(len(payload)))
	case len(payload) < 65536:
		b = append(b, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		b = append(b, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(len(payload)))
		b = append(b, ext[:]...)
	}
	return append(b, payload...)
}

func TestReadMessageText(t *testing.T) {
	ws, _ := newTestWS(serverFrame(wsOpcodeText, []byte(`{"name":"x"}`)))
	msg, err := ws.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if string(msg) != `{"name":"x"}` {
		t.Errorf("got %q", msg)
	}
}

func TestReadMessageExtendedLength(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), 300) // forces the 126 length form
	ws, _ := newTestWS(serverFrame(wsOpcodeText, payload))
	msg, err := ws.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if !bytes.Equal(msg, payload) {
		t.Errorf("payload mismatch: got %d bytes", len(msg))
	}
}

func TestReadMessageMasked(t *testing.T) {
	// Servers shouldn't mask, but the parser supports it — verify unmasking.
	payload := []byte("hello")
	mask := [4]byte{0x01, 0x02, 0x03, 0x04}
	frame := []byte{0x80 | wsOpcodeText, 0x80 | byte(len(payload))}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	ws, _ := newTestWS(frame)
	msg, err := ws.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if string(msg) != "hello" {
		t.Errorf("got %q", msg)
	}
}

func TestReadMessagePingRepliesMaskedPongWithEcho(t *testing.T) {
	pingPayload := []byte("keepalive")
	stream := append(serverFrame(wsOpcodePing, pingPayload),
		serverFrame(wsOpcodeText, []byte("after"))...)
	ws, fc := newTestWS(stream)

	msg, err := ws.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if string(msg) != "after" {
		t.Errorf("got %q, want the text frame after the ping", msg)
	}

	pong := fc.wrote.Bytes()
	if len(pong) != 2+4+len(pingPayload) {
		t.Fatalf("pong frame length %d, want %d", len(pong), 2+4+len(pingPayload))
	}
	if pong[0] != 0x80|wsOpcodePong {
		t.Errorf("pong opcode byte = %#x", pong[0])
	}
	if pong[1]&0x80 == 0 {
		t.Error("pong is unmasked — RFC 6455 requires masking client→server frames")
	}
	if int(pong[1]&0x7F) != len(pingPayload) {
		t.Errorf("pong payload length %d, want %d", pong[1]&0x7F, len(pingPayload))
	}
	var mask [4]byte
	copy(mask[:], pong[2:6])
	unmasked := make([]byte, len(pingPayload))
	for i, b := range pong[6:] {
		unmasked[i] = b ^ mask[i%4]
	}
	if !bytes.Equal(unmasked, pingPayload) {
		t.Errorf("pong payload %q, want echo of ping payload %q", unmasked, pingPayload)
	}
}

func TestReadMessageClose(t *testing.T) {
	ws, _ := newTestWS(serverFrame(wsOpcodeClose, nil))
	if _, err := ws.readMessage(); err != io.EOF {
		t.Errorf("close frame: err = %v, want io.EOF", err)
	}
}

func TestReadMessageOversizedFrameRejected(t *testing.T) {
	frame := []byte{0x80 | wsOpcodeText, 127}
	var ext [8]byte
	binary.BigEndian.PutUint64(ext[:], 1<<40) // absurd length claim
	frame = append(frame, ext[:]...)
	ws, _ := newTestWS(frame)
	if _, err := ws.readMessage(); err == nil {
		t.Error("oversized frame accepted; want an error before allocating")
	}
}
