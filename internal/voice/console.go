package voice

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"meridian/trellis/internal/rtp"
)

// Consoles: a TOC monitoring and keying talk groups from a browser
// (architecture §13, Voice). A console is one more operator on this node:
// it hears the groups it monitors, decoded and at 16 kHz, and may key one
// group at a time, under an SSRC and CNAME of its own, with the same busy
// and priority rules as the node's channels.

const (
	maxConsoles = 8
	// consoleTxLimit releases a console's PTT: a stuck key or a lost
	// browser must not hold a net.
	consoleTxLimit = 60 * time.Second
	consoleQueue   = 10 // frames of the console's microphone held (200 ms)
)

// ConsoleFrame is 20 ms of a group, for a console.
type ConsoleFrame struct {
	Group int
	PCM   []int16 // ConsoleFrameLen samples at 16 kHz
}

// Console is one console's session.
type Console struct {
	ID   int
	Name string
	// Frames carries what the console hears; the engine drops a frame
	// rather than wait for a slow reader.
	Frames chan ConsoleFrame
	// Notices carries what the console should tell its operator: "busy",
	// "tx-limit".
	Notices chan string

	monitor map[int]bool
	// group is the group it keys: chosen (ConsoleSelect) before the press,
	// so that the node listens to it and knows when it is busy.
	group int
	tx    channelState
	// all is an all-call under way: a sender per group (nil: none).
	all      map[int]*channelState
	allSince time.Time
	up       Upsampler
	down     map[int]*Downsampler
	mic      [][]int16 // 48 kHz frames waiting to be sent
}

// AddConsole starts a console session. name is how listeners see it
// (RTCP CNAME): the node's callsign and the console's name.
func (e *Engine) AddConsole(name string) (*Console, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.consoles) >= maxConsoles {
		return nil, fmt.Errorf("at most %d consoles on one node", maxConsoles)
	}
	enc, err := e.codec.NewEncoder(e.cfg.Bitrate)
	if err != nil {
		return nil, err
	}
	_ = enc.SetPacketLossPerc(e.fecPerc) // as the channels' encoders are now
	e.consoleSeq++
	c := &Console{ID: e.consoleSeq, Name: consoleName(e.cfg.Callsign, name), Frames: make(chan ConsoleFrame, 64), Notices: make(chan string, 8),
		monitor: map[int]bool{}, down: map[int]*Downsampler{}}
	c.tx.enc = enc
	e.consoles = append(e.consoles, c)
	return c, nil
}

// consoleName is callsign/name, kept to what a CNAME may carry.
func consoleName(callsign, name string) string {
	clean := make([]byte, 0, len(name))
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			clean = append(clean, byte(r))
		}
		if len(clean) == 24 {
			break
		}
	}
	if len(clean) == 0 {
		clean = []byte("console")
	}
	return callsign + "/" + string(clean)
}

// allCallNote marks an all-call in RTCP (SDES NOTE): Trellis receivers
// let it take a busy group over.
const allCallNote = "all-call"

// allCallRepeats is how many of an all-call's first frames carry the note
// again.
const allCallRepeats = 3

// ConsoleAllCall keys (or releases) every talk group at once: an urgent
// message to everyone. It is not refused on a busy group: Trellis nodes
// switch to it, with the override tone, and duck the rest. It returns the
// groups keyed.
func (e *Engine) ConsoleAllCall(c *Console, down bool) []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.consoleAllCall(c, down)
}

func (e *Engine) consoleAllCall(c *Console, down bool) []int {
	now := e.now()
	if !down {
		for n, t := range c.all {
			g := e.groups[n]
			g.tx--
			e.allRTCP(c, n, t, true)
			e.event(Event{At: now, Group: n, Kind: "talk-ended", SSRC: t.ssrc, Talker: c.Name})
		}
		c.all = nil
		return nil
	}
	if c.all != nil {
		return keys(c.all)
	}
	if c.tx.granted {
		e.consolePTT(c, false)
	}
	c.all, c.allSince, c.mic = map[int]*channelState{}, now, nil
	for n, g := range e.groups {
		t := &channelState{granted: true, ssrc: newSSRC(), seq: uint16(newSSRC()), ts: newSSRC(), started: now}
		c.all[n] = t
		g.tx++
		e.allRTCP(c, n, t, false)
	}
	e.event(Event{At: now, Kind: "all-call", Talker: c.Name})
	return keys(c.all)
}

