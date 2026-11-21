package voice

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"
)

// fakeCodec carries a frame's first sample (and the one before it, as its
// "FEC") so a test can tell whose audio arrived where.
type fakeCodec struct{}

type fakeEnc struct {
	prev int16
	loss int
}

func (e *fakeEnc) Encode(pcm []int16, out []byte) (int, error) {
	binary.BigEndian.PutUint16(out, uint16(pcm[0]))
	binary.BigEndian.PutUint16(out[2:], uint16(e.prev))
	e.prev = pcm[0]
	return 4, nil
}
func (e *fakeEnc) SetPacketLossPerc(p int) error { e.loss = p; return nil }
func (e *fakeEnc) Close()                        {}

type fakeDec struct{}

func (fakeDec) Decode(p []byte, pcm []int16, fec bool) (int, error) {
	v := int16(0)
	switch {
	case len(p) < 4:
	case fec:
		v = int16(binary.BigEndian.Uint16(p[2:]))
	default:
		v = int16(binary.BigEndian.Uint16(p))
	}
	for i := range pcm {
		pcm[i] = v
	}
	return len(pcm), nil
}
func (fakeDec) Close() {}

func (fakeCodec) NewEncoder(int) (Encoder, error) { return &fakeEnc{}, nil }
func (fakeCodec) NewDecoder() (Decoder, error)    { return fakeDec{}, nil }

// bus is a multicast network for engines on one clock.
type bus struct {
	now     time.Time
	delay   time.Duration
	drop    func(n int) bool // the nth RTP packet sent
	sent    int
	queue   []queued
	engines []*Engine
}

type queued struct {
	at   time.Time
	from *Engine
	to   netip.AddrPort
	b    []byte
}

type busPort struct {
	b    *bus
	self **Engine
}

func (p busPort) Send(to netip.AddrPort, b []byte) error {
	if to.Port()%2 == 1 { // RTP (odd ports by default)
		p.b.sent++
		if p.b.drop != nil && p.b.drop(p.b.sent) {
			return nil
		}
	}
	p.b.queue = append(p.b.queue, queued{p.b.now.Add(p.b.delay), *p.self, to, append([]byte(nil), b...)})
	return nil
}

