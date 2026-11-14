package gossip

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func rec(mac string, boot, seq uint64) *Record {
	return &Record{V: 1, MAC: mac, Orig: []string{"a0:b0:c0:00:01:f1"}, ID: 1, Callsign: "node-1", Addr: "10.73.1.1", Boot: boot, Seq: seq}
}

func TestSealOpen(t *testing.T) {
	k, err := Key("passphrase-1234", "trellis-alpha")
	if err != nil {
		t.Fatal(err)
	}
	r := rec("ea:67:61:6f:bc:8b", 7, 1)
	r.GW = &GW{Healthy: true, DownKbps: 50000}
	pkt, err := Seal(k, r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(k, pkt)
	if err != nil || got.Callsign != "node-1" || got.GW == nil || !got.GW.Healthy {
		t.Fatalf("open: %+v %v", got, err)
	}
	// Tampering and a wrong key are refused.
	bad := append([]byte{}, pkt...)
	bad[10] ^= 1
	if _, err := Open(k, bad); err != ErrAuth {
		t.Errorf("tampered: %v", err)
	}
	k2, _ := Key("other-passphrase", "trellis-alpha")
	if _, err := Open(k2, pkt); err != ErrAuth {
		t.Errorf("wrong key: %v", err)
	}
	k3, _ := Key("passphrase-1234", "other-mesh")
	if _, err := Open(k3, pkt); err != ErrAuth {
		t.Errorf("other mesh id: %v", err)
	}
	if _, err := Open(k, []byte("hello")); err != ErrNotGossip {
		t.Errorf("junk: %v", err)
	}
}

func TestTable(t *testing.T) {
	now := time.Unix(1000, 0)
	tb := NewTable("66:26:2e:a5:2a:c9")
	tb.now = func() time.Time { return now }
	if tb.Update(rec("66:26:2E:A5:2A:C9", 1, 1), "x") != Ignored {
		t.Error("own record accepted")
	}
	if tb.Update(rec("ea:67:61:6f:bc:8b", 5, 3), "10.73.1.1") != New {
		t.Error("first record not New")
	}
	if tb.Update(rec("ea:67:61:6f:bc:8b", 5, 3), "10.73.1.1") != Ignored {
		t.Error("replay accepted")
	}
	if tb.Update(rec("ea:67:61:6f:bc:8b", 5, 2), "10.73.1.1") != Ignored {
		t.Error("older sequence accepted")
	}
	if tb.Update(rec("ea:67:61:6f:bc:8b", 5, 4), "10.73.1.1") != Refresh {
		t.Error("newer sequence not a refresh")
	}
	r := rec("ea:67:61:6f:bc:8b", 5, 5)
	r.GW = &GW{Healthy: true}
	if tb.Update(r, "10.73.1.1") != Changed {
		t.Error("state change not Changed")
	}
	// A restart (new boot id) is accepted even with a lower sequence.
	if tb.Update(rec("ea:67:61:6f:bc:8b", 9, 1), "10.73.1.1") != Changed {
		t.Error("new boot not accepted")
	}
	if p, ok := tb.ByOriginator("a0:b0:c0:00:01:f1"); !ok || p.Record.Callsign != "node-1" {
		t.Error("ByOriginator")
	}
	now = now.Add(Expiry + time.Second)
	if len(tb.Peers()) != 0 || tb.Expire() != 1 {
		t.Error("expiry")
	}
	if tb.Update(rec("ea:67:61:6f:bc:8b", 9, 2), "10.73.1.1") != New {
		t.Error("record after expiry should be New")
	}
}

func TestSameIgnoresLiveFigures(t *testing.T) {
	a := rec("ea:67:61:6f:bc:8b", 1, 1)
	a.GW = &GW{Healthy: true, RTTms: 12, LossPct: 0}
	b := rec("ea:67:61:6f:bc:8b", 1, 2)
	b.GW = &GW{Healthy: true, RTTms: 30, LossPct: 3}
	if !Same(a, b) {
		t.Error("RTT/loss changes should not count as a state change")
	}
	b.GW.Healthy = false
	if Same(a, b) {
		t.Error("health change must count")
	}
	if a.GW.RTTms != 12 {
		t.Error("Same modified its argument")
	}
}

func TestTableClone(t *testing.T) {
	self := "66:26:2e:a5:2a:c9"
	tb := NewTable(self)
	// Before the table knows its own boot value, nothing is a clone.
	if got := tb.Update(&Record{MAC: self, Boot: 9, Seq: 1}, "10.73.2.1"); got != Ignored {
		t.Fatalf("own MAC without a boot value: %d", got)
	}
	tb.SetBoot(7)
	if got := tb.Update(&Record{MAC: self, Boot: 7, Seq: 2}, "10.73.2.1"); got != Ignored {
		t.Fatalf("looped-back own record: %d", got)
	}
	if got := tb.Update(&Record{MAC: "66:26:2E:A5:2A:C9", Boot: 9, Seq: 1, Callsign: "copy"}, "10.73.2.1"); got != Clone {
		t.Fatalf("record from a clone: %d, want Clone", got)
	}
	if got := tb.Update(&Record{MAC: self, Boot: 9, Seq: 2}, "10.73.2.1"); got != Ignored {
		t.Fatalf("second record from the same clone: %d", got)
	}
	c, ok := tb.Clone()
	if !ok || c.Callsign != "" || c.Seq != 2 {
		t.Fatalf("Clone() = %+v, %v", c, ok)
	}
	if len(tb.Peers()) != 0 {
		t.Fatal("a clone must not enter the peer table")
	}
	tb.SetSelf("02:00:00:00:00:99")
	if _, ok := tb.Clone(); ok {
		t.Fatal("clone kept after the identity changed")
	}
	if got := tb.Update(&Record{MAC: self, Boot: 9, Seq: 3}, "10.73.2.1"); got != New {
		t.Fatalf("the clone's MAC is an ordinary peer after SetSelf: %d", got)
	}
}

// A correctly signed record from a member is still refused when a field
// could inject into this node's configuration or terminal output.
func TestOpenRejectsInvalidFields(t *testing.T) {
	k, err := Key("passphrase-1234", "trellis-alpha")
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Record){
		"newline in callsign": func(r *Record) { r.Callsign = "node-1\n10.0.0.66 example.com" },
		"space in callsign":   func(r *Record) { r.Callsign = "node 1" },
		"upper-case callsign": func(r *Record) { r.Callsign = "Node-1" },
		"address not an IP":   func(r *Record) { r.Addr = "10.73.1.1\nserver=1.1.1.1" },
		"IPv6 address":        func(r *Record) { r.Addr = "fe80::1" },
		"node id over 254":    func(r *Record) { r.ID = 300 },
		"negative node id":    func(r *Record) { r.ID = -1 },
		"bad mac":             func(r *Record) { r.MAC = "not-a-mac" },
		"bad originator":      func(r *Record) { r.Orig = []string{"zz"} },
		"escape in version":   func(r *Record) { r.Version = "1.0\x1b[2J" },
		"overlong id source":  func(r *Record) { r.IDSource = "derived-and-then-some-more" },
	} {
		r := rec("ea:67:61:6f:bc:8b", 7, 1)
		mutate(r)
		pkt, err := Seal(k, r)
		if err != nil {
			t.Fatalf("%s: seal: %v", name, err)
		}
		if _, err := Open(k, pkt); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
	// Fields a real node sends pass, including an empty address (not yet
	// assigned) and a dev build's version string.
	r := rec("ea:67:61:6f:bc:8b", 7, 1)
	r.Addr, r.Version, r.IDSource = "", "4a48da6-dirty+1109.181143", "derived"
	pkt, _ := Seal(k, r)
	if _, err := Open(k, pkt); err != nil {
		t.Errorf("valid record refused: %v", err)
	}
}

func TestLinkMessages(t *testing.T) {
	key, _ := Key("passphrase", "mesh")
	other, _ := Key("another", "mesh")
	q := &Query{V: 1, MAC: "02:00:00:00:00:01", Nonce: 42}
	pkt, err := SealMessage(key, QueryMagic, q)
	if err != nil {
		t.Fatal(err)
	}
	if Kind(pkt) != QueryMagic {
		t.Errorf("kind %q", Kind(pkt))
	}
	if got, err := OpenQuery(key, pkt); err != nil || *got != *q {
		t.Errorf("query round trip: %+v %v", got, err)
	}
	if _, err := OpenQuery(other, pkt); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong key: %v", err)
	}
	// A record is not a query, and the other way round: old nodes ignore
	// link packets.
	if _, err := Open(key, pkt); !errors.Is(err, ErrNotGossip) {
		t.Errorf("a query opened as a record: %v", err)
	}

	full := &Links{V: 1, MAC: "02:00:00:00:00:02", Nonce: 42, Clients: 3, Selected: 7}
	for i := 0; i < MaxLinks; i++ {
		full.Links = append(full.Links, Link{Local: "a0:b0:c0:00:02:26", Remote: fmt.Sprintf("a0:b0:c0:00:89:%02x", i), Kbps: 866700, Signal: -100})
	}
	pkt, err = SealMessage(key, LinksMagic, full)
	if err != nil {
		t.Fatalf("a full reply does not fit one packet: %v", err)
	}
	t.Logf("a reply with %d links is %d bytes", MaxLinks, len(pkt))
	got, err := OpenLinks(key, pkt)
	if err != nil || len(got.Links) != MaxLinks || got.Links[3] != full.Links[3] || got.Clients != 3 || got.Selected != 7 {
		t.Errorf("links round trip: %+v %v", got, err)
	}
	for name, bad := range map[string]*Links{
		"mac":      {V: 1, MAC: "nope"},
		"link mac": {V: 1, MAC: "02:00:00:00:00:02", Links: []Link{{Local: "x", Remote: "02:00:00:00:00:03"}}},
		"signal":   {V: 1, MAC: "02:00:00:00:00:02", Links: []Link{{Local: "02:00:00:00:00:02", Remote: "02:00:00:00:00:03", Signal: 20}}},
		"selected": {V: 1, MAC: "02:00:00:00:00:02", Selected: 999},
	} {
		pkt, _ := SealMessage(key, LinksMagic, bad)
		if _, err := OpenLinks(key, pkt); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

// A record lists the bands of the node's mesh-capable radios, one entry per
// radio; one from an older node, without the list, is still accepted.
func TestRecordRadios(t *testing.T) {
	k, _ := Key("passphrase-1234", "trellis-alpha")
	r := rec("ea:67:61:6f:bc:8b", 7, 1)
	r.Radios = []string{RadioEntry([]string{"2g", "5g"}), RadioEntry([]string{"s1g"})}
	pkt, err := Seal(k, r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(k, pkt)
	if err != nil || len(got.Radios) != 2 {
		t.Fatalf("open: %+v %v", got, err)
	}
	if b := RadioBands(got.Radios[0]); len(b) != 2 || b[0] != "2g" || b[1] != "5g" {
		t.Errorf("first radio's bands = %v", b)
	}
	// The list costs a few bytes per radio, not a new packet.
	plain, _ := Seal(k, rec("ea:67:61:6f:bc:8b", 7, 1))
	if extra := len(pkt) - len(plain); extra > 30 {
		t.Errorf("two radios add %d bytes to a record", extra)
	}
	// A change in it is a change of state (it is sent at once).
	if Same(r, rec("ea:67:61:6f:bc:8b", 7, 1)) {
		t.Error("records that differ in radios compare as the same")
	}
	old, _ := Seal(k, rec("ea:67:61:6f:bc:8b", 7, 2))
	if got, err := Open(k, old); err != nil || got.Radios != nil {
		t.Errorf("record without radios: %+v %v", got, err)
	}
	for _, bad := range [][]string{{""}, {"2g\n5g"}, {"a", "b", "c", "d", "e", "f", "g", "h", "i"}} {
		r := rec("ea:67:61:6f:bc:8b", 7, 3)
		r.Radios = bad
		pkt, _ := Seal(k, r)
		if _, err := Open(k, pkt); !errors.Is(err, ErrInvalid) {
			t.Errorf("radios %q accepted: %v", bad, err)
		}
	}
}
