package voice

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"sync"
	"time"

	"meridian/trellis/internal/rtp"
)

// Capture gives one frame of microphone audio per call (FrameLen mono
// samples), blocking until it has one: it is the engine's clock.
type Capture interface {
	Read(frame []int16) error
}

// Playback takes one frame of stereo audio (2*FrameLen samples, left and
// right interleaved).
type Playback interface {
	Write(frame []int16) error
}

// Net sends packets to a group's address (RTP) or the port after it
// (RTCP). Received packets come in through Engine.Deliver.
type Net interface {
	Send(to netip.AddrPort, b []byte) error
}

// Encoder and Decoder are the codec (Opus on a node, internal/voice/vopus).
type Encoder interface {
	Encode(pcm []int16, out []byte) (int, error)
	SetPacketLossPerc(p int) error
	Close()
}

type Decoder interface {
	// Decode decodes a packet into pcm; with fec, the redundancy it
	// carries for the packet before it; with a nil packet, conceals a loss.
	Decode(packet []byte, pcm []int16, fec bool) (int, error)
	Close()
}

type Codec interface {
	NewEncoder(bitrate int) (Encoder, error)
	NewDecoder() (Decoder, error)
}

// Timing of the half-duplex gate and of receive streams.
const (
	busyFor      = 250 * time.Millisecond // a group with a packet this recent is busy
	streamQuiet  = 2 * time.Second        // a talker unheard this long is gone
	underrunStop = 10                     // frames missing in a row: the talk has ended
	maxAhead     = 50                     // frames a packet may be ahead of playout
	rtcpEvery    = 5 * time.Second
)

// Engine is one node's voice: its channels, the groups it listens to, and
// the network.
type Engine struct {
	log   *slog.Logger
	cfg   Config
	codec Codec
	net   Net
	now   func() time.Time

	mu     sync.Mutex
	groups map[int]*groupState // every group in the profile
	ch     [2]*channelState
	tones  [2]*tonePlayer // per channel
	events []Event

	micLevel, outLevel0, outLevel1 level
	outLevel                       [2]*level
	loopbackUntil                  time.Time
	audio                          *AudioInfo

	voxOn      bool // channel A is keyed by voice (VOX)
	voxUntil   time.Time
	voxBlocked bool   // released by voxLimit: waits for the microphone to fall quiet
	agcs       [2]agc // per microphone (device)
	// A PTT that cuts the mic (check.go): frames sent from a dead
	// microphone, frames with sound, when a talk was cut, the mic's level
	// at rest; the PTT check, and the frame it plays now.
	cutWatch   [2]bool // the channel's PTT can short its mic (a headset button)
	zeroFrames int     // frames in a row of exact digital silence from the first mic
	// localAt/localBy: the last frame talked on each group from this node
	// (a console, or a channel), and by whom: Status shows it.
	localAt     map[int]time.Time
	localBy     map[int]string
	heldDB      [2][]float64 // the raw mic's level, frame by frame, this talk's first second
	soundFrames [2]int
	cutAt       [2]time.Time
	releasedDB  [2]float64
	restSeen    [2]bool // releasedDB has its first reading
	check       *pttCheck
	checkOut    []int16

	// Consoles (console.go).
	consoles   []*Console
	consoleSeq int
	joiner     Joiner
	joined     map[int]bool // groups joined for consoles only
	// playingUntil: a channel's group played a frame lately (Playing).
	playingUntil [2]time.Time

	// Adaptive FEC (adaptFEC).
	fecAt          time.Time
	fecRx, fecLost uint64
	fecEWMA        float64
	fecPerc        int
}

// Event is a talk starting or ending, here or heard: the lab measures
// latency from them.
type Event struct {
	At      time.Time `json:"at"`
	Group   int       `json:"group"`
	Kind    string    `json:"kind"` // talk-sent, talk-heard, talk-ended, ptt-refused
	SSRC    uint32    `json:"ssrc"`
	Talker  string    `json:"talker,omitempty"`
	Channel string    `json:"channel,omitempty"`
}

