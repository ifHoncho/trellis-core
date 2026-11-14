package meshd

import (
	"fmt"
	"strings"
	"testing"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/radio"
)

// The channel checks report; they refuse nothing. Radar is a warning (the
// mesh may start after its availability check); only what the kernel
// forbids outright is an error.
func TestCheckMeshChannel(t *testing.T) {
	rd := &radio.Radio{Phy: "phy1", Wiphy: &nl80211.Wiphy{Bands: map[nl80211.Band]*nl80211.BandInfo{
		nl80211.Band5GHz: {Frequencies: []nl80211.Frequency{
			{MHz: 5180},
			{MHz: 5260, Radar: true},
			{MHz: 5320, Disabled: true},
			{MHz: 5745, No160MHz: true},
			{MHz: 5825, No80MHz: true, No160MHz: true},
			{MHz: 5885, NoIR: true},
		}},
	}}}
	for _, c := range []struct {
		ch, width int
		want      string // level:code, "" for none
	}{
		{36, 160, "warning:channel-dfs"}, // 52 in its block needs radar detection
		{52, 80, "warning:channel-dfs"},
		{64, 20, "error:channel-disabled"},
		{177, 20, "error:channel-no-ir"},
		{100, 20, "error:channel-unsupported"},
		{149, 160, "warning:channel-width"},
		{149, 80, ""},
		{165, 80, "warning:channel-width"},
		{165, 40, ""},
	} {
		var got []string
		checkMeshChannel(rd, "mesh1", config.RadioPlan{Band: config.Band5G, Channel: c.ch, Width: c.width}, "US", func(level, code, format string, args ...any) {
			got = append(got, level+":"+code)
		})
		if c.want == "" && len(got) > 0 || c.want != "" && (len(got) != 1 || got[0] != c.want) {
			t.Errorf("channel %d at %d MHz: got %v, want %q", c.ch, c.width, got, c.want)
		}
	}
	// A primary without radar rules in a block with some (36 at 160 MHz:
	// 52–64) is a radar channel too.
	us := &radio.Radio{Phy: "phy1", Wiphy: &nl80211.Wiphy{Bands: map[nl80211.Band]*nl80211.BandInfo{nl80211.Band5GHz: {}}}}
	for f := 5180; f <= 5320; f += 20 {
		us.Wiphy.Bands[nl80211.Band5GHz].Frequencies = append(us.Wiphy.Bands[nl80211.Band5GHz].Frequencies, nl80211.Frequency{MHz: uint32(f), Radar: f >= 5260})
	}
	for width, want := range map[int]string{160: "52, 56, 60, 64", 80: "", 20: ""} {
		var got []string
		checkMeshChannel(us, "mesh1", config.RadioPlan{Band: config.Band5G, Channel: 36, Width: width}, "US", func(level, code, format string, args ...any) {
			got = append(got, level+":"+code+" "+fmt.Sprintf(format, args...))
		})
		switch {
		case want == "" && len(got) > 0:
			t.Errorf("channel 36 at %d MHz: got %v, want nothing", width, got)
		case want != "" && (len(got) != 1 || !strings.HasPrefix(got[0], "warning:channel-dfs") || !strings.Contains(got[0], want)):
			t.Errorf("channel 36 at %d MHz: got %v, want a channel-dfs warning naming %s", width, got, want)
		}
	}
	if got := blockFreqs(5745, 160); len(got) != 8 || got[0] != 5745 || got[7] != 5885 {
		t.Errorf("160 MHz block of channel 149 = %v, want 5745 … 5885", got)
	}

	var msg string
	checkMeshChannel(rd, "mesh1", config.RadioPlan{Band: config.Band5G, Channel: 52, Width: 20}, "US", func(level, code, format string, args ...any) {
		msg = format
	})
	if strings.Contains(msg, "cannot start") {
		t.Errorf("the DFS message still says the mesh cannot start there: %s", msg)
	}
}