func (b *bus) add(t *testing.T, cfg Config) *Engine {
	t.Helper()
	var self *Engine
	e, err := New(cfg, fakeCodec{}, busPort{b, &self}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	self = e
	e.now = func() time.Time { return b.now }
	b.engines = append(b.engines, e)
	return e
}

// step advances one frame: delivers what is due, then every engine sends
// its mic value and mixes; it returns each engine's stereo frame.
func (b *bus) step(mic map[*Engine]int16) map[*Engine][]int16 {
	b.now = b.now.Add(FrameMs * time.Millisecond)
	var rest []queued
	for _, q := range b.queue {
		if q.at.After(b.now) {
			rest = append(rest, q)
			continue
		}
		for _, e := range b.engines {
			if e == q.from {
				continue
			}
			if listens(e, q.to) {
				e.Deliver(q.to, q.b)
			}
		}
	}
	b.queue = rest
	out := map[*Engine][]int16{}
	for _, e := range b.engines {
		m := make([]int16, FrameLen)
		for i := range m {
			m[i] = mic[e]
		}
		s := make([]int16, 2*FrameLen)
		e.tick(m, s, make([]byte, 1500))
		out[e] = s
	}
	return out
}

// listens: the node's own groups, and those joined for its consoles.
func listens(e *Engine, to netip.AddrPort) bool {
	for _, a := range e.Listening() {
		if a == to {
			return true
		}
	}
	for n := range e.joined {
		a := e.groups[n].g.Addr
		if to == a || to == netip.AddrPortFrom(a.Addr(), a.Port()+1) {
			return true
		}
	}
	return false
}

func groups(n int) []Group {
	var gs []Group
	for i := 1; i <= n; i++ {
		gs = append(gs, Group{Number: i})
	}
	return gs
}

func cfg(name string, a, b Channel, listen ...int) Config {
	return Config{Callsign: name, Groups: groups(3), Channels: [2]Channel{a, b}, Listen: listen}
}

func lr(f []int16) (int16, int16) { return f[0], f[1] }

// One talker, three listeners: the one on the group hears it in the
// channel's ear after the jitter buffer; one on another group hears
// nothing; groups are kept apart.
func TestTalkGroups(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0), delay: 5 * time.Millisecond}
	talker := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	left := b.add(t, cfg("bravo", Channel{Group: 1, Ear: Left, Volume: 100}, Channel{Group: 2, Ear: Right, Volume: 100}))
	other := b.add(t, cfg("charlie", Channel{Group: 2, Volume: 100}, Channel{}))
	if ok, err := talker.SetPTT(0, true); !ok || err != nil {
		t.Fatalf("PTT: %v %v", ok, err)
	}
	first := -1
	for i := 0; i < 20; i++ {
		out := b.step(map[*Engine]int16{talker: 1000})
		l, r := lr(out[left])
		if l == 1000 && first < 0 {
			first = i
		}
		if first >= 0 && (l != 1000 || r != 0) {
			t.Fatalf("frame %d: bravo heard L %d R %d", i, l, r)
		}
		if ol, or := lr(out[other]); ol != 0 || or != 0 {
			t.Fatalf("frame %d: charlie, on group 2, heard %d/%d", i, ol, or)
		}
	}
	// Prebuffer 60 ms = 3 frames, sent from the first tick.
	if first != 3 {
		t.Errorf("first heard at frame %d, want 3", first)
	}
	st := left.Status()
	if st.Groups[0].Talker != "alpha" || st.Groups[0].Received < 15 || st.Groups[0].Lost != 0 {
		t.Errorf("status: %+v", st.Groups[0])
	}
	talker.SetPTT(0, false)
	for i := 0; i < 10; i++ {
		b.step(nil)
	}
	if st := left.Status(); st.Groups[0].Talker != "" || !hasEvent(st.Events, "talk-ended") {
		t.Errorf("after release: %+v", st)
	}
}

func hasEvent(evs []Event, kind string) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// Two channels at once: group 1 in the left ear, group 2 in the right.
func TestTwoChannels(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	c := b.add(t, cfg("charlie", Channel{Group: 2, Volume: 100}, Channel{}))
	l := b.add(t, cfg("bravo", Channel{Group: 1, Ear: Left, Volume: 100}, Channel{Group: 2, Ear: Right, Volume: 100}, 3))
	a.SetPTT(0, true)
	c.SetPTT(0, true)
	var out map[*Engine][]int16
	for i := 0; i < 10; i++ {
		out = b.step(map[*Engine]int16{a: 1000, c: 2000})
	}
	if ll, rr := lr(out[l]); ll != 1000 || rr != 2000 {
		t.Errorf("left %d right %d, want 1000 and 2000", ll, rr)
	}
}

// PTT on a busy group is refused, with the busy tone; free again once the
// talker stops.
func TestBusyRefused(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	o := cfg("bravo", Channel{Group: 1, Volume: 100}, Channel{})
	o.Tones = true
	l := b.add(t, o)
	a.SetPTT(0, true)
	for i := 0; i < 5; i++ {
		b.step(map[*Engine]int16{a: 1000})
	}
	if ok, err := l.SetPTT(0, true); ok || err != nil {
		t.Fatalf("PTT on a busy group granted: %v %v", ok, err)
	}
	if !hasEvent(l.Status().Events, "ptt-refused") || l.tones[0].next() == nil {
		t.Error("no refusal event or busy tone")
	}
	// Still held: nothing sent.
	if st := l.Status(); st.Channels[0].Sending {
		t.Error("sending while refused")
	}
	l.SetPTT(0, false)
	a.SetPTT(0, false)
	for i := 0; i < 20; i++ {
		b.step(nil)
	}
	if ok, _ := l.SetPTT(0, true); !ok {
		t.Error("PTT refused on a quiet group")
	}
}

