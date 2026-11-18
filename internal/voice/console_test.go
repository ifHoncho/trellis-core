package voice

import (
	"net/netip"
	"testing"
	"time"
)

type fakeJoiner struct{ joined map[netip.AddrPort]bool }

func (j *fakeJoiner) Join(a []netip.AddrPort) error {
	for _, x := range a {
		j.joined[x] = true
	}
	return nil
}
func (j *fakeJoiner) Leave(a []netip.AddrPort) {
	for _, x := range a {
		delete(j.joined, x)
	}
}

func drain(c *Console) map[int][]int16 {
	last := map[int][]int16{}
	for {
		select {
		case f := <-c.Frames:
			last[f.Group] = f.PCM
		default:
			return last
		}
	}
}

func consoleFrame(v int16) []int16 {
	f := make([]int16, ConsoleFrameLen)
	for i := range f {
		f[i] = v
	}
	return f
}

// A console on a node that hears only group 2 monitors groups 1 and 3:
// the node joins them, and the console hears group 1's talker.
func TestConsoleMonitors(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	toc := b.add(t, cfg("toc", Channel{Group: 2, Volume: 100}, Channel{}))
	j := &fakeJoiner{joined: map[netip.AddrPort]bool{}}
	toc.SetJoiner(j)
	c, err := toc.AddConsole("desk 1")
	if err != nil || c.Name != "toc/desk1" {
		t.Fatalf("%v %v", c, err)
	}
	if err := toc.ConsoleMonitor(c, []int{1, 3}); err != nil {
		t.Fatal(err)
	}
	if !j.joined[DefaultAddr(1)] || !j.joined[DefaultAddr(3)] || j.joined[DefaultAddr(2)] || len(j.joined) != 4 {
		t.Errorf("joined %v", j.joined)
	}
	a.SetPTT(0, true)
	var heard map[int][]int16
	for i := 0; i < 15; i++ {
		b.step(map[*Engine]int16{a: 1000})
		if h := drain(c); h[1] != nil {
			heard = h
		}
	}
	if heard == nil || heard[1][ConsoleFrameLen-1] < 990 || heard[1][ConsoleFrameLen-1] > 1010 {
		t.Fatalf("console heard %v", heard)
	}
	if st := toc.Status(); len(st.Consoles) != 1 || st.Consoles[0].Name != "toc/desk1" || len(st.Consoles[0].Monitor) != 2 {
		t.Errorf("status %+v", st.Consoles)
	}
	toc.RemoveConsole(c)
	if len(j.joined) != 0 {
		t.Errorf("left %v", j.joined)
	}
}

// A console keys group 2: a node on group 2 across the mesh hears it, and
// so does the console's own node's channel on group 2 (multicast does not
// loop back); the talker is the console's name.
func TestConsoleTalks(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	far := b.add(t, cfg("far", Channel{Group: 2, Volume: 100}, Channel{}))
	toc := b.add(t, cfg("toc", Channel{Group: 2, Ear: Left, Volume: 100}, Channel{}))
	c, _ := toc.AddConsole("desk")
	if ok, err := toc.ConsolePTT(c, 2, true); !ok || err != nil {
		t.Fatalf("PTT %v %v", ok, err)
	}
	var out map[*Engine][]int16
	for i := 0; i < 12; i++ {
		toc.ConsoleAudio(c, consoleFrame(1000))
		out = b.step(nil)
	}
	if v, _ := lr(out[far]); v < 990 || v > 1010 {
		t.Errorf("far node heard %d", v)
	}
	if v, _ := lr(out[toc]); v < 990 || v > 1010 {
		t.Errorf("the console's own node heard %d", v)
	}
	if st := far.Status(); st.Groups[1].Talker != "toc/desk" {
		t.Errorf("talker %q", st.Groups[1].Talker)
	}
	// The console's own node shows who talks: not "receiving".
	if lt := toc.Status().Channels[0].LocalTalker; lt != "toc/desk" {
		t.Errorf("local talker %q", lt)
	}
	toc.ConsolePTT(c, 2, false)
}