func keys(m map[int]*channelState) []int {
	out := make([]int, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func (e *Engine) allRTCP(c *Console, n int, t *channelState, bye bool) {
	g := e.groups[n].g
	sr := rtp.SenderReport{SSRC: t.ssrc, NTPTime: ntpTime(e.now()), RTPTime: t.ts, PacketCount: t.packets, OctetCount: t.octets}
	if err := e.net.Send(netip.AddrPortFrom(g.Addr.Addr(), g.Addr.Port()+1), rtp.AppendCompoundNote(nil, sr, c.Name, allCallNote, bye)); err != nil {
		e.log.Debug("voice: RTCP not sent", "group", g.Number, "err", err)
	}
	t.lastRTCP = e.now()
}

// RemoveConsole ends a session: its talk ends, its groups are left.
func (e *Engine) RemoveConsole(c *Console) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c.all != nil {
		e.consoleAllCall(c, false)
	}
	if c.tx.granted {
		e.consolePTT(c, false)
	}
	for i, x := range e.consoles {
		if x == c {
			e.consoles = append(e.consoles[:i], e.consoles[i+1:]...)
			break
		}
	}
	c.tx.enc.Close()
	e.rejoin()
}

// ConsoleMonitor sets the groups a console hears.
func (e *Engine) ConsoleMonitor(c *Console, groups []int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := map[int]bool{}
	for _, n := range groups {
		if e.groups[n] == nil {
			return fmt.Errorf("no talk group %d", n)
		}
		m[n] = true
	}
	c.monitor = m
	e.rejoin()
	return nil
}

// ConsoleSelect chooses the group a console will key (0: none). The node
// joins it, so a press finds out whether someone talks on it: a press on a
// group never chosen has nothing to go by and is granted.
func (e *Engine) ConsoleSelect(c *Console, group int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if group != 0 && e.groups[group] == nil {
		return fmt.Errorf("no talk group %d", group)
	}
	if c.tx.granted || c.tx.pttDown {
		return errors.New("the console is keyed")
	}
	c.group = group
	e.rejoin()
	return nil
}

// ConsolePTT keys (or releases) a console on a group: refused, with a
// "busy" notice, when another node talks on it.
func (e *Engine) ConsolePTT(c *Console, group int, down bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if down {
		if e.groups[group] == nil {
			return false, fmt.Errorf("no talk group %d", group)
		}
		if c.all != nil {
			return false, errors.New("an all-call is under way")
		}
		if c.tx.granted && c.group != group {
			e.consolePTT(c, false)
		}
		if c.group != group {
			c.group = group
			e.rejoin() // heard from now on: the next press knows
		}
	}
	return e.consolePTT(c, down), nil
}

func (e *Engine) consolePTT(c *Console, down bool) bool {
	now := e.now()
	g := e.groups[c.group]
	if g == nil {
		return false
	}
	t := &c.tx
	if down == t.pttDown {
		return t.granted
	}
	t.pttDown = down
	if down {
		if now.Sub(g.lastRx) < busyFor && g.tx == 0 {
			notify(c, "busy")
			e.event(Event{At: now, Group: g.g.Number, Kind: "ptt-refused", Talker: c.Name})
			return false
		}
		t.granted = true
		t.ssrc, t.seq, t.ts = newSSRC(), uint16(newSSRC()), newSSRC()
		t.packets, t.octets, t.started = 0, 0, now
		c.mic = nil
		g.tx++
		e.consoleRTCP(c, false)
		return true
	}
	if t.granted {
		t.granted = false
		g.tx--
		e.consoleRTCP(c, true)
		e.event(Event{At: now, Group: g.g.Number, Kind: "talk-ended", SSRC: t.ssrc, Talker: c.Name})
	}
	return false
}

func notify(c *Console, s string) {
	select {
	case c.Notices <- s:
	default:
	}
}

func (e *Engine) consoleRTCP(c *Console, bye bool) {
	g := e.groups[c.group].g
	t := &c.tx
	sr := rtp.SenderReport{SSRC: t.ssrc, NTPTime: ntpTime(e.now()), RTPTime: t.ts, PacketCount: t.packets, OctetCount: t.octets}
	if err := e.net.Send(netip.AddrPortFrom(g.Addr.Addr(), g.Addr.Port()+1), rtp.AppendCompound(nil, sr, c.Name, bye)); err != nil {
		e.log.Debug("voice: RTCP not sent", "group", g.Number, "err", err)
	}
	t.lastRTCP = e.now()
}

// ConsoleAudio takes 20 ms of a console's microphone (ConsoleFrameLen
// samples at 16 kHz). It is kept only while the console is keyed.
func (e *Engine) ConsoleAudio(c *Console, pcm []int16) error {
	if len(pcm) != ConsoleFrameLen {
		return errors.New("a console frame is 20 ms at 16 kHz")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !c.tx.granted && c.all == nil {
		return nil
	}
	f := make([]int16, FrameLen)
	c.up.Process(pcm, f)
	if len(c.mic) >= consoleQueue {
		c.mic = c.mic[1:] // the oldest goes: the console is ahead
	}
	c.mic = append(c.mic, f)
	return nil
}

// Joiner joins and leaves group addresses as consoles need them (the
// transport: UDP.Join, UDP.Leave).
type Joiner interface {
	Join(addrs []netip.AddrPort) error
	Leave(addrs []netip.AddrPort)
}

// SetJoiner lets the engine join the groups consoles monitor.
func (e *Engine) SetJoiner(j Joiner) {
	e.mu.Lock()
	e.joiner = j
	e.mu.Unlock()
}

// rejoin joins the groups consoles monitor or key that the node does not
// use, and leaves those no console has any more. A group that could not
// be joined is tried again at the next call. Call with mu held.
func (e *Engine) rejoin() {
	used := map[int]bool{}
	for _, n := range e.used() {
		used[n] = true
	}
	want := map[int]bool{}
	for _, c := range e.consoles {
		for n := range c.monitor {
			if !used[n] {
				want[n] = true
			}
		}
		if c.group != 0 && !used[c.group] {
			want[c.group] = true // the group it keys is busy-checked too
		}
	}
	addrs := func(n int) []netip.AddrPort {
		a := e.groups[n].g.Addr
		return []netip.AddrPort{a, netip.AddrPortFrom(a.Addr(), a.Port()+1)}
	}
	for n := range e.joined {
		if !want[n] {
			if e.joiner != nil {
				e.joiner.Leave(addrs(n))
			}
			delete(e.joined, n)
		}
	}
	for n := range want {
		if e.joined[n] {
			continue
		}
		if e.joiner != nil {
			if err := e.joiner.Join(addrs(n)); err != nil {
				e.log.Warn("voice: cannot join a group a console has", "group", n, "err", err)
				e.joiner.Leave(addrs(n)) // what of it was joined
				continue
			}
		}
		e.joined[n] = true
	}
}

// consoleGroups are the groups consoles hear, beyond the node's own.
func (e *Engine) consoleGroups() []int {
	var out []int
	for n := range e.joined {
		out = append(out, n)
	}
	return out
}

// consolesSend sends one frame for each keyed console, and enforces the
// transmit limit. local gets what each keyed console says, for the other
// listeners on this node (multicast does not loop back). Call with mu held.
func (e *Engine) consolesSend(now time.Time, buf []byte, local map[int]localTalk) {
	for _, c := range e.consoles {
		if c.all != nil {
			e.allCallSend(c, now, buf, local)
			continue
		}
		t := &c.tx
		if !t.granted {
			continue
		}
		if now.Sub(t.started) > consoleTxLimit {
			e.consolePTT(c, false)
			notify(c, "tx-limit")
			continue
		}
		f := make([]int16, FrameLen) // silence when the browser fell behind
		if len(c.mic) > 0 {
			f, c.mic = c.mic[0], c.mic[1:]
		}
		g := e.groups[c.group].g
		n, err := t.enc.Encode(f, buf[rtp.HeaderLen:])
		if err != nil {
			e.log.Warn("voice: encode failed", "err", err)
			continue
		}
		p := rtp.Packet{PayloadType: PayloadType, Seq: t.seq, Timestamp: t.ts, SSRC: t.ssrc, Marker: t.packets == 0, Payload: buf[rtp.HeaderLen : rtp.HeaderLen+n]}
		if err := e.net.Send(g.Addr, p.Append(make([]byte, 0, rtp.HeaderLen+n))); err != nil {
			e.log.Debug("voice: RTP not sent", "group", g.Number, "err", err)
		}
		if t.packets == 0 {
			e.event(Event{At: now, Group: g.Number, Kind: "talk-sent", SSRC: t.ssrc, Talker: c.Name})
		}
		t.seq++
		t.ts += FrameLen
		t.packets++
		t.octets += uint32(n)
		if now.Sub(t.lastRTCP) >= rtcpEvery {
			e.consoleRTCP(c, false)
		}
		local[c.group] = localTalk{frame: f, console: c}
	}
}

// allCallSend sends one frame of an all-call to every group: encoded once,
// a packet per group under that group's SSRC. Call with mu held.
func (e *Engine) allCallSend(c *Console, now time.Time, buf []byte, local map[int]localTalk) {
	if now.Sub(c.allSince) > consoleTxLimit {
		e.consoleAllCall(c, false)
		notify(c, "tx-limit")
		return
	}
	f := make([]int16, FrameLen)
	if len(c.mic) > 0 {
		f, c.mic = c.mic[0], c.mic[1:]
	}
	n, err := c.tx.enc.Encode(f, buf[rtp.HeaderLen:])
	if err != nil {
		e.log.Warn("voice: encode failed", "err", err)
		return
	}
	payload := buf[rtp.HeaderLen : rtp.HeaderLen+n]
	for gn, t := range c.all {
		g := e.groups[gn].g
		p := rtp.Packet{PayloadType: PayloadType, Seq: t.seq, Timestamp: t.ts, SSRC: t.ssrc, Marker: t.packets == 0, Payload: payload}
		if err := e.net.Send(g.Addr, p.Append(make([]byte, 0, rtp.HeaderLen+n))); err != nil {
			e.log.Debug("voice: RTP not sent", "group", g.Number, "err", err)
		}
		t.seq++
		t.ts += FrameLen
		t.packets++
		t.octets += uint32(n)
		// The note that says "all-call" goes again with the first frames:
		// one lost packet must not leave a busy group deaf to it.
		if t.packets <= allCallRepeats || now.Sub(t.lastRTCP) >= rtcpEvery {
			e.allRTCP(c, gn, t, false)
		}
		local[gn] = localTalk{frame: f, console: c}
	}
}

// localTalk is a frame talked on this node (a channel's mic or a
// console's): heard by this node's other listeners.
type localTalk struct {
	frame   []int16
	channel int // 1 + the channel; 0 for a console
	console *Console
}

// consolesDeliver hands each console the frames of the groups it
// monitors: heard from the mesh, or talked on this node by someone else.
// Call with mu held.
func (e *Engine) consolesDeliver(played map[int][]int16, local map[int]localTalk) {
	for _, c := range e.consoles {
		for n := range c.monitor {
			f := played[n]
			if f == nil {
				if l, ok := local[n]; ok && l.console != c {
					f = l.frame
				}
			}
			if f == nil || (c.tx.granted && c.group == n) || c.all != nil {
				continue
			}
			d := c.down[n]
			if d == nil {
				d = &Downsampler{}
				c.down[n] = d
			}
			out := make([]int16, ConsoleFrameLen)
			d.Process(f, out)
			select {
			case c.Frames <- ConsoleFrame{Group: n, PCM: out}:
			default: // a slow console loses a frame, nobody else waits
			}
		}
	}
}

// ConsoleStatus is a console as Status shows it.
type ConsoleStatus struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Monitor []int  `json:"monitor"`
	Group   int    `json:"group,omitempty"`
	Sending bool   `json:"sending"`
	AllCall bool   `json:"all_call,omitempty"`
}
