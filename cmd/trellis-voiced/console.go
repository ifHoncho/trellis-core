package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"meridian/trellis/internal/voice"
	"meridian/trellis/internal/voice/wire"
)

// The browser console (architecture §13, Voice): webd passes each
// browser's WebSocket to this socket, one connection per browser, its
// messages framed by internal/voice/wire.
//
// From the browser, JSON: {"t":"hello","name":…}, {"t":"monitor",
// "groups":[…]}, {"t":"tx","group":n} (the group its talk button keys:
// the node then listens to it, to know when it is busy),
// {"t":"ptt","group":n,"down":bool} or {"t":"ptt",
// "all":true,"down":bool} (an all-call: every group); binary: 20 ms of
// microphone, 320 16-bit little-endian samples at 16 kHz.
//
// To the browser, JSON: {"t":"hello",…} with the console and the groups,
// {"t":"state",…} four times a second, {"t":"ptt",…} for each press,
// {"t":"notice","what":"busy"|"tx-limit"}; binary: a group's 20 ms, its
// number in one byte, then 320 samples as above.

type consoleServer struct {
	cur *atomic.Pointer[voice.Engine]
	log *slog.Logger
}

func serveConsoles(path string, cur *atomic.Pointer[voice.Engine], log *slog.Logger) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	s := &consoleServer{cur: cur, log: log}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.session(c)
		}
	}()
	return ln, nil
}

type msg struct {
	T      string `json:"t"`
	Name   string `json:"name,omitempty"`
	Groups []int  `json:"groups,omitempty"`
	Group  int    `json:"group,omitempty"`
	Down   bool   `json:"down,omitempty"`
	All    bool   `json:"all,omitempty"` // an all-call: every group at once
}

func (s *consoleServer) session(conn net.Conn) {
	defer conn.Close()
	var wmu sync.Mutex
	send := func(kind byte, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return wire.Write(conn, kind, b)
	}
	sendJSON := func(v any) error {
		b, _ := json.Marshal(v)
		return send(wire.Text, b)
	}
	eng := s.cur.Load()
	if eng == nil {
		sendJSON(map[string]string{"t": "error", "error": "voice is not running on this node"})
		return
	}
	// The first message names the console.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	kind, b, err := wire.Read(conn)
	var hello msg
	if err != nil || kind != wire.Text || json.Unmarshal(b, &hello) != nil || hello.T != "hello" {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	c, err := eng.AddConsole(hello.Name)
	if err != nil {
		sendJSON(map[string]string{"t": "error", "error": err.Error()})
		return
	}
	defer eng.RemoveConsole(c)
	s.log.Info("console connected", "console", c.Name)
	defer s.log.Info("console gone", "console", c.Name)
	st := eng.Status()
	type groupInfo struct {
		Number   int    `json:"number"`
		Name     string `json:"name"`
		Priority bool   `json:"priority,omitempty"`
	}
	var gs []groupInfo
	for _, g := range eng.Groups() {
		gs = append(gs, groupInfo{g.Number, g.Name, g.Priority})
	}
	if sendJSON(map[string]any{"t": "hello", "console": c.Name, "groups": gs, "rate": voice.ConsoleRate, "frame": voice.ConsoleFrameLen, "tx_limit_s": 60, "fec_loss_perc": st.FECLossPerc}) != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// To the browser: audio, notices, state; and the session ends with the
	// engine (a new configuration starts a new one: the browser reconnects).
	go func() {
		defer cancel()
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		frame := make([]byte, 1+2*voice.ConsoleFrameLen)
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-c.Frames:
				frame[0] = byte(f.Group)
				for i, v := range f.PCM {
					binary.LittleEndian.PutUint16(frame[1+2*i:], uint16(v))
				}
				if send(wire.Binary, frame) != nil {
					return
				}
			case n := <-c.Notices:
				if sendJSON(map[string]string{"t": "notice", "what": n}) != nil {
					return
				}
			case <-tick.C:
				if s.cur.Load() != eng {
					return
				}
				st := eng.Status()
				if sendJSON(map[string]any{"t": "state", "groups": st.Groups, "consoles": st.Consoles, "fec_loss_perc": st.FECLossPerc}) != nil {
					return
				}
			}
		}
	}()
	// From the browser.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	mic := make([]int16, voice.ConsoleFrameLen)
	for {
		kind, b, err := wire.Read(conn)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.log.Debug("console read", "console", c.Name, "err", err)
			}
			return
		}
		switch kind {
		case wire.Binary:
			if len(b) != 2*voice.ConsoleFrameLen {
				continue
			}
			for i := range mic {
				mic[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
			}
			_ = eng.ConsoleAudio(c, mic)
		case wire.Text:
			var m msg
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			switch m.T {
			case "monitor":
				if err := eng.ConsoleMonitor(c, m.Groups); err != nil {
					sendJSON(map[string]string{"t": "error", "error": err.Error()})
				}
			case "tx":
				if err := eng.ConsoleSelect(c, m.Group); err != nil {
					sendJSON(map[string]string{"t": "error", "error": err.Error()})
				}
			case "ptt":
				if m.All {
					groups := eng.ConsoleAllCall(c, m.Down)
					sendJSON(map[string]any{"t": "ptt", "all": true, "down": m.Down, "granted": m.Down && len(groups) > 0, "groups": groups})
					break
				}
				granted, err := eng.ConsolePTT(c, m.Group, m.Down)
				r := map[string]any{"t": "ptt", "group": m.Group, "down": m.Down, "granted": granted}
				if err != nil {
					r["error"] = err.Error()
				}
				sendJSON(r)
			}
		}
	}
}