// A busy group refuses a console, with a notice; a stuck key is released
// after the transmit limit.
func TestConsoleBusyAndLimit(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	toc := b.add(t, cfg("toc", Channel{Group: 2, Volume: 100}, Channel{}))
	c, _ := toc.AddConsole("desk")
	toc.ConsoleMonitor(c, []int{1})
	a.SetPTT(0, true)
	for i := 0; i < 5; i++ {
		b.step(map[*Engine]int16{a: 1000})
	}
	if ok, _ := toc.ConsolePTT(c, 1, true); ok {
		t.Fatal("granted on a busy group")
	}
	if n := <-c.Notices; n != "busy" {
		t.Errorf("notice %q", n)
	}
	toc.ConsolePTT(c, 1, false)
	if ok, _ := toc.ConsolePTT(c, 2, true); !ok {
		t.Fatal("refused on a quiet group")
	}
	for i := 0; i < int(consoleTxLimit/(FrameMs*time.Millisecond))+5; i++ {
		b.step(nil)
	}
	if toc.Status().Consoles[0].Sending {
		t.Error("still keyed after the limit")
	}
	if n := <-c.Notices; n != "tx-limit" {
		t.Errorf("notice %q", n)
	}
}

// An all-call reaches every group at once, and takes over a group where
// someone else is talking: Trellis receivers switch to it, show it as an
// all-call, and play the override tone.
func TestConsoleAllCall(t *testing.T) {
	b := &bus{now: time.Unix(1000, 0)}
	a := b.add(t, cfg("alpha", Channel{Group: 1, Volume: 100}, Channel{}))
	c1 := cfg("one", Channel{Group: 1, Volume: 100}, Channel{})
	c1.Tones = true
	one := b.add(t, c1)
	two := b.add(t, cfg("two", Channel{Group: 2, Volume: 100}, Channel{}))
	toc := b.add(t, cfg("toc", Channel{Group: 3, Volume: 100}, Channel{}))
	con, _ := toc.AddConsole("cmd")
	a.SetPTT(0, true)
	var out map[*Engine][]int16
	for i := 0; i < 8; i++ {
		out = b.step(map[*Engine]int16{a: 2000})
	}
	if v, _ := lr(out[one]); v != 2000 {
		t.Fatalf("before: one hears %d", v)
	}
	if g := toc.ConsoleAllCall(con, true); len(g) != 3 {
		t.Fatalf("all-call keyed %v", g)
	}
	toned := false
	for i := 0; i < 12; i++ {
		toc.ConsoleAudio(con, consoleFrame(1000))
		out = b.step(map[*Engine]int16{a: 2000})
		if len(one.tones[0].samples) > 0 {
			toned = true
		}
	}
	if v, _ := lr(out[one]); v < 990 || v > 1010 {
		t.Errorf("one (on a busy group) hears %d, want the all-call's 1000", v)
	}
	if v, _ := lr(out[two]); v < 990 || v > 1010 {
		t.Errorf("two hears %d", v)
	}
	if g := one.Status().Groups[0]; g.Talker != "toc/cmd" || !g.AllCall {
		t.Errorf("one's group 1: %+v", g)
	}
	if !toned {
		t.Error("no override tone")
	}
	if st := toc.Status().Consoles[0]; !st.AllCall || !st.Sending {
		t.Errorf("console status %+v", st)
	}
	if ok, err := toc.ConsolePTT(con, 2, true); ok || err == nil {
		t.Error("a single-group key during an all-call")
	}
	toc.ConsoleAllCall(con, false)
	for i := 0; i < 5; i++ {
		b.step(nil)
	}
	if st := toc.Status(); st.Consoles[0].Sending || st.Groups[0].Sending {
		t.Errorf("after release: %+v", st.Consoles[0])
	}
}
