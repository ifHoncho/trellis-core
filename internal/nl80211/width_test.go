package nl80211

import "testing"

func TestChannelWidth(t *testing.T) {
	ht := func(info byte) []byte {
		return []byte{61, 22, 6, info, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	}
	vht := func(w, s0, s1 byte) []byte { return []byte{192, 5, w, s0, s1, 0, 0} }
	cat := func(parts ...[]byte) []byte {
		var b []byte
		for _, p := range parts {
			b = append(b, p...)
		}
		return b
	}
	// HE Operation with 6 GHz information: params bit 17, no VHT info,
	// colour, MCS (2), then primary, control, ccfs0, ccfs1, min rate.
	he6 := func(ctrl, c0, c1 byte) []byte {
		return []byte{255, 12, 36, 0x00, 0x00, 0x02, 0x01, 0xff, 0xff, 37, ctrl, c0, c1, 1}
	}
	meshID := []byte{114, 3, 'a', 'b', 'c'}
	for _, c := range []struct {
		name string
		ies  []byte
		want int
	}{
		{"none", meshID, 0},
		{"HT 20", cat(meshID, ht(0)), 20},
		{"HT 40 above", cat(ht(1 | 4)), 40},
		{"HT 40 below", cat(ht(3 | 4)), 40},
		{"HT secondary without the width bit", cat(ht(1)), 20},
		{"VHT 80", cat(ht(1|4), vht(1, 155, 0)), 80},
		{"VHT 160", cat(ht(1|4), vht(1, 42, 50)), 160},
		{"VHT 160 deprecated", cat(vht(2, 50, 0)), 160},
		{"VHT 20/40 keeps HT", cat(ht(1|4), vht(0, 0, 0)), 40},
		{"HE 6 GHz 80", cat(he6(2, 39, 0)), 80},
		{"HE 6 GHz 160", cat(he6(3, 39, 47)), 160},
		{"truncated", []byte{61, 22, 6}, 0},
	} {
		if got := ChannelWidth(c.ies); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
