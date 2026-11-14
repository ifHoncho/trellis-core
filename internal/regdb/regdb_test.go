package regdb

import (
	"testing"

	"meridian/trellis/internal/nl80211"
)

// The wireless-regdb release kept for old kernels (packaging/regdb).
const testDB = "../../packaging/regdb/regulatory.db"

func find(rd *nl80211.RegDomain, startMHz uint32) (nl80211.RegRule, bool) {
	for _, r := range rd.Rules {
		if r.StartKHz == startMHz*1000 {
			return r, true
		}
	}
	return nl80211.RegRule{}, false
}

func TestCountries(t *testing.T) {
	db, err := Load(testDB)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(db.Countries()); n < 150 {
		t.Errorf("%d countries, want wireless-regdb's 150+", n)
	}
	for _, c := range []struct {
		alpha2    string
		dfsRegion uint8
		startMHz  uint32
		flags     uint32
		eirp      uint32
	}{
		// US: 5250–5330 needs radar detection (FCC), 2.4 GHz at 30 dBm.
		{"US", 1, 5250, nl80211.NL80211_RRF_DFS | nl80211.NL80211_RRF_AUTO_BW, 2400},
		{"US", 1, 2400, 0, 3000},
		// Germany (ETSI) and Japan.
		{"DE", 2, 5250, nl80211.NL80211_RRF_DFS | nl80211.NL80211_RRF_AUTO_BW | nl80211.NL80211_RRF_NO_OUTDOOR, 2000},
		{"JP", 3, 2474, nl80211.NL80211_RRF_NO_OFDM, 2000},
	} {
		rd, ok := db.Country(c.alpha2)
		if !ok {
			t.Errorf("%s missing", c.alpha2)
			continue
		}
		r, ok := find(rd, c.startMHz)
		if rd.DFSRegion != c.dfsRegion || !ok || r.Flags != c.flags || r.MaxEIRPMBm != c.eirp {
			t.Errorf("%s: dfs region %d, rule at %d MHz %+v (found %v); want region %d, flags %#x, %d mBm", c.alpha2, rd.DFSRegion, c.startMHz, r, ok, c.dfsRegion, c.flags, c.eirp)
		}
	}
	if _, ok := db.Country("us"); !ok {
		t.Error("lower-case alpha2 not found")
	}
	if _, ok := db.Country("00"); !ok {
		t.Error("no world domain 00")
	}
	if _, ok := db.Country("XQ"); ok {
		t.Error("found a country that does not exist")
	}
}

func TestParseRejects(t *testing.T) {
	good, err := Load(testDB)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"empty":       nil,
		"bad magic":   append([]byte("XXXX"), good.data[4:]...),
		"bad version": append(append([]byte{}, good.data[:4]...), append([]byte{0, 0, 0, 19}, good.data[8:]...)...),
		"truncated":   good.data[:len(good.data)/2],
	} {
		if _, err := Parse(data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPermissive(t *testing.T) {
	rd := Permissive("us")
	if rd.Alpha2 != "US" || rd.DFSRegion != 0 {
		t.Errorf("alpha2 %q, dfs region %d", rd.Alpha2, rd.DFSRegion)
	}
	covers := func(mhz, bw uint32) bool {
		for _, r := range rd.Rules {
			if r.StartKHz <= (mhz-bw/2)*1000 && (mhz+bw/2)*1000 <= r.EndKHz && r.MaxBandwidthKHz >= bw*1000 {
				return true
			}
		}
		return false
	}
	// The mesh channels the US rules restrict or forbid.
	for _, c := range []struct{ centreMHz, widthMHz uint32 }{
		{5815, 160}, // channels 149–177
		{5250, 160}, // 36–64 without a radar check
		{5875, 20},  // 175 (UNII-4)
		{6025, 160}, // 6 GHz
		{2484, 20},  // 14
		{4920, 20},  // 4.9 GHz
		{915, 8},    // HaLow
	} {
		if !covers(c.centreMHz, c.widthMHz) {
			t.Errorf("%d MHz wide at %d MHz not covered", c.widthMHz, c.centreMHz)
		}
	}
	for _, r := range rd.Rules {
		if r.Flags != 0 || r.MaxEIRPMBm < 3600 {
			t.Errorf("rule %+v restricts", r)
		}
		if r.EndKHz <= r.StartKHz || r.MaxBandwidthKHz > r.EndKHz-r.StartKHz {
			t.Errorf("rule %+v fails the kernel's is_valid_reg_rule", r)
		}
	}
}

func TestIsPermissive(t *testing.T) {
	if !IsPermissive(Permissive("DE")) {
		t.Error("the permissive rules are not recognised")
	}
	db, err := Load(testDB)
	if err != nil {
		t.Fatal(err)
	}
	us, _ := db.Country("US")
	if IsPermissive(us) || IsPermissive(nil) {
		t.Error("US country rules taken for permissive")
	}
	p := Permissive("US")
	p.Rules[2].Flags = nl80211.NL80211_RRF_DFS
	if IsPermissive(p) {
		t.Error("permissive rules with a radar flag taken for permissive")
	}
}
