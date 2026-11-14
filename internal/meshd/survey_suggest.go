package meshd

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/status"
)

// Channel suggestions from a mesh-wide survey. Interference is
// local: a channel clear at one node may be busy at another, and a link is
// only as good as its worst end. So each candidate channel is judged by the
// worst node that surveyed it: its busiest 20 MHz channel in the link's
// width, and the networks heard there. Radar (DFS) channels are listed but
// not suggested: a move there waits a minute or more of radar watch, and
// leaves at once when radar is heard.

// surveyReport is one node's survey, as its reply carried it.
type surveyReport struct {
	Callsign string
	Links    []gossip.SurveyLink
}

// channelBlock lists the 20 MHz channels a link of this width occupies with
// its primary channel at ch: just ch on 2.4 GHz and at 20 MHz; the aligned
// 40, 80 or 160 MHz block on 5 GHz.
func channelBlock(band string, ch, width int) []int {
	if band != config.Band5G || width <= 20 {
		return []int{ch}
	}
	base := 36
	switch {
	case ch >= 149:
		base = 149
	case ch >= 100:
		base = 100
	}
	n := width / 20
	start := base + ((ch-base)/4/n)*n*4
	out := make([]int, n)
	for i := range out {
		out[i] = start + 4*i
	}
	return out
}

// isRadarChannel reports a 5 GHz channel in the radar bands (52–144), as
// most regulatory domains have them: radar is there whatever the rules in
// force say.
func isRadarChannel(band string, ch int) bool {
	return band == config.Band5G && ch >= 52 && ch <= 144
}

// overlaps reports whether a network on channel other is heard by a link
// whose block is blk: on 2.4 GHz a 20 MHz signal reaches 4 channels to
// either side.
func overlaps(band string, blk []int, other int) bool {
	if band == config.Band2G {
		for _, c := range blk {
			if d := c - other; d >= -4 && d <= 4 {
				return true
			}
		}
		return false
	}
	return slices.Contains(blk, other)
}

// scoreChannel judges primary channel ch for a link: across the reports,
// the worst node's busiest channel in the block and the most networks one
// node heard there. ok is false when no node surveyed the whole block.
func scoreChannel(link int, band string, ch, width int, reports []surveyReport) (s status.ChannelScore, ok bool) {
	blk := channelBlock(band, ch, width)
	s = status.ChannelScore{Ch: ch, WorstBusy: -1, DFS: slices.ContainsFunc(blk, func(c int) bool { return isRadarChannel(band, c) })}
	for _, r := range reports {
		for _, l := range r.Links {
			if l.Link != link || l.Band != band || len(l.Channels) == 0 {
				continue
			}
			byCh := map[int]gossip.SurveyCh{}
			for _, c := range l.Channels {
				byCh[c.Ch] = c
			}
			whole, busy, nets, strongest := true, -1, 0, 0
			for _, c := range blk {
				sc, have := byCh[c]
				if !have {
					whole = false
					break
				}
				busy = max(busy, sc.Busy)
			}
			if !whole {
				continue
			}
			for _, c := range l.Channels {
				if overlaps(band, blk, c.Ch) && c.Nets > 0 {
					nets += c.Nets
					if strongest == 0 || c.Strongest > strongest {
						strongest = c.Strongest
					}
				}
			}
			ok = true
			if busy > s.WorstBusy {
				s.WorstBusy, s.WorstNode = busy, r.Callsign
			}
			s.Nets = max(s.Nets, nets)
			if strongest != 0 && (s.Strongest == 0 || strongest > s.Strongest) {
				s.Strongest = strongest
			}
		}
	}
	s.Score = float64(max(s.WorstBusy, 0)) + 5*float64(min(s.Nets, 8))
	return s, ok
}