type channelState struct {
	pttDown  bool
	granted  bool
	enc      Encoder
	ssrc     uint32
	seq      uint16
	ts       uint32
	packets  uint32
	octets   uint32
	lastRTCP time.Time
	started  time.Time
}

type groupState struct {
	g        Group
	lastRx   time.Time // any packet from another node
	talker   *stream
	tx       int // channels sending on it
	received uint64
	lost     uint64
	late     uint64
	names    map[uint32]string // CNAMEs by SSRC, from RTCP
	allCall  map[uint32]bool   // SSRCs that said they are an all-call (RTCP NOTE)
}

// New makes an engine. The codec makes one encoder per channel and a
// decoder per talker.
func New(cfg Config, codec Codec, n Net, log *slog.Logger) (*Engine, error) {
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{log: log, cfg: cfg, codec: codec, net: n, now: time.Now, groups: map[int]*groupState{}, joined: map[int]bool{}}
	e.outLevel = [2]*level{&e.outLevel0, &e.outLevel1}
	e.fecPerc = InitialLossPerc
	// VOX keys channel A when that is channel A's PTT: its own setting,
	// else the node's.
	pttA := cfg.Channels[0].PTT
	if pttA == "" {
		pttA = cfg.PTT
	}
	e.voxOn = pttA == "vox"
	for _, g := range cfg.Groups {
		e.groups[g.Number] = &groupState{g: g, names: map[uint32]string{}, allCall: map[uint32]bool{}}
	}
	for i := range e.ch {
		enc, err := codec.NewEncoder(cfg.Bitrate)
		if err != nil {
			return nil, err
		}
		e.ch[i] = &channelState{enc: enc}
		e.tones[i] = &tonePlayer{}
	}
	return e, nil
}

// Close frees the codec state.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.ch {
		c.enc.Close()
	}
	for _, g := range e.groups {
		if g.talker != nil {
			g.talker.dec.Close()
		}
	}
	for _, c := range e.consoles {
		c.tx.enc.Close()
	}
}

// Listening is every address the engine needs packets from: each group a
// channel or the listen list uses, RTP and RTCP.
func (e *Engine) Listening() []netip.AddrPort {
	var out []netip.AddrPort
	for _, n := range e.used() {
		a := e.groups[n].g.Addr
		out = append(out, a, netip.AddrPortFrom(a.Addr(), a.Port()+1))
	}
	return out
}

