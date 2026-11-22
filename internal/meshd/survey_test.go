package meshd

import (
	"testing"

	"meridian/trellis/internal/nl80211"
)

// TestVisitBusy: busy during a scan from the two ways drivers count.
func TestVisitBusy(t *testing.T) {
	si := func(f uint32, active, busy uint64, inUse bool) nl80211.SurveyInfo {
		return nl80211.SurveyInfo{FreqMHz: f, ActiveMS: active, BusyMS: busy, InUse: inUse}
	}
	m := func(ins ...nl80211.SurveyInfo) map[uint32]nl80211.SurveyInfo {
		out := map[uint32]nl80211.SurveyInfo{}
		for _, in := range ins {
			out[in.FreqMHz] = in
		}
		return out
	}

	// An mt76x0u radio, a second survey: each off-channel holds
	// the last visit; the in-use channel a running total.
	before := m(si(2412, 113, 17, false), si(2437, 114, 29, false), si(2457, 113, 76, false), si(2462, 37435451, 12349580, true))
	after := m(si(2412, 110, 10, false), si(2437, 113, 30, false), si(2457, 112, 32, false), si(2462, 37445781, 12353589, true))
	if !countsPerVisit(before, after) {
		t.Fatal("mt76 counters not taken as per visit")
	}
	for f, want := range map[uint32]int{2412: 9, 2437: 26, 2457: 28} {
		if got := visitBusy(before[f], after[f], true); got != want {
			t.Errorf("per visit, %d MHz: busy %d, want %d", f, got, want)
		}
	}
	// The same figures as before the scan: a quiet channel's visit, or the
	// last one; either way that visit's share, not unknown.
	if got := visitBusy(si(2412, 110, 0, false), si(2412, 110, 0, false), true); got != 0 {
		t.Errorf("per visit, figures unchanged: busy %d, want 0", got)
	}

	// The first survey after boot: before is empty; either way of
	// counting gives the visit, and counters of one visit or less are
	// taken as per visit.
	if !countsPerVisit(nil, after) {
		t.Error("first survey: not taken as per visit")
	}
	if got := visitBusy(nl80211.SurveyInfo{}, after[2457], true); got != 28 {
		t.Errorf("first survey, per visit: busy %d, want 28", got)
	}

	// mt76, no counter down this time: one visit 100/10, the next
	// 120/100. Taken as per visit from the size alone: 83 %, not
	// (100-10)/(120-100).
	grewB, grewA := m(si(5180, 100, 10, false)), m(si(5180, 120, 100, false))
	if !countsPerVisit(grewB, grewA) {
		t.Fatal("per-visit counters that grew not taken as per visit")
	}
	if got := visitBusy(grewB[5180], grewA[5180], true); got != 83 {
		t.Errorf("per visit, counters grew: busy %d, want 83", got)
	}
	if got := visitBusy(nl80211.SurveyInfo{}, after[2457], false); got != 28 {
		t.Errorf("first survey: busy %d, want 28", got)
	}

	// A running total (ath9k): the visit is after less before.
	before = m(si(2412, 220, 40, false), si(2437, 330, 90, false))
	after = m(si(2412, 330, 95, false), si(2437, 330, 90, false))
	if countsPerVisit(before, after) {
		t.Error("running totals taken as per visit")
	}
	if got := visitBusy(before[2412], after[2412], false); got != 50 {
		t.Errorf("running total: busy %d, want 50", got)
	}
	if got := visitBusy(before[2437], after[2437], false); got != -1 {
		t.Errorf("running total, not visited: busy %d, want -1", got)
	}
	if got := visitBusy(si(2412, 220, 40, false), si(2412, 230, 45, false), false); got != -1 {
		t.Errorf("running total, a 10 ms visit: busy %d, want -1", got)
	}
}
