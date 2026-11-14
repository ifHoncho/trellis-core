package meshd

import (
	"slices"
	"strings"
	"testing"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
)

func TestChannelBlock(t *testing.T) {
	for _, c := range []struct {
		band      string
		ch, width int
		want      []int
	}{
		{"2g", 6, 20, []int{6}},
		{"5g", 149, 20, []int{149}},
		{"5g", 44, 40, []int{44, 48}},
		{"5g", 157, 80, []int{149, 153, 157, 161}},
		{"5g", 116, 80, []int{116, 120, 124, 128}},
		{"5g", 104, 160, []int{100, 104, 108, 112, 116, 120, 124, 128}},
	} {
		if got := channelBlock(c.band, c.ch, c.width); !slices.Equal(got, c.want) {
			t.Errorf("%s ch %d at %d: %v, want %v", c.band, c.ch, c.width, got, c.want)
		}
	}
}

func ch24(busy map[int]int, nets map[int]int) []gossip.SurveyCh {
	var out []gossip.SurveyCh
	for c := 1; c <= 11; c++ {
		b, ok := busy[c]
		if !ok {
			b = 5
		}
		out = append(out, gossip.SurveyCh{Ch: c, Busy: b, Nets: nets[c], Strongest: map[bool]int{true: -60}[nets[c] > 0]})
	}
	return out
}

// The bench's case: 2.4 GHz channel 6 busy where one node is (31%) with two
// networks near it; channel 1 clear everywhere. A node that measures no
// busy time (rt2800usb) still counts the networks it heard.
func TestSuggestChannels(t *testing.T) {
	p := &config.Profile{Radio: config.RadioPlan{Band: "2g", Channel: 6, Width: 20},
		Links: []config.RadioPlan{{Band: "5g", Channel: 149, Width: 80}, {Band: "s1g", Channel: 26, Width: 2}}}
	five := func(busy int) []gossip.SurveyCh {
		var out []gossip.SurveyCh
		for _, c := range []int{36, 40, 44, 48, 52, 56, 60, 64, 149, 153, 157, 161} {
			b := 3
			if c >= 149 {
				b = busy
			}
			out = append(out, gossip.SurveyCh{Ch: c, Busy: b})
		}
		return out
	}
	noBusy := ch24(nil, map[int]int{6: 1, 7: 1})
	for i := range noBusy {
		noBusy[i].Busy = -1
	}
	reports := []surveyReport{
		{Callsign: "node-1", Links: []gossip.SurveyLink{
			{Link: 1, Band: "2g", Channels: ch24(map[int]int{5: 25, 6: 31, 7: 28}, map[int]int{6: 1, 8: 1})},
			{Link: 2, Band: "5g", Channels: five(4)}}},
		{Callsign: "node-2", Links: []gossip.SurveyLink{
			{Link: 1, Band: "2g", Channels: ch24(map[int]int{6: 12}, nil)},
			{Link: 2, Band: "5g", Channels: five(2)}}},
		{Callsign: "alpha-5", Links: []gossip.SurveyLink{{Link: 1, Band: "2g", Channels: noBusy}}},
	}
	sug := suggestChannels(p, reports)
	if len(sug) != 3 {
		t.Fatalf("%+v", sug)
	}
	two := sug[0]
	if two.Current == nil || two.Current.WorstBusy != 31 || two.Current.WorstNode != "node-1" || two.Current.Nets != 2 {
		t.Errorf("2.4 GHz current: %+v", two.Current)
	}
	if two.Best == nil || two.Best.Ch != 1 || !strings.Contains(two.Note, "channel 1 is clearer") {
		t.Errorf("2.4 GHz suggestion: %+v %q", two.Best, two.Note)
	}
	// 5 GHz at 80 MHz: 149's block is clear, 36's too; 52's is radar.
	fiveSug := sug[1]
	if fiveSug.Best != nil {
		t.Errorf("5 GHz: a move suggested from a clear channel: %+v", fiveSug)
	}
	for _, r := range fiveSug.Ranked {
		if r.Ch == 52 && !r.DFS {
			t.Errorf("52's block not marked as radar: %+v", r)
		}
		if r.Ch == 40 || r.Ch == 153 {
			t.Errorf("a channel inside a block ranked on its own: %+v", r)
		}
	}
	if sug[2].Note == "" || sug[2].Ranked != nil {
		t.Errorf("HaLow: %+v", sug[2])
	}
}

// A radio that reads every channel as fully busy during a scan (the MT7610U
// on 5 GHz) gives no busy times; its own channel's load stays.
func TestSaturated(t *testing.T) {
	var chs []gossip.SurveyCh
	for _, c := range []int{52, 56, 60, 64, 100, 104, 149, 153} {
		b := 100
		if c == 149 {
			b = 1 // its own channel, measured the usual way
		}
		chs = append(chs, gossip.SurveyCh{Ch: c, Busy: b})
	}
	if !saturated(chs, 1) {
		t.Error("an MT7610U-like 5 GHz survey not taken as saturated")
	}
	sane := []gossip.SurveyCh{{Ch: 1, Busy: 2}, {Ch: 6, Busy: 12}, {Ch: 11, Busy: 95}, {Ch: 3, Busy: 30}, {Ch: 9, Busy: 28}}
	if saturated(sane, 12) {
		t.Error("a plausible survey taken as saturated")
	}
}

// What is not suggested: a channel between 1, 6 and 11; one nobody
// measured; a small gain from one short listen.
func TestSuggestChannelsRestraint(t *testing.T) {
	p := &config.Profile{Radio: config.RadioPlan{Band: "2g", Channel: 6, Width: 20}}
	at := func(busy map[int]int) []surveyReport {
		chs := ch24(busy, nil)
		return []surveyReport{{Callsign: "node-1", Links: []gossip.SurveyLink{{Link: 1, Band: "2g", Channels: chs}}}}
	}
	// 8 is clearest, but sits between 6 and 11.
	s := suggestChannels(p, at(map[int]int{6: 40, 8: 1, 1: 30, 11: 30}))[0]
	for _, r := range s.Ranked {
		if r.Ch != 1 && r.Ch != 6 && r.Ch != 11 {
			t.Errorf("channel %d ranked on 2.4 GHz", r.Ch)
		}
	}
	// 1 and 11 not measured (only networks heard): not suggested.
	unknown := at(map[int]int{6: 40, 1: -1, 11: -1})
	if s := suggestChannels(p, unknown)[0]; s.Best != nil {
		t.Errorf("an unmeasured channel suggested: %+v", s.Best)
	}
	// 14% against 8%: within what one listen tells apart.
	if s := suggestChannels(p, at(map[int]int{6: 14, 1: 8, 11: 9}))[0]; s.Best != nil || !strings.Contains(s.Note, "not busy") {
		t.Errorf("a small gain suggested: %+v %q", s.Best, s.Note)
	}
}
