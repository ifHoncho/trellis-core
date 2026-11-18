package rtp

import "testing"

func FuzzParse(f *testing.F) {
	f.Add((&Packet{PayloadType: 111, Seq: 1, Payload: []byte{1, 2, 3}}).Append(nil))
	f.Add([]byte{0xb2, 111, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 9, 0, 0, 0, 10, 0xbe, 0xde, 0, 1, 1, 2, 3, 4, 'h', 'i', 0, 0, 3})
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Parse(b)
		if err == nil && len(p.Payload) > len(b)-HeaderLen {
			t.Fatalf("payload of %d from %d bytes", len(p.Payload), len(b))
		}
	})
}

func FuzzParseCompound(f *testing.F) {
	f.Add(AppendCompound(nil, SenderReport{SSRC: 1}, "node-1", true))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseCompound(b)
		if err != nil {
			return
		}
		for _, n := range r.CNAME {
			if len(n) > maxCNAME {
				t.Fatalf("CNAME of %d bytes", len(n))
			}
		}
	})
}
