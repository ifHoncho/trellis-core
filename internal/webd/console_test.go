package webd

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/voice/wire"
)

// fakeVoiced echoes every console message back.
func fakeVoiced(t *testing.T) string {
	sock := filepath.Join(t.TempDir(), "c.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				for {
					k, m, err := wire.Read(c)
					if err != nil {
						return
					}
					wire.Write(c, k, m)
				}
			}()
		}
	}()
	return sock
}

// dialWS opens the console's WebSocket on the test server.
func dialWS(t *testing.T, base, cookie, origin string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	u, _ := url.Parse(base)
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := "GET " + apiPrefix + "/voice/console HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\nOrigin: " + origin + "\r\n"
	if cookie != "" {
		req += "Cookie: " + sessionCookie + "=" + cookie + "\r\n"
	}
	c.Write([]byte(req + "\r\n"))
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, r, resp.Status
}

func TestConsoleBridge(t *testing.T) {
	n := &node{password: "pw-console-1"}
	ts, s := start(t, n)
	s.opts.VoiceConsole = fakeVoiced(t)
	cookie := login(t, ts, "pw-console-1")
	origin := ts.URL

	if _, _, st := dialWS(t, ts.URL, "", origin); !strings.HasPrefix(st, "401") {
		t.Errorf("no session: %s", st)
	}
	if _, _, st := dialWS(t, ts.URL, cookie, "https://evil.example"); !strings.HasPrefix(st, "400") {
		t.Errorf("another site: %s", st)
	}
	c, r, st := dialWS(t, ts.URL, cookie, origin)
	if !strings.HasPrefix(st, "101") {
		t.Fatalf("handshake: %s", st)
	}
	defer c.Close()
	c.Write(frame(true, opText, []byte(`{"t":"hello"}`)))
	c.Write(frame(true, opBinary, make([]byte, 640)))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for i, want := range []struct {
		op byte
		n  int
	}{{opText, 13}, {opBinary, 640}} {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			t.Fatal(err)
		}
		n := int(h[1] & 0x7f)
		if n == 126 {
			var b [2]byte
			io.ReadFull(r, b[:])
			n = int(binary.BigEndian.Uint16(b[:]))
		}
		p := make([]byte, n)
		io.ReadFull(r, p)
		if h[0]&0x0f != want.op || n != want.n || h[1]&0x80 != 0 {
			t.Errorf("message %d: op %d, %d bytes, masked %v", i, h[0]&0x0f, n, h[1]&0x80 != 0)
		}
	}
}

// readWSFrame reads one unmasked frame from the server.
func readWSFrame(r *bufio.Reader) (op byte, p []byte, err error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[1] & 0x7f)
	if n == 126 {
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = int(binary.BigEndian.Uint16(b[:]))
	}
	p = make([]byte, n)
	_, err = io.ReadFull(r, p)
	return h[0] & 0x0f, p, err
}

// A console that only listens sends nothing but pongs: those keep it
// connected. One that stops answering is cut.
func TestConsoleIdle(t *testing.T) {
	defer func(p, i time.Duration) { consolePing, consoleIdle = p, i }(consolePing, consoleIdle)
	consolePing, consoleIdle = 50*time.Millisecond, 300*time.Millisecond
	n := &node{password: "pw-console-1"}
	ts, s := start(t, n)
	s.opts.VoiceConsole = fakeVoiced(t)
	cookie := login(t, ts, "pw-console-1")
	c, r, st := dialWS(t, ts.URL, cookie, ts.URL)
	if !strings.HasPrefix(st, "101") {
		t.Fatalf("handshake: %s", st)
	}
	defer c.Close()
	begin, pings := time.Now(), 0
	for time.Since(begin) < 4*consoleIdle {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		op, p, err := readWSFrame(r)
		if err != nil || op == opClose {
			t.Fatalf("cut after %v with %d pings answered (op %d, err %v)", time.Since(begin), pings, op, err)
		}
		if op == opPing {
			pings++
			c.Write(frame(true, opPong, p))
		}
	}
	if pings < 3 {
		t.Fatalf("%d pings in %v", pings, time.Since(begin))
	}
	// No more pongs: gone within the idle time (and a little).
	silent := time.Now()
	for {
		c.SetReadDeadline(time.Now().Add(5 * consoleIdle))
		op, _, err := readWSFrame(r)
		if err != nil || op == opClose {
			break
		}
	}
	if d := time.Since(silent); d < consoleIdle/2 || d > 4*consoleIdle {
		t.Errorf("a silent console was cut after %v, want about %v", d, consoleIdle)
	}
}

// Signing out ends the console at its next ping.
func TestConsoleEndsWithSession(t *testing.T) {
	defer func(p time.Duration) { consolePing = p }(consolePing)
	consolePing = 50 * time.Millisecond
	n := &node{password: "pw-console-1"}
	ts, s := start(t, n)
	s.opts.VoiceConsole = fakeVoiced(t)
	cookie := login(t, ts, "pw-console-1")
	c, r, st := dialWS(t, ts.URL, cookie, ts.URL)
	if !strings.HasPrefix(st, "101") {
		t.Fatalf("handshake: %s", st)
	}
	defer c.Close()
	s.sessions.drop(cookie)
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.SetReadDeadline(deadline)
		op, p, err := readWSFrame(r)
		if err != nil || op == opClose {
			return
		}
		if op == opPing {
			c.Write(frame(true, opPong, p))
		}
		if time.Now().After(deadline) {
			t.Fatal("the console outlived its session")
		}
	}
}
