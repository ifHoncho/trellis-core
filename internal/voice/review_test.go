package voice

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
	"time"

	"meridian/trellis/internal/rtp"
)

// Tests for the jitter buffer, consoles, rejoin, listen, VOX and PTT checks.

func seqPacket(seq uint16) rtp.Packet {
	p := make([]byte, 4)
	binary.BigEndian.PutUint16(p, seq)
	return rtp.Packet{Seq: seq, Payload: p}
}

// playAt runs a stream for d from t0, one frame every 20 ms, with each
// packet put at the time arrive gives, and returns when each sequence
// number was played.
func playAt(s *stream, t0 time.Time, d time.Duration, arrive map[uint16]time.Duration) map[uint16]time.Duration {
	played := map[uint16]time.Duration{}
	pcm := make([]int16, FrameLen)
	for at := time.Duration(0); at <= d; at += FrameMs * time.Millisecond {
		now := t0.Add(at)
		for seq, when := range arrive {
			if when == at {
				s.put(seqPacket(seq), now)
			}
		}
		if s.next(pcm, now) && s.misses == 0 {
			played[uint16(pcm[0])] = at
		}
	}
	return played
}

// Packets lost at the start of a talk do not become delay for the rest of
// it: the buffer is a time, not a count of packets.
func TestJitterLossAtStart(t *testing.T) {
	s := newStream(1, fakeDec{}, 3)
	arrive := map[uint16]time.Duration{100: 0}
	for seq := uint16(106); seq < 140; seq++ { // 101–105 lost
		arrive[seq] = time.Duration(seq-100) * FrameMs * time.Millisecond
	}
	played := playAt(s, time.Unix(1000, 0), time.Second, arrive)
	for _, seq := range []uint16{106, 120, 139} {
		at, ok := played[seq]
		if delay := at - arrive[seq]; !ok || delay > 80*time.Millisecond {
			t.Errorf("packet %d: played %v after it arrived (played: %v), want within the 60 ms buffer and a frame", seq, delay, ok)
		}
	}
}

// A gap of more than a second after a talk's first packet does not leave
// the talker silent for the rest of it.
func TestJitterLongGapAtStart(t *testing.T) {
	s := newStream(1, fakeDec{}, 3)
	arrive := map[uint16]time.Duration{100: 0}
	for seq := uint16(170); seq < 200; seq++ {
		arrive[seq] = time.Duration(seq-100) * FrameMs * time.Millisecond
	}
	played := playAt(s, time.Unix(1000, 0), 2*time.Second, arrive)
	if at, ok := played[180]; !ok || at-arrive[180] > 80*time.Millisecond {
		t.Errorf("packet 180 after a 1.4 s gap: played %v, %v after it arrived", ok, at-arrive[180])
	}
	// Not playing, something held, and a packet far past it: the run
	// starts over from that packet rather than dropping all that follows.
	s = newStream(1, fakeDec{}, 3)
	now := time.Unix(1000, 0)
	s.put(seqPacket(100), now)
	if r := s.put(seqPacket(100+maxAhead+10), now); r != putOK || s.due != 100+maxAhead+10 || len(s.buf) != 1 {
		t.Errorf("a packet far ahead before the start: result %v, due %d, %d held", r, s.due, len(s.buf))
	}
}

// A talk shorter than the buffer is still played: after the buffer's
// time, or at once when the talker said goodbye.
func TestJitterShortTalk(t *testing.T) {
	s := newStream(1, fakeDec{}, 3)
	played := playAt(s, time.Unix(1000, 0), 300*time.Millisecond, map[uint16]time.Duration{100: 0, 101: 20 * time.Millisecond})
	if len(played) != 2 || played[100] > 80*time.Millisecond {
		t.Errorf("a two-packet talk: played %v", played)
	}
	s = newStream(1, fakeDec{}, 3)
	now := time.Unix(1000, 0)
	s.put(seqPacket(100), now)
	s.bye = true
	if !s.next(make([]int16, FrameLen), now) {
		t.Error("a talk that ended is not played at once")
	}
}

