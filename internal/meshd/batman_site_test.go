package meshd

import "testing"

func TestSiteHopPenalty(t *testing.T) {
	for _, mbit := range []uint32{1, 2, 5, 10, 50, 100, 1000, 100000} {
		tp := mbit * 10
		hp := siteHopPenalty(tp, batadvDefaultHopPenalty)
		rt, one := siteRoundTrip(tp, hp, batadvDefaultHopPenalty), siteCrossing(tp, hp)
		t.Logf("%d Mbit/s: hop penalty %d, one crossing %d.%d Mbit/s, round trip %d.%d", mbit, hp, one/10, one%10, rt/10, rt%10)
		if rt > siteRoundTripMax && hp < 254 {
			t.Errorf("%d Mbit/s: round trip %d > %d", mbit, rt, siteRoundTripMax)
		}
		if one == 0 {
			t.Errorf("%d Mbit/s: one crossing is worth 0, and batman-adv would drop it", mbit)
		}
		if hp > 0 && siteRoundTrip(tp, hp-1, batadvDefaultHopPenalty) <= siteRoundTripMax {
			t.Errorf("%d Mbit/s: %d is not the lowest penalty that holds", mbit, hp)
		}
	}
	// The default: a round trip loses to HaLow at 1 MHz; one crossing is
	// still worth more than a round trip.
	hp := siteHopPenalty(100, batadvDefaultHopPenalty)
	if got := siteRoundTrip(100, 0, batadvDefaultHopPenalty); got != 88 {
		t.Errorf("without a penalty the round trip is %d, want 88 (8.8 Mbit/s, as the lab's batman-adv works it out)", got)
	}
	if hp != 174 {
		t.Errorf("the default's penalty is %d, want 174 (lab S5d)", hp)
	}
	if siteRoundTrip(100, hp, batadvDefaultHopPenalty) >= siteCrossing(100, hp) {
		t.Error("a round trip is worth as much as one crossing")
	}
	// A larger mesh-wide penalty needs less of the tunnel's own.
	if siteHopPenalty(100, 90) > hp {
		t.Error("a higher global penalty raised the tunnel's")
	}
}