// Loss: FEC from the next packet fills a single loss, so the audio keeps
// its value; the loss is counted.
func TestLossFilled(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0), drop: func(n int) bool { return n%10 == 0 }}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	l := b.add(t, cfg("bravo", Channel{Group: 1, Volume: 100}, Channel{}))
	a.SetPTT(0, true)
	for i := 0; i < 60; i++ {
		out := b.step(map[*Engine]int16{a: 1000})
		if ll, _ := lr(out[l]); i > 3 && i < 58 && ll != 1000 {
			t.Fatalf("frame %d: %d", i, ll)
		}
	}
	if st := l.Status().Groups[0]; st.Lost < 4 || st.Lost > 6 {
		t.Errorf("lost %d, want 5 or so", st.Lost)
	}
}

func TestConfigValidate(t *testing.T) {
	ok := cfg("x", Channel{Group: 1, Volume: 50}, Channel{Group: 2, Volume: 50}, 3)
	ok.Defaults()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"same group twice": func(c *Config) { c.Channels[1].Group = 1 },
		"unknown group":    func(c *Config) { c.Channels[0].Group = 9 },
		"bad ear":          func(c *Config) { c.Channels[0].Ear = "middle" },
		"volume":           func(c *Config) { c.Channels[0].Volume = 101 },
		"duplicate number": func(c *Config) { c.Groups = append(c.Groups, Group{Number: 1, Addr: DefaultAddr(9)}) },
		"unicast address":  func(c *Config) { c.Groups[0].Addr = netip.MustParseAddrPort("192.0.2.1:5000") },
		"shared RTCP port": func(c *Config) {
			c.Groups[1].Addr = netip.AddrPortFrom(c.Groups[0].Addr.Addr(), c.Groups[0].Addr.Port()+1)
		},
		"unknown listen":    func(c *Config) { c.Listen = []int{7} },
		"bitrate too low":   func(c *Config) { c.Bitrate = 1000 },
		"group number zero": func(c *Config) { c.Groups[0].Number = 0 },
		"listen volume":     func(c *Config) { c.Volume = 300 },
	} {
		c := ok
		c.Groups = append([]Group(nil), ok.Groups...)
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if a := DefaultAddr(1); a.String() != "239.73.80.1:38801" {
		t.Errorf("DefaultAddr(1) = %v", a)
	}
}

func TestAdjustVolume(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	o := cfg("alpha", Channel{Group: 1, Volume: 98}, Channel{})
	o.Tones = true
	e := b.add(t, o)
	if v := e.AdjustVolume(0, 5); v != 100 || e.tones[0].next() == nil {
		t.Errorf("up: %d", v)
	}
	if v := e.AdjustVolume(0, -30); v != 70 || e.Status().Channels[0].Volume != 70 {
		t.Errorf("down: %d", v)
	}
}

// The headset checks: the mic level, a loopback of it, a test tone.
func TestHeadsetChecks(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	e := b.add(t, cfg("alpha", Channel{Group: 1, Ear: Left, Volume: 100}, Channel{}))
	out := b.step(map[*Engine]int16{e: 1000})
	if l, r := lr(out[e]); l != 0 || r != 0 {
		t.Fatalf("mic heard without loopback: %d %d", l, r)
	}
	if st := e.Status(); st.MicDBFS < -31 || st.MicDBFS > -30 || st.Loopback {
		t.Errorf("mic level %v dBFS (1000/32768 is -30.3), loopback %v", st.MicDBFS, st.Loopback)
	}
	e.Loopback(time.Second)
	if l, r := lr(b.step(map[*Engine]int16{e: 1000})[e]); l != 1000 || r != 1000 {
		t.Errorf("loopback: %d %d", l, r)
	}
	e.Loopback(0)
	e.TestTone(0)
	f := b.step(nil)[e]
	var left, right int
	for i := 0; i < len(f); i += 2 {
		left += abs(int(f[i]))
		right += abs(int(f[i+1]))
	}
	if left == 0 || right != 0 {
		t.Errorf("test tone: left %d right %d (channel a is the left ear)", left, right)
	}
}