// shortDec decodes half a frame, as a packet with a 10 ms frame does.
type shortDec struct{}

func (shortDec) Decode(p []byte, pcm []int16, fec bool) (int, error) {
	if p == nil {
		return 0, errors.New("nothing to conceal with")
	}
	for i := 0; i < len(pcm)/2; i++ {
		pcm[i] = 7
	}
	return len(pcm) / 2, nil
}
func (shortDec) Close() {}

// What a short frame does not fill is silence, not what the buffer held.
func TestShortFrameCleared(t *testing.T) {
	s := newStream(1, shortDec{}, 1)
	now := time.Unix(1000, 0)
	s.put(seqPacket(1), now)
	pcm := make([]int16, FrameLen)
	for i := range pcm {
		pcm[i] = 999 // another group's frame, left in the shared buffer
	}
	if !s.next(pcm, now) || pcm[0] != 7 || pcm[FrameLen/2] != 0 || pcm[FrameLen-1] != 0 {
		t.Errorf("frame: %d … %d … %d", pcm[0], pcm[FrameLen/2], pcm[FrameLen-1])
	}
}

// A console's press on a group it does not monitor is refused while
// someone talks there: the node listens to the group the console keys.
func TestConsoleKeyedGroupIsHeard(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	toc := b.add(t, cfg("toc", Channel{Group: 2, Volume: 100}, Channel{}))
	j := &fakeJoiner{joined: map[netip.AddrPort]bool{}}
	toc.SetJoiner(j)
	c, _ := toc.AddConsole("desk")
	if err := toc.ConsoleMonitor(c, []int{3}); err != nil {
		t.Fatal(err)
	}
	if err := toc.ConsoleSelect(c, 1); err != nil || !j.joined[DefaultAddr(1)] {
		t.Fatalf("choosing group 1 to talk on: %v, joined %v", err, j.joined)
	}
	a.SetPTT(0, true)
	for i := 0; i < 5; i++ {
		b.step(map[*Engine]int16{a: 1000})
	}
	if ok, _ := toc.ConsolePTT(c, 1, true); ok {
		t.Fatal("granted on a busy group the console does not monitor")
	}
	toc.ConsolePTT(c, 1, false)
	if err := toc.ConsoleSelect(c, 9); err == nil {
		t.Error("a group that does not exist was chosen")
	}
	// Another group chosen: the first is left.
	if err := toc.ConsoleSelect(c, 3); err != nil || j.joined[DefaultAddr(1)] {
		t.Errorf("choosing group 3: %v, joined %v", err, j.joined)
	}
	// A console that never said which group it keys: its first press joins
	// the group, so the next one knows.
	c2, _ := toc.AddConsole("old")
	toc.ConsolePTT(c2, 1, true)
	toc.ConsolePTT(c2, 1, false)
	for i := 0; i < 5; i++ {
		b.step(map[*Engine]int16{a: 1000})
	}
	if ok, _ := toc.ConsolePTT(c2, 1, true); ok {
		t.Error("the second press on a busy group was granted")
	}
}

// failJoiner refuses the first join.
type failJoiner struct {
	fakeJoiner
	fails int
}

func (j *failJoiner) Join(a []netip.AddrPort) error {
	if j.fails > 0 {
		j.fails--
		return errors.New("no")
	}
	return j.fakeJoiner.Join(a)
}

// A group that could not be joined is tried again.
func TestRejoinRetries(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	toc := b.add(t, cfg("toc", Channel{Group: 2, Volume: 100}, Channel{}))
	j := &failJoiner{fakeJoiner: fakeJoiner{joined: map[netip.AddrPort]bool{}}, fails: 1}
	toc.SetJoiner(j)
	c, _ := toc.AddConsole("desk")
	toc.ConsoleMonitor(c, []int{1})
	if len(j.joined) != 0 || toc.joined[1] {
		t.Fatalf("after a failed join: %v, engine %v", j.joined, toc.joined)
	}
	toc.ConsoleMonitor(c, []int{1})
	if !j.joined[DefaultAddr(1)] || !toc.joined[1] {
		t.Errorf("not joined at the second try: %v", j.joined)
	}
}