// suggestChannels judges every link of the profile against the surveys.
func suggestChannels(p *config.Profile, reports []surveyReport) []status.ChannelSuggestion {
	var out []status.ChannelSuggestion
	for i, l := range p.AllLinks() {
		link := i + 1
		sug := status.ChannelSuggestion{Link: link, Name: l.Name(), Width: l.Width}
		if l.Band == config.BandS1G {
			sug.Note = "HaLow radios report no channel survey"
			out = append(out, sug)
			continue
		}
		// Every channel some node surveyed for this link is a candidate
		// primary.
		seen := map[int]bool{}
		for _, r := range reports {
			for _, sl := range r.Links {
				if sl.Link == link && sl.Band == l.Band {
					for _, c := range sl.Channels {
						seen[c.Ch] = true
					}
				}
			}
		}
		var ranked []status.ChannelScore
		for _, ch := range slices.Sorted(maps.Keys(seen)) {
			if blk := channelBlock(l.Band, ch, l.Width); blk[0] != ch && l.Band == config.Band5G {
				continue // one candidate per block: its first channel
			}
			if l.Band == config.Band2G && !slices.Contains([]int{1, 6, 11, l.Channel}, ch) {
				// 1, 6 and 11 are the 2.4 GHz channels that do not
				// overlap; any other sits between two of them and
				// shares air with both. The link's own channel is
				// judged whatever it is.
				continue
			}
			if s, ok := scoreChannel(link, l.Band, ch, l.Width, reports); ok {
				ranked = append(ranked, s)
			}
		}
		if len(ranked) == 0 {
			sug.Note = "no node surveyed this link's band"
			out = append(out, sug)
			continue
		}
		cur, haveCur := scoreChannel(link, l.Band, l.Channel, l.Width, reports)
		if haveCur {
			cur.Ch = l.Channel
			sug.Current = &cur
		}
		slices.SortStableFunc(ranked, func(a, b status.ChannelScore) int {
			return cmp.Or(cmp.Compare(a.Score, b.Score), cmp.Compare(a.Ch, b.Ch))
		})
		sug.Ranked = ranked[:min(len(ranked), 6)]
		// The best is a channel some node measured as busy or not: one that
		// only heard networks there is not known to be clear.
		var best *status.ChannelScore
		for i := range ranked {
			if !ranked[i].DFS && ranked[i].WorstBusy >= 0 {
				best = &ranked[i]
				break
			}
		}
		switch {
		case !haveCur:
			sug.Note = "the link's own channel was not surveyed"
		case best == nil:
			sug.Note = "every surveyed channel is a radar channel"
		case slices.Contains(channelBlock(l.Band, best.Ch, l.Width), l.Channel):
			sug.Note = "this link's channel is the clearest one surveyed"
		case cur.Score >= 20 && cur.Score-best.Score >= max(10, 0.4*cur.Score):
			// A move interrupts the link everywhere: only for a channel
			// that is clearly busy, and a clearly better one, beyond what
			// one short listen per channel can tell apart.
			b := *best
			sug.Best = &b
			sug.Note = fmt.Sprintf("channel %d is clearer: at worst %s, against %s on channel %d", b.Ch, scoreWords(b), scoreWords(cur), l.Channel)
		case cur.Score < 20:
			sug.Note = fmt.Sprintf("channel %d is not busy (%s): no move is worth it", l.Channel, scoreWords(cur))
		default:
			sug.Note = fmt.Sprintf("no channel is clearly better than %d (the best, %d, is %s)", l.Channel, best.Ch, scoreWords(*best))
		}
		out = append(out, sug)
	}
	return out
}

// scoreWords says a score in words: "12% busy, 3 networks".
func scoreWords(s status.ChannelScore) string {
	busy := "busy unknown"
	if s.WorstBusy >= 0 {
		busy = fmt.Sprintf("%d%% busy", s.WorstBusy)
	}
	switch s.Nets {
	case 0:
		return busy + ", no other network"
	case 1:
		return busy + ", 1 other network"
	}
	return fmt.Sprintf("%s, %d other networks", busy, s.Nets)
}
