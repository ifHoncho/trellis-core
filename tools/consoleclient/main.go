// Command consoleclient is a voice console without a browser, for the lab:
// it signs in to webd, opens the console's WebSocket, monitors groups and
// measures what it hears, and keys a group with a tone.
//
//	consoleclient --url http://127.0.0.1:8480 --password PW --monitor 1 \
//	    --listen 3s --talk 2 --tone 1000 --talk-for 2s
//
// It prints one JSON object: per group the frames heard and the share of
// --hear-hz in them, whether the PTT was granted, the notices.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	u := flag.String("url", "http://127.0.0.1:8480", "webd")
	pw := flag.String("password", "", "the admin password")
	name := flag.String("name", "lab", "console name")
	mon := flag.String("monitor", "", "groups to monitor, comma-separated")
	listen := flag.Duration("listen", 3*time.Second, "how long to listen before talking")
	talk := flag.Int("talk", 0, "group to key (0: none)")
	tone := flag.Float64("tone", 1000, "the tone to talk, Hz")
	talkFor := flag.Duration("talk-for", 2*time.Second, "how long to talk")
	hearHz := flag.Float64("hear-hz", 440, "the tone to look for in what is heard")
	all := flag.Bool("all", false, "an all-call (every group) instead of --talk")
	flag.Parse()
	allCall = *all
	if allCall && *talk == 0 {
		*talk = -1
	}
	if err := run(*u, *pw, *name, *mon, *listen, *talk, *tone, *talkFor, *hearHz); err != nil {
		fmt.Fprintln(os.Stderr, "consoleclient:", err)
		os.Exit(1)
	}
}

var allCall bool

func ptt(group int, down bool) map[string]any {
	if allCall {
		return map[string]any{"t": "ptt", "all": true, "down": down}
	}
	return map[string]any{"t": "ptt", "group": group, "down": down}
}

func run(base, pw, name, mon string, listen time.Duration, talk int, tone float64, talkFor time.Duration, hearHz float64) error {
	bu, err := url.Parse(base)
	if err != nil {
		return err
	}
	// Sign in.
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/login", strings.NewReader(`{"password":`+strconv.Quote(pw)+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Trellis-Request", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	var cookie string
	for _, c := range resp.Cookies() {
		cookie = c.Name + "=" + c.Value
	}
	if resp.StatusCode != 200 || cookie == "" {
		return fmt.Errorf("sign-in: %s", resp.Status)
	}
	// The WebSocket.
	conn, err := net.Dial("tcp", bu.Host)
	if err != nil {
		return err
	}
	defer conn.Close()
	k := make([]byte, 16)
	rand.Read(k)
	fmt.Fprintf(conn, "GET /api/v1/voice/console HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nOrigin: %s\r\nCookie: %s\r\n\r\n",
		bu.Host, base64.StdEncoding.EncodeToString(k), base, cookie)
	r := bufio.NewReader(conn)
	hr, err := http.ReadResponse(r, nil)
	if err != nil || hr.StatusCode != 101 {
		return fmt.Errorf("handshake: %v %v", err, hr)
	}
	var wmu sync.Mutex
	send := func(op byte, p []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		h := []byte{0x80 | op}
		if len(p) < 126 {
			h = append(h, 0x80|byte(len(p)))
		} else {
			h = append(h, 0x80|126)
			h = binary.BigEndian.AppendUint16(h, uint16(len(p)))
		}
		m := make([]byte, 4)
		rand.Read(m)
		h = append(h, m...)
		b := make([]byte, len(p))
		for i := range p {
			b[i] = p[i] ^ m[i%4]
		}
		conn.Write(append(h, b...))
	}
	sendJSON := func(v any) { b, _ := json.Marshal(v); send(1, b) }
	sendJSON(map[string]string{"t": "hello", "name": name})
	var groups []int
	for _, s := range strings.Split(mon, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			groups = append(groups, n)
		}
	}
	sendJSON(map[string]any{"t": "monitor", "groups": groups})

	type heard struct {
		Frames int     `json:"frames"`
		Share  float64 `json:"share"`
		power  [2]float64
	}
	out := struct {
		Console string         `json:"console"`
		Heard   map[int]*heard `json:"heard"`
		Granted *bool          `json:"granted,omitempty"`
		Notices []string       `json:"notices"`
		Talker  map[int]string `json:"talkers"`
	}{Heard: map[int]*heard{}, Notices: []string{}, Talker: map[int]string{}}
	var mu sync.Mutex
	go func() {
		for {
			var h [2]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				return
			}
			n := int(h[1] & 0x7f)
			if n == 126 {
				var b [2]byte
				io.ReadFull(r, b[:])
				n = int(binary.BigEndian.Uint16(b[:]))
			} else if n == 127 {
				var b [8]byte
				io.ReadFull(r, b[:])
				n = int(binary.BigEndian.Uint64(b[:]))
			}
			p := make([]byte, n)
			if _, err := io.ReadFull(r, p); err != nil {
				return
			}
			mu.Lock()
			switch h[0] & 0x0f {
			case 2:
				g := int(p[0])
				x := out.Heard[g]
				if x == nil {
					x = &heard{}
					out.Heard[g] = x
				}
				x.Frames++
				pcm := make([]float64, (len(p)-1)/2)
				for i := range pcm {
					pcm[i] = float64(int16(binary.LittleEndian.Uint16(p[1+2*i:])))
				}
				x.power[0] += goertzel(pcm, hearHz, 16000)
				for _, v := range pcm {
					x.power[1] += v * v
				}
			case 1:
				var m map[string]any
				json.Unmarshal(p, &m)
				switch m["t"] {
				case "hello":
					out.Console, _ = m["console"].(string)
				case "ptt":
					if m["down"] == true {
						g, _ := m["granted"].(bool)
						out.Granted = &g
					}
				case "notice":
					out.Notices = append(out.Notices, fmt.Sprint(m["what"]))
				case "state":
					gs, _ := m["groups"].([]any)
					for _, x := range gs {
						gm, _ := x.(map[string]any)
						if t, _ := gm["talker"].(string); t != "" {
							out.Talker[int(gm["number"].(float64))] = t
						}
					}
				}
			case 9: // ping
			}
			mu.Unlock()
		}
	}()
	time.Sleep(listen)
	if talk != 0 {
		sendJSON(ptt(talk, true))
		frame := make([]byte, 640)
		t0 := time.Now()
		for i := 0; time.Since(t0) < talkFor; i++ {
			for j := 0; j < 320; j++ {
				v := 8000 * math.Sin(2*math.Pi*tone*float64(i*320+j)/16000)
				binary.LittleEndian.PutUint16(frame[2*j:], uint16(int16(v)))
			}
			send(2, frame)
			time.Sleep(time.Until(t0.Add(time.Duration(i+1) * 20 * time.Millisecond)))
		}
		sendJSON(ptt(talk, false))
		time.Sleep(500 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, x := range out.Heard {
		if x.power[1] > 0 {
			// Goertzel power over sum of squares, scaled so a pure tone is ~1.
			x.Share = math.Round(x.power[0]/(x.power[1]*160)*100) / 100
		}
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

func goertzel(xs []float64, hz, rate float64) float64 {
	k := 2 * math.Cos(2*math.Pi*hz/rate)
	var s1, s2 float64
	for _, x := range xs {
		s1, s2 = x+k*s1-s2, s1
	}
	return s1*s1 + s2*s2 - k*s1*s2
}
