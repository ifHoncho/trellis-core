package rtp

import (
	"bytes"
	"testing"
)

func TestPacketRoundTrip(t *testing.T) {
	p := Packet{Marker: true, PayloadType: 111, Seq: 65535, Timestamp: 0xdeadbeef, SSRC: 0x01020304, Payload: []byte("opus")}
	b := p.Append(nil)
	got, err := Parse(b)
	if err != nil || got.Marker != p.Marker || got.PayloadType != 111 || got.Seq != p.Seq || got.Timestamp != p.Timestamp || got.SSRC != p.SSRC || !bytes.Equal(got.Payload, p.Payload) {
		t.Fatalf("got %+v %v", got, err)
	}
}

// A packet from another sender may carry CSRCs, an extension and padding.
func TestParseFullHeader(t *testing.T) {
	b := []byte{
		0xb2, 111, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3, // V2, P, X, CC=2
		0, 0, 0, 9, 0, 0, 0, 10, // CSRCs
		0xbe, 0xde, 0, 1, 1, 2, 3, 4, // extension, one word
		'h', 'i', 0, 0, 3, // payload, then 3 bytes of padding
	}
	p, err := Parse(b)
	if err != nil || string(p.Payload) != "hi" {
		t.Fatalf("got %q %v", p.Payload, err)
	}
	for _, bad := range [][]byte{
		nil, b[:11], {0x40, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, // short; version 1
		{0x8f, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},    // 15 CSRCs promised
		{0xa0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, // padding 0
		{0xa0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 9}, // padding past the header
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("% x parsed", bad)
		}
	}
}

func TestLess(t *testing.T) {
	for _, c := range []struct {
		a, b uint16
		want bool
	}{{1, 2, true}, {2, 1, false}, {65535, 0, true}, {0, 65535, false}, {5, 5, false}, {0, 0x7fff, true}} {
		if Less(c.a, c.b) != c.want {
			t.Errorf("Less(%d, %d) != %v", c.a, c.b, c.want)
		}
	}
}

func TestCompoundRoundTrip(t *testing.T) {
	sr := SenderReport{SSRC: 7, NTPTime: 1 << 40, RTPTime: 960, PacketCount: 50, OctetCount: 4000}
	for _, name := range []string{"", "a", "ab", "abc", "alpha-1", string(make([]byte, 100))} {
		for _, bye := range []bool{false, true} {
			b := AppendCompound(nil, sr, name, bye)
			if len(b)%4 != 0 {
				t.Fatalf("%q: %d bytes", name, len(b))
			}
			r, err := ParseCompound(b)
			if err != nil || r.SR == nil || *r.SR != sr {
				t.Fatalf("%q: %+v %v", name, r, err)
			}
			want := printable([]byte(name))
			if len(want) > maxCNAME {
				want = want[:maxCNAME]
			}
			if r.CNAME[7] != want || (len(r.Bye) == 1) != bye {
				t.Errorf("%q bye=%v: %+v", name, bye, r)
			}
		}
	}
	if _, err := ParseCompound([]byte{0x80, 200, 0, 9, 0, 0}); err == nil {
		t.Error("a packet longer than its buffer parsed")
	}
}

func TestNote(t *testing.T) {
	b := AppendCompoundNote(nil, SenderReport{SSRC: 5}, "toc/desk", "all-call", false)
	r, err := ParseCompound(b)
	if err != nil || r.CNAME[5] != "toc/desk" || r.Note[5] != "all-call" || len(b)%4 != 0 {
		t.Errorf("%+v %v", r, err)
	}
}
