package meshd

import (
	"testing"

	"meridian/trellis/internal/nl80211"
)

func TestOneSided(t *testing.T) {
	st := func(packets, failed uint32) nl80211.Station {
		return nl80211.Station{TXPackets: packets, TXFailed: failed}
	}
	for name, c := range map[string]struct {
		prev peerTX
		now  nl80211.Station
		bad  bool
	}{
		// Most frames to a peer out of range fail.
		"most fail":         {peerTX{packets: 100, failed: 10}, st(341, 215), true},
		"half fail":         {peerTX{packets: 0, failed: 0}, st(40, 20), true},
		"a few fail":        {peerTX{packets: 1000, failed: 3}, st(3000, 40), false},
		"too few to judge":  {peerTX{packets: 10, failed: 0}, st(25, 15), false},
		"counters reset":    {peerTX{packets: 5000, failed: 100}, st(30, 30), false},
		"idle":              {peerTX{packets: 50, failed: 2}, st(50, 2), false},
		"nothing but fails": {peerTX{packets: 50, failed: 0}, st(50, 25), false}, // packets did not grow: too few
	} {
		if bad, _, _ := oneSided(c.prev, c.now); bad != c.bad {
			t.Errorf("%s: one-sided %v, want %v", name, bad, c.bad)
		}
	}
	// By signal: a peer heard at -93 dBm on 80 MHz carries nothing, one at
	// -77 is fine, and 2.4 GHz peers at -33 to -67 dBm carry traffic.
	sig := func(dbm int8) nl80211.Station { return nl80211.Station{SignalAvgDBm: dbm, HasSignal: true} }
	for _, c := range []struct {
		dbm   int8
		width int
		weak  bool
	}{{-93, 80, true}, {-77, 80, false}, {-67, 20, false}, {-91, 20, false}, {-93, 20, true}, {-84, 160, true}, {-82, 160, false}} {
		if got := tooWeak(sig(c.dbm), c.width); got != c.weak {
			t.Errorf("%d dBm at %d MHz: too weak %v, want %v", c.dbm, c.width, got, c.weak)
		}
	}
	if tooWeak(nl80211.Station{}, 80) {
		t.Error("no signal reported taken for a weak one")
	}
}
