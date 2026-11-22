package meshd

import (
	"fmt"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/reconcile"
)

// The congestion alert: a mesh link whose channel stays busy at some node
// is worth a channel survey. It is built from the busy figures every node
// already samples on its own link channels and sends in gossip (Load), so
// it costs nothing on the air; the survey, and any move, stay the
// operator's. The channel is judged by its busiest node, as the survey
// judges it: interference is local.

const (
	congestOnPct  = 60               // busy at or above this …
	congestOnFor  = 10 * time.Minute // … for this long raises the alert
	congestOffPct = 50               // below this …
	congestOffFor = 2 * time.Minute  // … for this long clears it
)

// peerQuiet: a peer not heard this long (two gossip refreshes) is taken
// as gone, though its record has not expired: its last busy figure is not
// the channel's now.
const peerQuiet = 2 * gossipRefresh

// heardLately reports a peer heard within peerQuiet.
func heardLately(p gossip.Peer) bool { return time.Since(p.LastHeard) <= peerQuiet }

// congestLink is one link's state.
type congestLink struct {
	busySince  time.Time // first sample at or above congestOnPct in the current run
	clearSince time.Time // first sample below congestOffPct while raised
	raised     bool
	worst      int    // the last worst figure, percent
	at         string // the node it was measured at
}

// congestion follows every link of the profile.
type congestion struct {
	links map[string]*congestLink // by the link's "band/channel/width": a move starts afresh
}

// linkSample is the busiest node's figure on one link, -1 when no node
// measures it.
type linkSample struct {
	worst int
	at    string
}

// observe takes one sample per link and returns the alerts in force.
func (c *congestion) observe(now time.Time, names []string, keys []string, samples []linkSample) []reconcile.Warning {
	if c.links == nil {
		c.links = map[string]*congestLink{}
	}
	seen := map[string]bool{}
	var out []reconcile.Warning
	for i, s := range samples {
		key := keys[i]
		seen[key] = true
		l := c.links[key]
		if l == nil {
			l = &congestLink{}
			c.links[key] = l
		}
		switch {
		case s.worst >= congestOnPct:
			l.clearSince = time.Time{}
			if l.busySince.IsZero() {
				l.busySince = now
			}
			if now.Sub(l.busySince) >= congestOnFor {
				l.raised = true
			}
			l.worst, l.at = s.worst, s.at
		case s.worst < congestOffPct:
			// Under the lower mark, or nobody measures the link now (-1):
			// a busy run ends, and a raised alert clears once it lasts.
			l.busySince = time.Time{}
			if l.raised {
				if l.clearSince.IsZero() {
					l.clearSince = now
				}
				if now.Sub(l.clearSince) >= congestOffFor {
					l.raised, l.clearSince = false, time.Time{}
				}
			}
			// While the alert stands (clearing), it keeps the busy figure
			// that raised it, not "busy: 20 %".
			if !l.raised && s.worst >= 0 {
				l.worst, l.at = s.worst, s.at
			}
		default:
			// Between the two marks: a raised alert stays, and a run
			// towards one goes on (a channel around 60 % dips under it
			// now and then); only under 50 % does it start again.
			l.clearSince = time.Time{}
			l.worst, l.at = s.worst, s.at
		}
		if l.raised {
			msg := fmt.Sprintf("Channel busy: %s, %d %% at %s (mesh traffic included). Run channel survey", names[i], l.worst, l.at)
			out = append(out, reconcile.Warning{Level: "warning", Code: "channel-busy", Message: msg})
		}
	}
	for k := range c.links {
		if !seen[k] {
			delete(c.links, k)
		}
	}
	return out
}

// linkSamples is the busiest node's figure on each of the profile's links:
// this node's own sample and every node's in gossip that runs the link as
// this node does.
func (d *Daemon) linkSamples(p *config.Profile) (names, keys []string, out []linkSample) {
	links := p.AllLinks()
	entries := profileLinkEntries(p)
	keys = congestKeysOf(p)
	names = make([]string, len(links))
	out = make([]linkSample, len(links))
	me := "this node" // a callsign is required (config); never shown blank
	if cfg, _ := d.conf(); cfg != nil && cfg.Callsign != "" {
		me = cfg.Callsign
	}
	for i, l := range links {
		names[i] = l.Name()
		out[i] = linkSample{worst: -1}
		if i >= len(keys) {
			keys = append(keys, fmt.Sprintf("%d", i))
			entries = append(entries, "")
		}
		// This node's own figure only while the link is on the plan's
		// channel: off it (a recovery visit, a peer's switch) the figure
		// is another channel's, as a peer's on another plan is.
		if ll := d.linkLoad(i + 1); ll.busy >= 0 && onPlan(l, ll.freq) {
			out[i] = linkSample{worst: ll.busy, at: me}
		}
	}
	if d.peers == nil {
		return names, keys, out
	}
	// Every node on this site's air: the busiest decides, wherever it
	// falls in the table (a few ints a node; the table holds the mesh's
	// own nodes, by key). A node at another site of the site link is on
	// other air: its channel says nothing about this one's.
	mySite := d.mySite()
	for _, peer := range d.peers.Peers() {
		r := peer.Record
		// A node at another site; or one gone quiet (left the mesh, its
		// record not expired yet): its last figure is not the channel's now.
		if otherSite(r, mySite) || !heardLately(peer) {
			continue
		}
		for i := range out {
			if i >= len(r.Load) || i >= len(r.Links) || r.Links[i] != entries[i] || r.Load[i] < 0 || i >= gossip.MaxLinkEntries {
				continue
			}
			if r.Load[i] > out[i].worst {
				out[i] = linkSample{worst: r.Load[i], at: r.Callsign}
			}
		}
	}
	return names, keys, out
}

// mySite is this node's site while it is a site link member, else "".
func (d *Daemon) mySite() string {
	var site string
	d.facts.Read(func(f *Facts) {
		if f.SiteLink.Member {
			site = f.SiteLink.Site
		}
	})
	return site
}

// otherSite reports a node at another site of the site link than mine:
// reached over the internet, on other air.
func otherSite(r gossip.Record, mine string) bool {
	return mine != "" && r.Site != nil && r.Site.Site != "" && r.Site.Site != mine
}

// onPlan reports whether a figure measured on freq (0 unknown) is the
// plan's channel's.
func onPlan(plan config.RadioPlan, freq uint32) bool {
	want, err := config.ChannelFrequencyMHz(plan.Band, plan.Channel)
	return freq == 0 || err != nil || want == int(freq)
}

// congestKeysOf is what the alert's state is kept by, per profile link: its
// index and entry, so two links on the same channel (dual backhaul) are
// two, and a moved link starts afresh.
func congestKeysOf(p *config.Profile) []string {
	entries := profileLinkEntries(p)
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = fmt.Sprintf("%d %s", i, e)
	}
	return out
}
