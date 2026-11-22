package meshd

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestUDPPayload(t *testing.T) {
	// IPv4 (IHL 5) from 10.73.1.150, UDP 40000 → 6969, payload "hi".
	p := []byte{0x45, 0, 0, 30, 0, 0, 0, 0, 1, 17, 0, 0, 10, 73, 1, 150, 239, 2, 3, 1,
		0x9c, 0x40, 0x1b, 0x39, 0, 10, 0, 0, 'h', 'i'}
	src, payload, ok := udpPayload(p)
	if !ok || src != "10.73.1.150" || string(payload) != "hi" {
		t.Fatalf("%v %q %q", ok, src, payload)
	}
	// Ethernet padding after the datagram is not payload.
	if _, payload, ok := udpPayload(append(append([]byte{}, p...), 0, 0, 0, 0)); !ok || string(payload) != "hi" {
		t.Fatalf("padded: %v %q", ok, payload)
	}
	// htons: in memory, network order.
	var b [2]byte
	binary.NativeEndian.PutUint16(b[:], htons(0x0003))
	if b != [2]byte{0, 3} {
		t.Fatalf("htons: %v", b)
	}
	// With IP options (IHL 6).
	q := append([]byte{0x46}, p[1:20]...)
	q = append(q, 1, 1, 1, 1)
	q = append(q, p[20:]...)
	q[3] = 34 // total length with the options
	if src, payload, ok := udpPayload(q); !ok || src != "10.73.1.150" || string(payload) != "hi" {
		t.Fatalf("options: %v %q %q", ok, src, payload)
	}
	for name, b := range map[string][]byte{
		"short":                 p[:10],
		"ipv6":                  append([]byte{0x60}, p[1:]...),
		"ihl 4":                 append([]byte{0x44}, p[1:]...),
		"no udp":                p[:24],
		"total over the frame":  append([]byte{0x45, 0, 0, 200}, p[4:]...),
		"udp length over total": append(append([]byte{}, p[:24]...), 0, 99, 0, 0, 'h', 'i'),
		"not udp":               append(append(append([]byte{}, p[:9]...), 6), p[10:]...),
		"another port":          append(append(append([]byte{}, p[:22]...), 0x1f, 0x90), p[24:]...),
	} {
		if _, _, ok := udpPayload(b); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The SA filter's jumps: every failed check lands on the final drop, the
// port match on accept, nothing past the end.
func TestSAFilterJumps(t *testing.T) {
	prog := saFilter([2]uint32{7, 9})
	last := len(prog) - 1
	if prog[last].Code != 0x06 || prog[last].K != 0 || prog[last-1].Code != 0x06 || prog[last-1].K == 0 {
		t.Fatal("program does not end accept, drop")
	}
	for i, ins := range prog {
		if ins.Code&0x07 != 0x05 { // BPF_JMP class
			continue
		}
		for _, off := range []uint8{ins.Jt, ins.Jf} {
			if to := i + 1 + int(off); to > last {
				t.Fatalf("instruction %d jumps past the end (%d)", i, to)
			}
		}
	}
	// The ifindex checks drop on a match; the protocol, UDP and port
	// checks drop on a mismatch; the fragment test drops when set.
	for i, want := range map[int][2]int{1: {last, 2}, 2: {last, 3}, 4: {5, last}, 6: {7, last}, 8: {last, 9}, 11: {last - 1, last}} {
		ins := prog[i]
		if got := [2]int{i + 1 + int(ins.Jt), i + 1 + int(ins.Jf)}; got != want {
			t.Errorf("instruction %d goes to %v, want %v", i, got, want)
		}
	}
	if prog[1].K != 7 || prog[2].K != 9 {
		t.Error("excluded ifindexes not in the program")
	}
}

const (
	testPLI    = `<event version="2.0" uid="ANDROID-1" type="a-f-G-U-C" how="m-g"><point lat="52.0" lon="5.0" hae="10" ce="5" le="9999999.0"/><detail><takv device="x"/><contact endpoint="*:-1:stcp" callsign="A-1"/></detail></event>`
	testMarker = `<event version="2.0" uid="m-1" type="a-h-G" how="h-g-i-g-o"><point lat="52.1" lon="5.1" hae="0" ce="10" le="10"/><detail><link uid="ANDROID-1" relation="p-p"/></detail></event>`
)

// A device's own position report is this node's position when it has no
// fix of its own; a marker the device shares, or a Trellis node's report,
// is not.
func TestDevicePosition(t *testing.T) {
	d := &Daemon{}
	now := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	d.noteDeviceSA("10.73.1.50", "02:00:00:00:00:50", []byte(testMarker), now)
	d.noteDeviceSA("10.73.1.50", "02:00:00:00:00:50", []byte(strings.Replace(testPLI, "ANDROID-1", "trellis-node-020000000003", 1)), now)
	if _, _, _, ok := d.positionFix(now); ok {
		t.Fatal("a marker or a node's report taken as the device's position")
	}
	d.noteDeviceSA("10.73.1.50", "02:00:00:00:00:50", []byte(testPLI), now)
	st, from, how, ok := d.positionFix(now.Add(time.Second))
	if !ok || st.Lat != 52.0 || !st.HasAlt || st.AltM != 10 || st.HAccM != 5 || from != "10.73.1.50" || how != "m-g" {
		t.Fatalf("own report: %+v from %q how %q ok %v", st, from, how, ok)
	}
	// Not heard for deviceFresh (it left, or went quiet): forgotten.
	if _, _, _, ok := d.positionFix(now.Add(deviceFresh + time.Second)); ok {
		t.Fatal("a device not heard still used")
	}
}

// The three rules that hold a device back, each with its reason in status.
func TestDeviceHeld(t *testing.T) {
	now := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	note := func(d *Daemon, at time.Time) string {
		if _, _, _, ok := d.positionFix(at); ok {
			t.Helper()
			t.Fatal("position used")
		}
		return d.phone.note
	}
	// No GPS fix of its own: ATAK reports a last or hand-set point with
	// another how.
	d := &Daemon{}
	d.noteDeviceSA("10.73.1.50", "02:00:00:00:00:50", []byte(strings.Replace(testPLI, `how="m-g"`, `how="h-e"`, 1)), now)
	if got := note(d, now); !strings.Contains(got, "no GPS fix") {
		t.Errorf("how h-e: %q", got)
	}
	// A source reporting two units is a gateway; one unit again once the
	// other has not been heard for deviceFresh.
	d = &Daemon{}
	d.noteDeviceSA("10.73.1.60", "02:00:00:00:00:60", []byte(testPLI), now)
	d.noteDeviceSA("10.73.1.60", "02:00:00:00:00:60", []byte(strings.Replace(testPLI, "ANDROID-1", "ANDROID-2", 1)), now.Add(time.Second))
	if got := note(d, now.Add(2*time.Second)); !strings.Contains(got, "gateway") {
		t.Errorf("two units: %q", got)
	}
	// Kept alive by one unit while four more come and go quiet: the
	// units remembered do not fill up.
	for i, uid := range []string{"ANDROID-3", "ANDROID-4", "ANDROID-5", "ANDROID-6"} {
		at := now.Add(time.Duration(i+1) * 90 * time.Second)
		d.noteDeviceSA("10.73.1.60", "02:00:00:00:00:60", []byte(strings.Replace(testPLI, "ANDROID-1", uid, 1)), at)
		d.noteDeviceSA("10.73.1.60", "02:00:00:00:00:60", []byte(testPLI), at.Add(time.Second))
	}
	if got := note(d, now.Add(6*time.Minute+2*time.Second)); !strings.Contains(got, "gateway") {
		t.Errorf("units coming and going: %q", got)
	}
	later := now.Add(6*time.Minute + deviceFresh + time.Minute)
	d.noteDeviceSA("10.73.1.60", "02:00:00:00:00:60", []byte(testPLI), later)
	if _, _, _, ok := d.positionFix(later); !ok {
		t.Error("one unit again: still held")
	}
	// A device this node fed its own fix reports that fix back for a while.
	d = &Daemon{}
	d.noteDeviceSA("10.73.1.50", "02:00:00:00:00:50", []byte(testPLI), now)
	d.phone.devs["10.73.1.50"].fedAt = now
	if got := note(d, now.Add(time.Minute)); !strings.Contains(got, "fed") {
		t.Errorf("fed lately: %q", got)
	}
	d.noteDeviceSA("10.73.1.50", "02:00:00:00:00:50", []byte(testPLI), now.Add(fedHold+time.Second))
	if _, _, _, ok := d.positionFix(now.Add(fedHold + time.Second)); !ok {
		t.Error("fed over fedHold ago: still held")
	}
	// A second device that can be used is, with the first held.
	d.phone.devs["10.73.1.50"].fedAt = now.Add(fedHold)
	d.noteDeviceSA("10.73.1.51", "02:00:00:00:00:51", []byte(strings.Replace(testPLI, "ANDROID-1", "ANDROID-9", 1)), now.Add(fedHold+2*time.Second))
	if _, from, _, ok := d.positionFix(now.Add(fedHold + 3*time.Second)); !ok || from != "10.73.1.51" {
		t.Errorf("second device: from %q ok %v", from, ok)
	}
}

// The devices remembered are bounded, and an address that goes to another
// device starts afresh.
func TestDevicesBounded(t *testing.T) {
	d := &Daemon{}
	now := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	for i := 0; i < maxDevices+10; i++ {
		ip := "10.73.1." + string(rune('0'+i/100)) + string(rune('0'+i/10%10)) + string(rune('0'+i%10))
		d.noteDeviceSA(ip, "02:00:00:00:00:01", []byte(testPLI), now)
	}
	if n := len(d.phone.devs); n != maxDevices {
		t.Fatalf("%d devices kept, want %d", n, maxDevices)
	}
	d = &Daemon{}
	d.noteDeviceSA("10.73.1.50", "02:00:00:00:00:50", []byte(testPLI), now)
	d.phone.devs["10.73.1.50"].fedAt = now
	d.noteDeviceSA("10.73.1.50", "02:00:00:00:00:77", []byte(strings.Replace(testPLI, "ANDROID-1", "ANDROID-7", 1)), now.Add(time.Second))
	if v := d.phone.devs["10.73.1.50"]; v.mac != "02:00:00:00:00:77" || !v.fedAt.IsZero() || len(v.units) != 1 {
		t.Errorf("address reused: %+v", v)
	}
}