// Mic gain scales what is sent; sidetone plays the talker's own voice,
// quietly, in the channel's ear.
func TestMicGainSidetone(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	c := cfg("alpha", Channel{Group: 1, Ear: Left, Volume: 100}, Channel{})
	c.MicGain, c.Sidetone = 6, true
	a := b.add(t, c)
	l := b.add(t, cfg("bravo", Channel{Group: 1, Volume: 100}, Channel{}))
	a.SetPTT(0, true)
	var out map[*Engine][]int16
	for i := 0; i < 8; i++ {
		out = b.step(map[*Engine]int16{a: 1000})
	}
	if v, _ := lr(out[l]); v < 1990 || v > 2000 {
		t.Errorf("+6 dB: heard %d, want about 1995", v)
	}
	if left, right := lr(out[a]); left < 300 || left > 340 || right != 0 {
		t.Errorf("sidetone: left %d (want about 319), right %d", left, right)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Flash override: while the priority group talks, the other channel is
// ducked by 12 dB, and the override tone plays in the priority channel's
// ear when its talk begins.
func TestPriorityOverride(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	gs := groups(3)
	gs[0].Priority = true
	mk := func(name string, a, bch Channel, tones bool) *Engine {
		c := Config{Callsign: name, Groups: gs, Channels: [2]Channel{a, bch}, Tones: tones}
		return b.add(t, c)
	}
	cmd := mk("cmd", Channel{Group: 1, Volume: 100}, Channel{}, false)
	alpha := mk("alpha", Channel{Group: 2, Volume: 100}, Channel{}, false)
	l := mk("bravo", Channel{Group: 1, Ear: Left, Volume: 100}, Channel{Group: 2, Ear: Right, Volume: 100}, true)
	alpha.SetPTT(0, true)
	var out map[*Engine][]int16
	for i := 0; i < 8; i++ {
		out = b.step(map[*Engine]int16{alpha: 2000})
	}
	if _, r := lr(out[l]); r != 2000 {
		t.Fatalf("alpha alone: right %d", r)
	}
	cmd.SetPTT(0, true)
	toned := false
	for i := 0; i < 8; i++ {
		out = b.step(map[*Engine]int16{alpha: 2000, cmd: 1000})
		if len(l.tones[0].samples) > 0 {
			toned = true
		}
	}
	if ll, r := lr(out[l]); ll != 1000 || r != 500 {
		t.Errorf("during the priority talk: left %d (want 1000), right %d (want 500, -12 dB)", ll, r)
	}
	if !toned {
		t.Error("no override tone")
	}
}

// FEC follows the loss heard: 15 % lost raises it to 40, a clean link
// brings it down to 10.
func TestAdaptiveFEC(t *testing.T) {
	lossy := true
	b := &bus{now: time.Unix(1000, 0), drop: func(n int) bool { return lossy && n%7 == 0 }}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	l := b.add(t, cfg("bravo", Channel{Group: 1, Volume: 100}, Channel{}))
	a.SetPTT(0, true)
	for i := 0; i < 600; i++ { // 12 s
		b.step(map[*Engine]int16{a: 1000})
	}
	if p := l.Status().FECLossPerc; p != 40 || l.ch[0].enc.(*fakeEnc).loss != 40 {
		t.Errorf("after 14 %% loss: %d", p)
	}
	lossy = false
	for i := 0; i < 1500; i++ { // 30 s
		b.step(map[*Engine]int16{a: 1000})
	}
	if p := l.Status().FECLossPerc; p != 10 {
		t.Errorf("after a clean link: %d", p)
	}
}

// A bridged radio is keyed while its channel's group plays, held through
// short gaps, and released after.
func TestPlayingKeysRadio(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	gw := b.add(t, cfg("gateway", Channel{Group: 1, Volume: 100}, Channel{}))
	if gw.Playing(0) {
		t.Fatal("keyed at rest")
	}
	a.SetPTT(0, true)
	for i := 0; i < 6; i++ {
		b.step(map[*Engine]int16{a: 1000})
	}
	if !gw.Playing(0) {
		t.Error("not keyed while the group plays")
	}
	a.SetPTT(0, false)
	for i := 0; i < 40; i++ {
		b.step(nil)
	}
	if gw.Playing(0) {
		t.Error("still keyed after the talk")
	}
}

// VOX: speech keys channel A, the hang time holds it through a pause, and
// silence releases it.
func TestVOX(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	c := cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{})
	c.PTT = "vox"
	a := b.add(t, c)
	l := b.add(t, cfg("bravo", Channel{Group: 1, Volume: 100}, Channel{}))
	b.step(map[*Engine]int16{a: 100}) // -50 dBFS: quiet
	if a.Status().Channels[0].Sending {
		t.Fatal("keyed by a quiet mic")
	}
	b.step(map[*Engine]int16{a: 4000}) // -18 dBFS
	if !a.Status().Channels[0].Sending {
		t.Fatal("not keyed by speech")
	}
	for i := 0; i < 20; i++ { // 400 ms pause: held
		b.step(map[*Engine]int16{a: 100})
	}
	if !a.Status().Channels[0].Sending {
		t.Error("released inside the hang time")
	}
	for i := 0; i < 30; i++ {
		b.step(map[*Engine]int16{a: 100})
	}
	if a.Status().Channels[0].Sending || l.Status().Groups[0].Received == 0 {
		t.Errorf("not released after the hang time, or nothing heard (%d)", l.Status().Groups[0].Received)
	}
}

// Two devices on one node (dual PTT, a headset each): each channel talks
// from its own mic and is heard on its own device.
func TestTwoDevices(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	dual := b.add(t, cfg("dual", Channel{Group: 1, Volume: 100}, Channel{Group: 2, Volume: 100}))
	l := b.add(t, cfg("bravo", Channel{Group: 1, Ear: Left, Volume: 100}, Channel{Group: 2, Ear: Right, Volume: 100}))
	dual.SetPTT(0, true)
	dual.SetPTT(1, true)
	for i := 0; i < 8; i++ {
		b.now = b.now.Add(FrameMs * time.Millisecond)
		for _, q := range b.queue {
			for _, a := range l.Listening() {
				if a == q.to {
					l.Deliver(q.to, q.b)
				}
			}
		}
		b.queue = nil
		ma, mb := make([]int16, FrameLen), make([]int16, FrameLen)
		for j := range ma {
			ma[j], mb[j] = 1000, 2000
		}
		dual.tickDevices([2][]int16{ma, mb}, [2][]int16{make([]int16, 2*FrameLen), make([]int16, 2*FrameLen)}, make([]byte, 1500))
		l.tick(make([]int16, FrameLen), make([]int16, 2*FrameLen), make([]byte, 1500))
	}
	st := l.Status()
	if st.Groups[0].Received == 0 || st.Groups[1].Received == 0 {
		t.Fatalf("not heard: %+v", st.Groups)
	}
	// The listener's frame: group 1 (from mic A) left, group 2 (mic B) right.
	out := make([]int16, 2*FrameLen)
	l.tick(make([]int16, FrameLen), out, make([]byte, 1500))
	if ll, rr := lr(out); ll != 1000 || rr != 2000 {
		t.Errorf("heard left %d right %d: each channel must send its own mic", ll, rr)
	}

	// Receiving on two devices: each group only on its own device.
	r := b.add(t, cfg("rx", Channel{Group: 1, Volume: 100}, Channel{Group: 2, Volume: 100}))
	for i := 0; i < 8; i++ {
		b.now = b.now.Add(FrameMs * time.Millisecond)
		for _, q := range b.queue {
			for _, a := range r.Listening() {
				if a == q.to {
					r.Deliver(q.to, q.b)
				}
			}
		}
		b.queue = nil
		ma, mb := make([]int16, FrameLen), make([]int16, FrameLen)
		for j := range ma {
			ma[j], mb[j] = 1000, 2000
		}
		dual.tickDevices([2][]int16{ma, mb}, [2][]int16{make([]int16, 2*FrameLen), make([]int16, 2*FrameLen)}, make([]byte, 1500))
		outs := [2][]int16{make([]int16, 2*FrameLen), make([]int16, 2*FrameLen)}
		r.tickDevices([2][]int16{make([]int16, FrameLen), make([]int16, FrameLen)}, outs, make([]byte, 1500))
		if i == 7 {
			if a, _ := lr(outs[0]); a != 1000 {
				t.Errorf("device A plays %d, want group 1's 1000", a)
			}
			if bb, _ := lr(outs[1]); bb != 2000 {
				t.Errorf("device B plays %d, want group 2's 2000", bb)
			}
		}
	}
}

// The AGC brings a quiet microphone (-36 dBFS) up toward -18 dBFS within
// a few seconds, leaves room noise (-48 dBFS, as measured on a USB
// adapter) alone, and does not raise a loud one.
func TestMicAGC(t *testing.T) {
	var a agc
	quiet := tone(FrameLen, 1000, SampleRate, 0)
	for i := range quiet {
		quiet[i] /= 15 // about -36 dBFS RMS
	}
	var last []int16
	for f := 0; f < 250; f++ { // 5 s
		x := append([]int16(nil), quiet...)
		a.process(x)
		last = x
	}
	if r := rmsOf(last) / 32768; r < 0.09 || r > 0.14 {
		t.Errorf("a quiet mic after 5 s: rms %.3f (%.1f dB gain), want about 0.125", r, a.dB())
	}
	var n agc
	noise := tone(FrameLen, 120, SampleRate, 0) // hum at about -48 dBFS
	for i := range noise {
		noise[i] /= 60
	}
	for f := 0; f < 100; f++ {
		n.process(append([]int16(nil), noise...))
	}
	if n.dB() != 0 {
		t.Errorf("noise raised by %.1f dB", n.dB())
	}
	var l agc
	loud := tone(FrameLen, 1000, SampleRate, 0) // -12 dBFS
	l.process(loud)
	if l.dB() != 0 {
		t.Errorf("a loud mic raised by %.1f dB", l.dB())
	}
}

// A headset-button PTT that shorts the mic: the press makes the raw
// microphone drop (rest -40 dBFS, held silent), so a second in the talk
// stops, the mic-cut tone plays even with tones off, and the channel is
// marked until a talk with sound. Mic gain does not hide it (it is
// measured before any gain). Never cut: a PTT that cannot short the mic
// (not watched), a microphone as silent at rest as held (a gated
// headset), or press-to-start mode.
func TestMicCut(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	c := cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{})
	c.MicGain = 20
	a := b.add(t, c)
	a.SetCutWatch([2]bool{true, false})
	l := b.add(t, cfg("bravo", Channel{Group: 1, Volume: 100}, Channel{}))
	rest := func(e *Engine, v int16) {
		for i := 0; i < 40; i++ {
			b.step(map[*Engine]int16{e: v})
		}
	}
	rest(a, 300)
	a.SetPTT(0, true)
	b.step(map[*Engine]int16{a: 20000}) // the press click
	for i := 0; i < cutFrames+5; i++ {
		b.step(map[*Engine]int16{a: 0})
	}
	st := a.Status().Channels[0]
	if st.Sending || !st.MicCut || len(a.tones[0].samples) == 0 {
		t.Fatalf("not cut: %+v", st)
	}
	rx := l.Status().Groups[0].Received
	for i := 0; i < 10; i++ {
		b.step(map[*Engine]int16{a: 0})
	}
	if l.Status().Groups[0].Received != rx {
		t.Error("still sending after the cut")
	}
	a.SetPTT(0, false)
	rest(a, 300)
	a.SetPTT(0, true)
	for i := 0; i < cutFrames+5; i++ {
		b.step(map[*Engine]int16{a: 1000})
	}
	if st := a.Status().Channels[0]; !st.Sending || st.MicCut {
		t.Errorf("a good talk: %+v", st)
	}
	a.SetPTT(0, false)

	never := func(name string, watch bool, restLevel int16, mode string) {
		t.Helper()
		c := cfg(name, Channel{Group: 2, Volume: 100}, Channel{})
		c.PTTMode = mode
		e := b.add(t, c)
		e.SetCutWatch([2]bool{watch, false})
		rest(e, restLevel)
		e.SetPTT(0, true)
		for i := 0; i < 3*cutFrames; i++ {
			b.step(map[*Engine]int16{e: 0})
		}
		if st := e.Status().Channels[0]; !st.Sending || st.MicCut {
			t.Errorf("%s: cut: %+v", name, st)
		}
		e.SetPTT(0, false)
	}
	never("cm108-pin", false, 300, "")   // proper PTT hardware, a silent second
	never("gated", true, 0, "")          // silent at rest too: nothing changed at the press
	never("toggle", true, 300, "toggle") // the mic works in press-to-start mode
}