// A group that is a channel's, or is named twice, is listened to once: it
// plays in the channel's ear only.
func TestListenOnce(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	l := b.add(t, cfg("bravo", Channel{Group: 1, Ear: Left, Volume: 100}, Channel{}, 1, 3, 3))
	if got := l.cfg.Listen; len(got) != 1 || got[0] != 3 {
		t.Fatalf("listen %v, want [3]", got)
	}
	a.SetPTT(0, true)
	var left, right int16
	for i := 0; i < 15; i++ {
		left, right = lr(b.step(map[*Engine]int16{a: 1000})[l])
	}
	if left != 1000 || right != 0 {
		t.Errorf("group 1 in the left ear: left %d, right %d", left, right)
	}
}

// VOX listens before the AGC: a microphone under the threshold does not
// key however far the AGC plays it up.
func TestVOXBeforeAGC(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	c := cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{})
	c.PTT, c.MicAGC = "vox", true
	a := b.add(t, c)
	for i := 0; i < 200; i++ { // 4 s at -35 dBFS: the AGC adds some 15 dB
		b.step(map[*Engine]int16{a: 600})
		if a.Status().Channels[0].Sending {
			t.Fatalf("keyed after %d frames by a level under the threshold (AGC %+.1f dB)", i, a.Status().AGCDB)
		}
	}
	if a.Status().AGCDB < 6 {
		t.Fatalf("the AGC did not raise its gain (%.1f dB): the test proves nothing", a.Status().AGCDB)
	}
}

// A VOX talk ends at its limit, and VOX keys again only after the
// microphone has fallen quiet.
func TestVOXLimit(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	c := cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{})
	c.PTT = "vox"
	a := b.add(t, c)
	frames := int(voxLimit / (FrameMs * time.Millisecond))
	for i := 0; i < frames-5; i++ {
		b.step(map[*Engine]int16{a: 4000})
	}
	if !a.Status().Channels[0].Sending {
		t.Fatal("released before the limit")
	}
	for i := 0; i < 10; i++ {
		b.step(map[*Engine]int16{a: 4000})
	}
	if a.Status().Channels[0].Sending {
		t.Fatal("still keyed past the limit")
	}
	for i := 0; i < 100; i++ { // the noise goes on: not keyed again
		b.step(map[*Engine]int16{a: 4000})
		if a.Status().Channels[0].Sending {
			t.Fatal("keyed again while the microphone never fell quiet")
		}
	}
	for i := 0; i < 50; i++ {
		b.step(map[*Engine]int16{a: 100})
	}
	b.step(map[*Engine]int16{a: 4000})
	if !a.Status().Channels[0].Sending {
		t.Error("not keyed by speech after a pause")
	}
}

// Channel A's own PTT setting decides whether VOX keys it, before the
// node's.
func TestVOXOnlyAsChannelAsPTT(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	c := cfg("alpha", Channel{Group: 1, Volume: 100, PTT: "gpio:gpiochip0:17"}, Channel{})
	c.PTT = "vox"
	a := b.add(t, c)
	b.step(map[*Engine]int16{a: 4000})
	if a.Status().Channels[0].Sending {
		t.Error("VOX keyed a channel whose PTT is a line")
	}
}

// A PTT check says when it holds a press, so that a press-to-start PTT
// keeps it on until its next press.
func TestCheckRecording(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	if a.CheckRecording(0) {
		t.Fatal("recording with no check")
	}
	a.StartCheck(0)
	if ok, _ := a.SetPTT(0, true); ok || !a.CheckRecording(0) || a.CheckRecording(1) {
		t.Fatalf("press: granted %v, recording %v", ok, a.CheckRecording(0))
	}
	for i := 0; i < 30; i++ {
		b.step(map[*Engine]int16{a: 3000})
	}
	a.SetPTT(0, false)
	if st := a.Status().Check; a.CheckRecording(0) || st == nil || st.Verdict != "ok" {
		t.Errorf("after the release: recording %v, check %+v", a.CheckRecording(0), st)
	}
}