func (e *Engine) used() []int {
	var out []int
	seen := map[int]bool{}
	add := func(n int) {
		if n != 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, c := range e.cfg.Channels {
		add(c.Group)
	}
	for _, n := range e.cfg.Listen {
		add(n)
	}
	return out
}

func newSSRC() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// SetPTT presses or releases a channel's PTT (0 = A, 1 = B). A press on a
// busy group is refused: the busy tone plays and nothing is sent until the
// PTT is released and pressed again.
func (e *Engine) SetPTT(channel int, down bool) (granted bool, err error) {
	if channel < 0 || channel > 1 {
		return false, errors.New("no such channel")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.setPTT(channel, down)
}

// setPTT is SetPTT with mu held.
func (e *Engine) setPTT(channel int, down bool) (granted bool, err error) {
	c, cfg := e.ch[channel], e.cfg.Channels[channel]
	if down == c.pttDown {
		return c.granted, nil
	}
	if e.checkPTT(channel, down) {
		c.pttDown = down
		return false, nil
	}
	c.pttDown = down
	now := e.now()
	name := "ab"[channel : channel+1]
	if cfg.Group == 0 {
		return false, fmt.Errorf("channel %s has no talk group", name)
	}
	g := e.groups[cfg.Group]
	if down {
		if now.Sub(g.lastRx) < busyFor && g.tx == 0 {
			c.granted = false
			e.tone(channel, toneBusy)
			e.event(Event{At: now, Group: g.g.Number, Kind: "ptt-refused", Channel: name})
			return false, nil
		}
		c.granted = true
		e.heldDB[channel], e.soundFrames[channel] = e.heldDB[channel][:0], 0
		c.ssrc, c.seq, c.ts = newSSRC(), uint16(newSSRC()), newSSRC()
		c.packets, c.octets, c.started = 0, 0, now
		g.tx++
		e.tone(channel, toneGrant)
		e.sendRTCP(channel, false)
		return true, nil
	}
	if c.granted {
		c.granted = false
		g.tx--
		e.sendRTCP(channel, true)
		e.tone(channel, toneRelease)
		e.event(Event{At: now, Group: g.g.Number, Kind: "talk-ended", SSRC: c.ssrc, Talker: e.cfg.Callsign, Channel: name})
	}
	return false, nil
}

// Groups are the mesh's talk groups.
func (e *Engine) Groups() []Group { return append([]Group(nil), e.cfg.Groups...) }

// SetAudio records the audio hardware in use, for Status.
func (e *Engine) SetAudio(a AudioInfo) {
	e.mu.Lock()
	e.audio = &a
	e.mu.Unlock()
}

// TestTone plays a 1 kHz tone for a second in a channel's ear, here only.
func (e *Engine) TestTone(channel int) error {
	if channel < 0 || channel > 1 {
		return errors.New("no such channel")
	}
	e.mu.Lock()
	e.tones[channel].start(toneTest)
	e.mu.Unlock()
	return nil
}

// Loopback plays the microphone back here, in both ears, for d (0 stops
// it): a headset check. Nothing goes on the network.
func (e *Engine) Loopback(d time.Duration) {
	e.mu.Lock()
	e.loopbackUntil = e.now().Add(min(d, 30*time.Second))
	e.mu.Unlock()
}

// AdjustVolume changes a channel's volume by delta (clamped to 0–100),
// with a short tone at the new level, and returns it: the volume buttons.
func (e *Engine) AdjustVolume(channel, delta int) int {
	if channel < 0 || channel > 1 {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	v := min(100, max(0, e.cfg.Channels[channel].Volume+delta))
	e.cfg.Channels[channel].Volume = v
	e.tone(channel, toneVolume)
	return v
}

func (e *Engine) tone(channel int, t []toneStep) {
	if e.cfg.Tones {
		e.tones[channel].start(t)
	}
}

func (e *Engine) event(ev Event) {
	e.events = append(e.events, ev)
	if len(e.events) > 256 {
		e.events = e.events[len(e.events)-256:]
	}
	e.log.Info("voice "+ev.Kind, "group", ev.Group, "ssrc", ev.SSRC, "talker", ev.Talker, "channel", ev.Channel, "at_ns", ev.At.UnixNano())
}

// sendRTCP sends a sender report and the CNAME for a channel's talk, with
// a BYE when it ends. Call with mu held.
func (e *Engine) sendRTCP(channel int, bye bool) {
	c := e.ch[channel]
	g := e.groups[e.cfg.Channels[channel].Group].g
	now := e.now()
	sr := rtp.SenderReport{SSRC: c.ssrc, NTPTime: ntpTime(now), RTPTime: c.ts, PacketCount: c.packets, OctetCount: c.octets}
	b := rtp.AppendCompound(nil, sr, e.cfg.Callsign, bye)
	if err := e.net.Send(netip.AddrPortFrom(g.Addr.Addr(), g.Addr.Port()+1), b); err != nil {
		e.log.Debug("voice: RTCP not sent", "group", g.Number, "err", err)
	}
	c.lastRTCP = now
}

func ntpTime(t time.Time) uint64 {
	const epoch = 2208988800 // 1900 to 1970
	s := uint64(t.Unix()) + epoch
	f := uint64(t.Nanosecond()) << 32 / 1e9
	return s<<32 | f
}

// Deliver hands the engine a packet received for address to. It is safe
// to call from any goroutine.
func (e *Engine) Deliver(to netip.AddrPort, b []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for _, g := range e.groups {
		switch to {
		case g.g.Addr:
			e.deliverRTP(g, b, now)
			return
		case netip.AddrPortFrom(g.g.Addr.Addr(), g.g.Addr.Port()+1):
			e.deliverRTCP(g, b, now)
			return
		}
	}
}

func (e *Engine) deliverRTP(g *groupState, b []byte, now time.Time) {
	p, err := rtp.Parse(b)
	if err != nil || p.PayloadType != PayloadType {
		return
	}
	for _, c := range e.ch {
		if c.granted && p.SSRC == c.ssrc {
			return // our own, looped back
		}
	}
	g.lastRx = now
	if g.tx > 0 {
		return // we are talking on this group: half-duplex
	}
	if g.talker == nil || g.talker.ssrc != p.SSRC {
		// First heard wins, but an all-call takes the group over.
		if g.talker != nil && now.Sub(g.talker.lastArrival) < busyFor && !(g.allCall[p.SSRC] && !g.allCall[g.talker.ssrc]) {
			return
		}
		if g.talker != nil {
			g.talker.dec.Close()
		}
		dec, err := e.codec.NewDecoder()
		if err != nil {
			e.log.Warn("voice: no decoder", "err", err)
			return
		}
		g.talker = newStream(p.SSRC, dec, e.cfg.JitterMs/FrameMs)
	}
	switch g.talker.put(p, now) {
	case putLate:
		g.late++
	case putOK:
		g.received++
	}
}

func (e *Engine) deliverRTCP(g *groupState, b []byte, now time.Time) {
	r, err := rtp.ParseCompound(b)
	if err != nil {
		return
	}
	// Both maps are bounded; when full, everything goes but the talker
	// now playing (its name and its all-call must outlast the others).
	keep := func(ssrc uint32) bool { return g.talker != nil && g.talker.ssrc == ssrc }
	for ssrc, name := range r.CNAME {
		if len(g.names) > 64 {
			for k := range g.names {
				if !keep(k) {
					delete(g.names, k)
				}
			}
		}
		g.names[ssrc] = name
	}
	for ssrc, note := range r.Note {
		if note == allCallNote {
			if len(g.allCall) > 64 {
				for k := range g.allCall {
					if !keep(k) {
						delete(g.allCall, k)
					}
				}
			}
			g.allCall[ssrc] = true
		}
	}
	for _, ssrc := range r.Bye {
		if g.talker != nil && g.talker.ssrc == ssrc {
			g.talker.bye = true
		}
	}
	_ = now
}

// Run runs the engine on one audio device until ctx ends or the audio
// fails.
func (e *Engine) Run(ctx context.Context, in Capture, out Playback) error {
	return e.RunDevices(ctx, [2]Capture{in}, [2]Playback{out})
}

// RunDevices runs the engine with a device per channel: channel A's on
// in[0] and out[0], channel B's on in[1] and out[1] (two headsets, each
// with its PTT: dual PTT). With in[1] nil, both channels share the first
// device. The first device's capture is the clock; listen-only groups
// play on it.
func (e *Engine) RunDevices(ctx context.Context, in [2]Capture, out [2]Playback) error {
	var mics, frames [2][]int16
	for i := range mics {
		mics[i] = make([]int16, FrameLen)
		frames[i] = make([]int16, 2*FrameLen)
	}
	buf := make([]byte, 1500)
	two := in[1] != nil && out[1] != nil
	for ctx.Err() == nil {
		for i, c := range in {
			if c == nil {
				continue
			}
			if err := c.Read(mics[i]); err != nil {
				return fmt.Errorf("capture: %w", err)
			}
		}
		if !two {
			e.tickDevices([2][]int16{mics[0], mics[0]}, [2][]int16{frames[0], nil}, buf)
		} else {
			e.tickDevices(mics, frames, buf)
		}
		for i, p := range out {
			if p == nil || (!two && i == 1) {
				continue
			}
			if err := p.Write(frames[i]); err != nil {
				return fmt.Errorf("playback: %w", err)
			}
		}
	}
	return ctx.Err()
}

// tick is one frame on one device.
func (e *Engine) tick(mic, stereo []int16, buf []byte) {
	e.tickDevices([2][]int16{mic, mic}, [2][]int16{stereo, nil}, buf)
}

// tickDevices sends one frame for each talking channel, from its own mic,
// and mixes one frame of everything heard: each channel into its own
// device's frame when there are two (outs[1] non-nil), else all into
// outs[0].
func (e *Engine) tickDevices(mics, outs [2][]int16, buf []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	// The raw microphone, before any gain: what the PTT checks look at.
	var rawDB [2]float64
	for i := range mics {
		var l level
		l.add(mics[i], now)
		rawDB[i] = l.dbfs()
	}
	allZero := true
	for _, v := range mics[0] {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		e.zeroFrames++
	} else {
		e.zeroFrames = 0
	}
	var raw []int16
	if k := e.check; k != nil && k.State == "recording" {
		raw = append([]int16(nil), mics[k.ch]...)
	}
	e.micCutCheck(rawDB, now)
	if e.cfg.MicGain != 0 {
		g := float32(math.Pow(10, float64(e.cfg.MicGain)/20))
		for i, mic := range mics {
			if i == 1 && &mics[1][0] == &mics[0][0] {
				break // one device: the same slice, scaled once
			}
			for j, v := range mic {
				mic[j] = clip(float32(v) * g)
			}
		}
	}
	// VOX listens here: after the microphone gain the operator set, before
	// the AGC, whose gain (held through silence) would play the room's
	// noise up over the threshold.
	var voxLevel level
	if e.voxOn {
		voxLevel.add(mics[0], now)
	}
	if e.cfg.MicAGC {
		e.agcs[0].process(mics[0])
		if outs[1] != nil && &mics[1][0] != &mics[0][0] {
			e.agcs[1].process(mics[1])
		}
	}
	if k := e.check; k != nil {
		var sent []int16
		if k.State == "recording" {
			sent = mics[k.ch]
		}
		e.checkTick(raw, sent, now)
	}
	mic := mics[0]
	stereo := outs[0]
	out := func(ch int) []int16 {
		if outs[1] != nil {
			return outs[ch]
		}
		return outs[0]
	}
	if e.voxOn {
		e.vox(voxLevel.dbfs(), now)
	}
	local := map[int]localTalk{}
	for i, c := range e.ch {
		if !c.granted {
			continue
		}
		g := e.groups[e.cfg.Channels[i].Group].g
		local[g.Number] = localTalk{frame: mics[i], channel: i + 1}
		n, err := c.enc.Encode(mics[i], buf[rtp.HeaderLen:])
		if err != nil {
			e.log.Warn("voice: encode failed", "err", err)
			continue
		}
		p := rtp.Packet{PayloadType: PayloadType, Seq: c.seq, Timestamp: c.ts, SSRC: c.ssrc, Marker: c.packets == 0, Payload: buf[rtp.HeaderLen : rtp.HeaderLen+n]}
		pkt := p.Append(make([]byte, 0, rtp.HeaderLen+n))
		if err := e.net.Send(g.Addr, pkt); err != nil {
			e.log.Debug("voice: RTP not sent", "group", g.Number, "err", err)
		}
		if c.packets == 0 {
			e.event(Event{At: now, Group: g.Number, Kind: "talk-sent", SSRC: c.ssrc, Talker: e.cfg.Callsign, Channel: "ab"[i : i+1]})
		}
		c.seq++
		c.ts += FrameLen
		c.packets++
		c.octets += uint32(n)
		if now.Sub(c.lastRTCP) >= rtcpEvery {
			e.sendRTCP(i, false)
		}
	}

	e.consolesSend(now, buf, local)
	e.micLevel.add(mic, now)
	clear(outs[0])
	if outs[1] != nil {
		clear(outs[1])
	}
	frame := make([]int16, FrameLen)
	played := map[int][]int16{}
	override := map[int]bool{} // priority groups whose talk began this frame
	pull := func(n int) []int16 {
		if f, ok := played[n]; ok {
			return f
		}
		g := e.groups[n]
		var f []int16
		if s := g.talker; s != nil {
			if s.done(now) {
				e.event(Event{At: now, Group: n, Kind: "talk-ended", SSRC: s.ssrc, Talker: g.names[s.ssrc]})
				s.dec.Close()
				g.talker = nil
			} else if s.next(frame, now) {
				if !s.heard {
					s.heard = true
					e.event(Event{At: now, Group: n, Kind: "talk-heard", SSRC: s.ssrc, Talker: g.names[s.ssrc]})
					override[n] = g.g.Priority || g.allCall[s.ssrc]
				}
				f = append([]int16(nil), frame...)
				g.lost += uint64(s.takeLost())
			}
		}
		played[n] = f
		return f
	}
	// Everything heard this frame, first: a priority group that is talking
	// ducks the others by 12 dB (flash override), and its talk starting
	// plays the override tone in its ear.
	for _, ch := range e.cfg.Channels {
		if ch.Group != 0 {
			pull(ch.Group)
		}
	}
	for _, n := range e.cfg.Listen {
		pull(n)
	}
	for _, n := range e.consoleGroups() {
		pull(n)
	}
	// heard is what a listener on this node hears of group n: from the
	// mesh, else talked here by someone other than itself (self: 1 + its
	// channel, or 0; con: the console, or nil).
	heard := func(n, self int, con *Console) []int16 {
		if f := played[n]; f != nil {
			return f
		}
		l, ok := local[n]
		switch {
		case !ok:
			return nil
		case l.channel != 0 && l.channel == self, l.console != nil && l.console == con:
			return nil // one's own voice
		}
		return l.frame
	}
	// An all-call heard counts as a priority talk: it ducks the rest.
	urgent := func(n int) bool {
		g := e.groups[n]
		if g.g.Priority {
			return true
		}
		if t := g.talker; t != nil && played[n] != nil && g.allCall[t.ssrc] {
			return true
		}
		l, ok := local[n]
		return ok && l.console != nil && l.console.all != nil
	}
	ducked := false
	for _, n := range e.used() {
		if heard(n, 0, nil) != nil && urgent(n) {
			ducked = true
		}
	}
	vol := func(n, v int) int {
		if ducked && !urgent(n) {
			return v / 2 // the squared curve: a quarter, -12 dB
		}
		return v
	}
	for i, ch := range e.cfg.Channels {
		var got []int16
		if ch.Group != 0 {
			if f := heard(ch.Group, i+1, nil); f != nil {
				mixInto(out(i), f, ch.Ear, vol(ch.Group, ch.Volume))
				got = f
			}
			if override[ch.Group] {
				e.tone(i, toneOverride)
			}
		}
		if e.cfg.Sidetone && e.ch[i].granted {
			mixInto(out(i), mics[i], ch.Ear, sidetoneVolume) // hear yourself, quietly
		}
		e.outLevel[i].add(got, now)
		if got != nil {
			e.playingUntil[i] = now.Add(keyHang)
		}
		if f := e.tones[i].next(); f != nil {
			mixInto(out(i), f, ch.Ear, 100)
		}
		if e.check != nil && e.check.ch == i && e.checkOut != nil {
			mixInto(out(i), e.checkOut, ch.Ear, 100) // the PTT check plays back
			e.checkOut = nil
		}
	}
	if now.Before(e.loopbackUntil) {
		mixInto(stereo, mic, Both, 100)
		if outs[1] != nil {
			mixInto(outs[1], mics[1], Both, 100)
		}
	}
	for _, n := range e.cfg.Listen {
		if f := heard(n, 0, nil); f != nil {
			mixInto(stereo, f, Both, vol(n, e.cfg.Volume))
		}
	}
	e.consolesDeliver(played, local)
	if e.localAt == nil {
		e.localAt, e.localBy = map[int]time.Time{}, map[int]string{}
	}
	for n, l := range local {
		e.localAt[n] = now
		if l.console != nil {
			e.localBy[n] = l.console.Name
		} else {
			e.localBy[n] = "channel " + "ab"[l.channel-1:l.channel]
		}
	}
	e.adaptFEC(now)
}

// sidetoneVolume is how loud a talker hears their own voice (-16 dB).
const sidetoneVolume = 40

// keyHang holds a radio keyed through the gaps of a talk (Playing).
const keyHang = 300 * time.Millisecond

// Playing reports whether a channel's group is playing, or played within
// keyHang: what keys a bridged radio's transmitter.
func (e *Engine) Playing(channel int) bool {
	if channel < 0 || channel > 1 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now().Before(e.playingUntil[channel])
}

// VOX: the microphone above voxThreshold keys channel A, and holds it
// for voxHang after it falls quiet. The frame that crosses the threshold
// is sent. A talk that has lasted voxLimit is released (a noise that
// never stops must not hold a group), and VOX keys again only after the
// microphone has fallen quiet.
const (
	voxThreshold = -30.0 // dBFS
	voxHang      = 800 * time.Millisecond
	voxLimit     = 60 * time.Second
)

// vox takes the microphone's level this frame (after the microphone
// gain, before the AGC). Call with mu held.
func (e *Engine) vox(db float64, now time.Time) {
	if db > voxThreshold {
		e.voxUntil = now.Add(voxHang)
	}
	want := now.Before(e.voxUntil)
	c := e.ch[0]
	if want && c.granted && now.Sub(c.started) > voxLimit {
		e.voxBlocked = true
		e.log.Warn("voice: VOX has keyed channel A for its limit; released until the microphone falls quiet", "limit", voxLimit)
	}
	if e.voxBlocked {
		if !want {
			e.voxBlocked = false
		}
		want = false
	}
	if want != c.pttDown {
		_, _ = e.setPTT(0, want)
	}
}

// adaptFEC sets how much redundancy the encoders spend from the loss this
// node hears on its groups (a stand-in for the loss its own packets meet:
// the mesh's links are mostly symmetric), every 2 s with enough traffic:
// under 2 % lost → 10, under 5 % → 20, under 10 % → 30, else 40.
func (e *Engine) adaptFEC(now time.Time) {
	if now.Sub(e.fecAt) < 2*time.Second {
		return
	}
	var rx, lost uint64
	for _, g := range e.groups {
		rx += g.received
		lost += g.lost
	}
	drx, dlost := rx-e.fecRx, lost-e.fecLost
	e.fecAt, e.fecRx, e.fecLost = now, rx, lost
	if drx+dlost < 50 {
		return
	}
	rate := float64(dlost) / float64(drx+dlost)
	e.fecEWMA = 0.7*e.fecEWMA + 0.3*rate
	perc := 40
	switch {
	case e.fecEWMA < 0.02:
		perc = 10
	case e.fecEWMA < 0.05:
		perc = 20
	case e.fecEWMA < 0.10:
		perc = 30
	}
	if perc == e.fecPerc {
		return
	}
	e.fecPerc = perc
	for _, c := range e.ch {
		_ = c.enc.SetPacketLossPerc(perc)
	}
	for _, c := range e.consoles {
		_ = c.tx.enc.SetPacketLossPerc(perc)
	}
	e.log.Info("voice: FEC follows the loss heard", "loss_pct", math.Round(e.fecEWMA*1000)/10, "fec_loss_perc", perc)
}

// mixInto adds mono into the stereo frame on the given ear, at volume
// 0–100 (a gentle curve: 50 is about -12 dB), clipping at full scale.
func mixInto(stereo, mono []int16, ear Ear, volume int) {
	gain := float32(math.Pow(float64(volume)/100, 2))
	for i, s := range mono {
		v := float32(s) * gain
		if ear != Right {
			stereo[2*i] = clip(float32(stereo[2*i]) + v)
		}
		if ear != Left {
			stereo[2*i+1] = clip(float32(stereo[2*i+1]) + v)
		}
	}
}

func clip(v float32) int16 {
	switch {
	case v > math.MaxInt16:
		return math.MaxInt16
	case v < math.MinInt16:
		return math.MinInt16
	}
	return int16(v)
}