// The PTT check: the next press transmits nothing; what the mic gives
// while held is judged and played back; a silent one is "cuts-mic", no
// press at all is "no-ptt".
func TestPTTCheck(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Ear: Left, Volume: 100}, Channel{}))
	l := b.add(t, cfg("bravo", Channel{Group: 1, Volume: 100}, Channel{}))
	run := func(level int16, frames int) *CheckStatus {
		t.Helper()
		if err := a.StartCheck(0); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			b.step(map[*Engine]int16{a: 300}) // the mic at rest
		}
		if ok, _ := a.SetPTT(0, true); ok {
			t.Fatal("a check press was granted")
		}
		for i := 0; i < frames; i++ {
			b.step(map[*Engine]int16{a: level})
		}
		a.SetPTT(0, false)
		heard := false
		for i := 0; i < frames+5; i++ {
			if v, _ := lr(b.step(nil)[a]); v != 0 {
				heard = true
			}
		}
		st := a.Status().Check
		if level > 100 && !heard {
			t.Error("nothing played back")
		}
		return st
	}
	if st := run(2000, 40); st == nil || st.Verdict != "ok" || st.State != "done" || st.Seconds != 0.8 {
		t.Errorf("speech: %+v", st)
	}
	if st := run(0, 40); st.Verdict != "cuts-mic" {
		t.Errorf("a dead mic: %+v", st)
	}
	if st := run(200, 40); st.Verdict != "quiet" {
		t.Errorf("a quiet mic: %+v", st)
	}
	if l.Status().Groups[0].Received != 0 {
		t.Error("a check transmitted")
	}
	a.StartCheck(0)
	for i := 0; i < int(checkWait/(FrameMs*time.Millisecond))+5; i++ {
		b.step(nil)
	}
	if st := a.Status().Check; st.Verdict != "no-ptt" {
		t.Errorf("no press: %+v", st)
	}
}

// Exact digital silence from the microphone for 3 s: no microphone.
func TestMicDead(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	for i := 0; i < 160; i++ {
		b.step(map[*Engine]int16{a: 0})
	}
	if !a.Status().MicDead {
		t.Error("no microphone not shown")
	}
	b.step(map[*Engine]int16{a: 3})
	if a.Status().MicDead {
		t.Error("still shown with a signal")
	}
}
