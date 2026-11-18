package main

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"meridian/trellis/internal/voice"
	"meridian/trellis/internal/voice/wire"
)

// A made-up voice console server (trellis-voiced's console socket), for
// make ui-dev: the talkers of Voice() speak (tones), PTT is refused on a
// busy group, and what the browser sends while keyed is measured and shown
// as this node's talk.

func (f *fake) serveConsole(path string) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.consoleSession(c)
		}
	}()
	return nil
}

// fakeTalkers is who talks on which group now (as Voice() says).
func fakeTalkers(now time.Time) map[int]string {
	sec := now.Unix()
	t := map[int]string{}
	if sec%15 < 10 {
		t[2] = "bravo-3"
	}
	if sec%40 < 3 {
		t[1] = "cmd-post"
	}
	if sec%60 > 50 {
		t[4] = "medic-2"
	}
	return t
}

func (f *fake) consoleSession(conn net.Conn) {
	defer conn.Close()
	var mu sync.Mutex
	send := func(kind byte, b []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return wire.Write(conn, kind, b)
	}
	sendJSON := func(v any) error { b, _ := json.Marshal(v); return send(wire.Text, b) }
	kind, b, err := wire.Read(conn)
	var hello struct{ T, Name string }
	if err != nil || kind != wire.Text || json.Unmarshal(b, &hello) != nil {
		return
	}
	f.mu.Lock()
	name := f.node.Callsign + "/" + hello.Name
	type gi struct {
		Number   int    `json:"number"`
		Name     string `json:"name"`
		Priority bool   `json:"priority,omitempty"`
	}
	var gs []gi
	for _, g := range f.profile.Voice.Groups {
		gs = append(gs, gi{g.Number, g.Name, g.Priority})
	}
	f.mu.Unlock()
	sendJSON(map[string]any{"t": "hello", "console": name, "groups": gs, "rate": 16000, "frame": 320, "tx_limit_s": 60})

	var (
		smu      sync.Mutex
		monitor  = map[int]bool{}
		txGroup  int
		txLevel  float64
		done     = make(chan struct{})
		received int
	)
	defer close(done)
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		frame := make([]byte, 1+2*320)
		phase := 0.0
		n := 0
		for {
			select {
			case <-done:
				return
			case now := <-tick.C:
				talk := fakeTalkers(now)
				smu.Lock()
				mon := map[int]bool{}
				for k, v := range monitor {
					mon[k] = v
				}
				tg := txGroup
				smu.Unlock()
				for g := range talk {
					if !mon[g] {
						continue
					}
					// A voice-ish tone: 300 Hz with a slow wobble, per group.
					base := 220.0 + 80*float64(g)
					frame[0] = byte(g)
					for i := 0; i < 320; i++ {
						p := phase + float64(i)/16000
						v := 6000 * math.Sin(2*math.Pi*base*p) * (0.6 + 0.4*math.Sin(2*math.Pi*3*p))
						binary.LittleEndian.PutUint16(frame[1+2*i:], uint16(int16(v)))
					}
					if send(wire.Binary, frame) != nil {
						return
					}
				}
				phase += 0.02
				if n++; n%12 == 0 {
					var groups []voice.GroupStatus
					f.mu.Lock()
					for _, g := range f.profile.Voice.Groups {
						ap, _ := g.VoiceAddr()
						s := voice.GroupStatus{Number: g.Number, Name: g.Name, Addr: ap.String(), Listening: mon[g.Number], Talker: talk[g.Number], Busy: talk[g.Number] != ""}
						if tg == g.Number || tg == -1 {
							s.Sending, s.Talker = true, ""
						}
						groups = append(groups, s)
					}
					f.mu.Unlock()
					smu.Lock()
					cs := []voice.ConsoleStatus{{ID: 1, Name: name, Sending: tg != 0, Group: max(tg, 0), AllCall: tg == -1}}
					smu.Unlock()
					if sendJSON(map[string]any{"t": "state", "groups": groups, "consoles": cs, "fec_loss_perc": 10, "rx_level": txLevel}) != nil {
						return
					}
				}
			}
		}
	}()
	for {
		kind, b, err := wire.Read(conn)
		if err != nil {
			return
		}
		if kind == wire.Binary {
			var s float64
			for i := 0; i+1 < len(b); i += 2 {
				v := float64(int16(binary.LittleEndian.Uint16(b[i:])))
				s += v * v
			}
			smu.Lock()
			txLevel = math.Sqrt(s / float64(len(b)/2))
			received++
			smu.Unlock()
			continue
		}
		var m struct {
			T      string
			Groups []int
			Group  int
			Down   bool
			All    bool
		}
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch m.T {
		case "monitor":
			smu.Lock()
			monitor = map[int]bool{}
			for _, g := range m.Groups {
				monitor[g] = true
			}
			smu.Unlock()
		case "ptt":
			if m.All {
				smu.Lock()
				txGroup = 0
				if m.Down {
					txGroup = -1 // every group
				}
				smu.Unlock()
				sendJSON(map[string]any{"t": "ptt", "all": true, "down": m.Down, "granted": m.Down})
				break
			}
			granted := false
			if m.Down {
				if fakeTalkers(time.Now())[m.Group] == "" {
					granted = true
					smu.Lock()
					txGroup = m.Group
					smu.Unlock()
				}
			} else {
				smu.Lock()
				txGroup = 0
				smu.Unlock()
			}
			sendJSON(map[string]any{"t": "ptt", "group": m.Group, "down": m.Down, "granted": granted})
		}
	}
}
