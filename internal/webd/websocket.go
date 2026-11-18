package webd

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A WebSocket server (RFC 6455), as much as the browser console needs:
// the handshake, text and binary messages (fragmented or not), ping, pong
// and close. Frames from the browser are read with every length checked:
// they come from any device that can reach the UI.

const (
	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxMessage = 64 << 10
	opCont       = 0x0
	opText       = 0x1
	opBinary     = 0x2
	opClose      = 0x8
	opPing       = 0x9
	opPong       = 0xa
)

var errWSClosed = errors.New("websocket closed")

type wsConn struct {
	c  net.Conn
	r  *bufio.Reader
	mu sync.Mutex // writes
	// idle, when set, is how long the peer may send nothing at all: every
	// frame read, a pong included, moves the read deadline on.
	idle time.Duration
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// wsUpgrade checks a WebSocket handshake from a page of this UI (the
// Origin must be this host: the session cookie alone does not show that)
// and takes the connection over.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	k, err := base64.StdEncoding.DecodeString(key)
	switch {
	case r.Method != http.MethodGet, !headerHas(r.Header, "Connection", "upgrade"), !headerHas(r.Header, "Upgrade", "websocket"):
		return nil, errors.New("not a WebSocket request")
	case r.Header.Get("Sec-WebSocket-Version") != "13":
		w.Header().Set("Sec-WebSocket-Version", "13")
		return nil, errors.New("WebSocket version 13 only")
	case err != nil || len(k) != 16:
		return nil, errors.New("bad Sec-WebSocket-Key")
	}
	o, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || o.Host != r.Host {
		return nil, errors.New("this request comes from another site")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("cannot take the connection over")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{}) // the server's request deadlines end here
	sum := sha1.Sum([]byte(key + wsGUID))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{c: conn, r: brw.Reader}, nil
}

// readFrame reads one frame: fin, opcode, unmasked payload.
func readFrame(r io.Reader, max int) (bool, byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin, op := h[0]&0x80 != 0, h[0]&0x0f
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: reserved bits set")
	}
	if h[1]&0x80 == 0 {
		return false, 0, nil, errors.New("websocket: an unmasked frame from a client")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= opClose && (n > 125 || !fin) {
		return false, 0, nil, errors.New("websocket: a bad control frame")
	}
	if n > uint64(max) {
		return false, 0, nil, fmt.Errorf("websocket: a %d-byte frame", n)
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return false, 0, nil, err
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return false, 0, nil, err
	}
	for i := range p {
		p[i] ^= mask[i%4]
	}
	return fin, op, p, nil
}

// ReadMessage reads the next text or binary message, answering pings and
// closes on the way.
func (c *wsConn) ReadMessage() (byte, []byte, error) {
	var (
		op  byte
		msg []byte
	)
	for {
		if c.idle > 0 {
			_ = c.c.SetReadDeadline(time.Now().Add(c.idle))
		}
		fin, fop, p, err := readFrame(c.r, wsMaxMessage-len(msg))
		if err != nil {
			return 0, nil, err
		}
		switch fop {
		case opPing:
			if err := c.write(opPong, p); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.write(opClose, p)
			return 0, nil, errWSClosed
		case opText, opBinary:
			if op != 0 {
				return 0, nil, errors.New("websocket: a new message inside a fragmented one")
			}
			op = fop
		case opCont:
			if op == 0 {
				return 0, nil, errors.New("websocket: a continuation with no message")
			}
		default:
			return 0, nil, errors.New("websocket: an unknown opcode")
		}
		msg = append(msg, p...)
		if fin {
			return op, msg, nil
		}
	}
}

// write sends one unfragmented, unmasked frame.
func (c *wsConn) write(op byte, p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := []byte{0x80 | op}
	switch n := len(p); {
	case n < 126:
		h = append(h, byte(n))
	case n <= 0xffff:
		h = append(h, 126)
		h = binary.BigEndian.AppendUint16(h, uint16(n))
	default:
		h = append(h, 127)
		h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	_ = c.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.c.Write(append(h, p...)); err != nil {
		return err
	}
	return nil
}

func (c *wsConn) Close() error {
	_ = c.write(opClose, []byte{0x03, 0xe8}) // 1000, normal
	return c.c.Close()
}
