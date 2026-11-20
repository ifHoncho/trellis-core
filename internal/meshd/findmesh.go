package meshd

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/reconcile"
)

// Finding the mesh again. A node, or a group of nodes, can be left on
// settings the rest of the mesh has left: off or out of reach when the mesh
// moved (a mesh-wide change they never received), or back too late to move
// with it. There they hear nobody, or only each other, so gossip cannot
// tell them where the others went.
//
// The trigger is the roster (roster.go): the other nodes heard in the last
// day. A node that now hears half of them or fewer (itself counted on both
// sides) is in the smaller part of its mesh, and looks for the rest: alone
// for a minute, then after 1, 2, 4, 8 and 16 minutes, then every 30 while
// the rest stay missing. The larger part never looks, so one node switched
// off makes no one scan; a mesh of one node has no roster and never scans.
//
// A look is a passive scan, on each Wi-Fi link's radio, of every band the
// radio has (HaLow left out: its beacons come too seldom to catch passing
// by). Where it hears its own mesh ID on a channel other than its link's,
// the link pays a short visit there (one link at a time), at the width the
// beacons there give, without changing the profile. Gossip there settles
// it in seconds:
//   - nodes there are ahead: this node asks for their profile and switches
//     (meshchange.go), which ends the visit;
//   - nodes there are behind: they ask this node, and switch together to
//     its settings; the visit ends once they have left;
//   - the two were changed apart: whichever node finds it first settles it
//     for both (meshconflict.go);
//   - nodes there run the same: nothing to take or give; the link goes
//     back.
//
// A channel whose visit found nothing to take is not visited again for
// half an hour.
//
// Two things are never taken for the rest of the mesh. The profile's own
// channels: every link has the same mesh ID, and a radio hears the other
// links' beacons (a neighbour's, or this node's own second radio). And
// nodes at another site: when the site link is down, the nodes behind it
// are missing from gossip like nodes that moved, but no channel leads to
// them (siteLinkApart).

const (
	findMeshCheck      = 5 * time.Second  // how often the reconciler looks
	findMeshAfter      = 60 * time.Second // missing the rest this long before the first scan
	findMeshFirst      = 60 * time.Second // then scans this far apart, doubling
	findMeshEveryMax   = 30 * time.Minute // up to this
	findMeshTry        = 2 * time.Minute  // a visit must find a peer within this
	findMeshGone       = 30 * time.Second // the peers there gone this long ends a visit
	findMeshSettle     = 10 * time.Second // nothing to take or give this long ends a visit
	findMeshVisitMax   = 5 * time.Minute  // a visit ends after this whatever happens
	findMeshCheckedFor = 30 * time.Minute // a channel whose visit gave nothing waits this long
	findMeshSeen       = 10 * time.Second // beacons heard longer ago do not count
)

// findMesh is the daemon's search for the rest of its mesh.
type findMesh struct {
	mu           sync.Mutex
	missingSince time.Time // in the smaller part since; zero when not
	next         time.Time // the next scan
	every        time.Duration
	scanning     bool
	visit        *meshVisit
	checked      map[string]time.Time // by "link/band/channel": when a visit there gave nothing
	lastScan     time.Time
}

// meshVisit is a link visiting another channel where its mesh was heard.
type meshVisit struct {
	link     int              // from 0
	plan     config.RadioPlan // where it went
	from     config.RadioPlan // the link in the profile then
	rev      uint64           // the profile's revision then
	change   string           // and its change ID
	since    time.Time
	lastPeer time.Time       // when it last had a peer there
	settled  time.Time       // since when the nodes there run the same as this one
	before   map[string]bool // the nodes gossip heard when it went
}

// visitPlan is where link (from 0) runs instead of its profile's plan
// while it visits, if it does. A visit made under another profile does not
// count.
func (d *Daemon) visitPlan(link int, p *config.Profile) (config.RadioPlan, bool) {
	fm := &d.findMesh
	fm.mu.Lock()
	defer fm.mu.Unlock()
	v := fm.visit
	if v == nil || v.link != link || !v.matches(p) {
		return config.RadioPlan{}, false
	}
	return v.plan, true
}

func (v *meshVisit) matches(p *config.Profile) bool {
	rp, ok := p.Link(v.link)
	return ok && rp == v.from && p.Revision == v.rev && p.ChangeID() == v.change
}

// searchWanted reports whether a node that knows of known other nodes from
// the last day, and hears live of them now, is in the smaller part of its
// mesh (half or fewer, itself counted on both sides).
func searchWanted(known, live int) bool {
	return known > 0 && (live+1)*2 <= known+1
}

type findMeshReconciler struct{ d *Daemon }

func (r *findMeshReconciler) Name() string                       { return "findmesh" }
func (r *findMeshReconciler) Teardown(ctx context.Context) error { return nil }

func (r *findMeshReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := r.d
	res := reconcile.Result{RequeueAfter: findMeshCheck}
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	if msg, ok := d.meshConflictWarning(); ok {
		warn("warning", "mesh-change-conflict", "%s", msg)
	}
	_, profile := d.conf()
	if profile == nil || d.setupState().Needed || d.nl == nil || d.peers == nil {
		return res, nil
	}
	now := time.Now()
	ifindex := map[int]int{} // by link (from 0)
	d.facts.Read(func(f *Facts) {
		ifindex[0] = f.MeshIfindex
		for i, l := range f.MoreLinks {
			ifindex[i+1] = l.Ifindex
		}
	})
	records := map[string]gossip.Record{}
	for _, p := range d.peers.Peers() {
		records[p.Record.MAC] = p.Record
	}
	known := d.rosterKnown()
	for mac, r := range records {
		known[mac] = r.Callsign
	}
	wanted := searchWanted(len(known), len(records))
	// Missing because the site link is down: nothing to find by radio.
	if wanted {
		var own *gossip.SiteState
		if d.sitelink != nil {
			own = d.siteLinkGossip()
		}
		if siteLinkApart(profile.SiteLink, own, records) {
			wanted = false
			missing := len(known) - len(records)
			warn("info", "mesh-apart", "%d/%d nodes heard in the last day not in range, site link incomplete: treated as another site; no channel search for them",
				missing, len(known))
		}
	}

	fm := &d.findMesh
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if v := fm.visit; v != nil {
		if why, gave := d.tendVisit(v, profile, records, ifindex[v.link], now); why != "" {
			if gave {
				d.log.Info("the visit ends: back to the profile's channel", "link", v.link+1, "visited", v.plan.Name(), "why", why)
			} else {
				d.log.Warn("the visit ends: back to the profile's channel", "link", v.link+1, "visited", v.plan.Name(), "why", why)
				fm.markChecked(v.link, v.plan, now)
			}
			fm.visit = nil
			res.Changed = true
			d.Kick("mesh link back from a visit")
		}
	}

	switch {
	case !wanted:
		fm.missingSince, fm.next, fm.every = time.Time{}, time.Time{}, 0
	case fm.missingSince.IsZero():
		fm.missingSince, fm.next = now, now.Add(findMeshAfter)
	case fm.visit == nil && !fm.scanning && !now.Before(fm.next):
		fm.every = min(max(2*fm.every, findMeshFirst), findMeshEveryMax)
		fm.next = now.Add(fm.every)
		fm.scanning = true
		go d.findMeshScan(profile)
	}

	if v := fm.visit; v != nil {
		warn("warning", "mesh-visit", "%s link scanning %s for the rest of the mesh (since %s; returns to ch %d)",
			bandName(v.from.Band), v.plan.Name(), now.Sub(v.since).Round(time.Second), v.from.Channel)
	} else if wanted && now.Sub(fm.missingSince) >= findMeshAfter {
		missing := len(known) - len(records)
		msg := fmt.Sprintf("%d of the %d other nodes heard in the last day %s not in reach; looking for the rest of the mesh on other channels",
			missing, len(known), map[bool]string{true: "is", false: "are"}[missing == 1])
		if !fm.lastScan.IsZero() {
			msg += fmt.Sprintf(" (last look %s ago, next in %s)", now.Sub(fm.lastScan).Round(time.Second), max(0, fm.next.Sub(now)).Round(time.Second))
		}
		warn("info", "mesh-alone", "%s", msg)
	}
	return res, nil
}

// tendVisit says whether a visit is over, and why ("" while it goes on);
// gave: it ended by doing its work (the profile changed, or the nodes there
// took this node's), so the channel is not held against.
func (d *Daemon) tendVisit(v *meshVisit, profile *config.Profile, records map[string]gossip.Record, ifindex int, now time.Time) (why string, gave bool) {
	if !v.matches(profile) {
		return "the mesh profile changed", true
	}
	if d.linkHasPeer(ifindex) {
		v.lastPeer = now
	}
	mine, have := d.latestProfile(), d.currentRevision()
	pending := false
	if c := d.meshChangeState(); c != nil && c.State == "pending" {
		pending = true
	}
	newcomers, open := 0, false
	for mac, r := range records {
		if v.before[mac] {
			continue
		}
		newcomers++
		if r.ProfileRev != have || (r.ProfileChange != "" && mine.ChangeID() != "" && r.ProfileChange != mine.ChangeID()) {
			open = true // something to take or give
		}
	}
	switch {
	case now.Sub(v.since) >= findMeshVisitMax:
		return "no outcome in " + findMeshVisitMax.String(), false
	case v.lastPeer.IsZero() && now.Sub(v.since) >= findMeshTry:
		return "no node there peered within " + findMeshTry.String(), false
	case !v.lastPeer.IsZero() && now.Sub(v.lastPeer) >= findMeshGone && !pending:
		// The nodes there left: they took this node's profile and moved
		// to its channels, or went for another reason.
		return "the nodes there left", newcomers > 0
	case newcomers > 0 && !open && !pending:
		if v.settled.IsZero() {
			v.settled = now
		} else if now.Sub(v.settled) >= findMeshSettle {
			return "the nodes there run the same profile: nothing to take or give", false
		}
	default:
		v.settled = time.Time{}
	}
	return "", false
}

func (fm *findMesh) markChecked(link int, p config.RadioPlan, now time.Time) {
	if fm.checked == nil {
		fm.checked = map[string]time.Time{}
	}
	fm.checked[checkedKey(link, p.Band, p.Channel)] = now
}

func (fm *findMesh) checkedLately(link int, band string, ch int, now time.Time) bool {
	t, ok := fm.checked[checkedKey(link, band, ch)]
	return ok && now.Sub(t) < findMeshCheckedFor
}

func checkedKey(link int, band string, ch int) string { return fmt.Sprintf("%d/%s/%d", link, band, ch) }

// findMeshScan scans for this mesh with each Wi-Fi link's radio and sends
// one link to visit the best channel it was heard on elsewhere.
func (d *Daemon) findMeshScan(profile *config.Profile) {
	fm := &d.findMesh
	defer func() {
		fm.mu.Lock()
		fm.scanning, fm.lastScan = false, time.Now()
		fm.mu.Unlock()
		d.Kick("looked for the mesh")
	}()
	s := &d.survey
	s.runMu.Lock()
	defer s.runMu.Unlock()
	type choice struct {
		link  int
		plan  config.RadioPlan
		heard heardNet
	}
	var best *choice
	var all []string
	for _, t := range d.findMeshTargets(profile) {
		link := t.link - 1
		rp, ok := profile.Link(link)
		if !ok {
			continue
		}
		if err := d.scanOnce(t.ifindex, t.freqs); err != nil {
			d.log.Info("could not look for the mesh", "link", t.link, "err", err)
			continue
		}
		bss, err := d.nl.Scan(t.ifindex)
		if err != nil {
			continue
		}
		heard := meshHeard(bss, profile.MeshID, findMeshSeen)
		now := time.Now()
		fm.mu.Lock()
		for _, h := range heard {
			all = append(all, fmt.Sprintf("link %d: %s", t.link, h.plan().Name()))
			if onProfile(profile, h.band, h.channel) || fm.checkedLately(link, h.band, h.channel, now) {
				continue
			}
			c := choice{link: link, plan: h.plan(), heard: h}
			if best == nil || betterHeard(c.heard, rp.Band, best.heard, best.plan.Band) {
				best = &c
			}
		}
		fm.mu.Unlock()
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if _, cur := d.conf(); cur.Revision != profile.Revision || cur.ChangeID() != profile.ChangeID() || fm.visit != nil {
		return // the profile changed meanwhile: it says where to be
	}
	if best == nil {
		d.log.Info("looked for the rest of the mesh on other channels: nowhere new", "heard", fmt.Sprint(all))
		return
	}
	from, _ := profile.Link(best.link)
	v := &meshVisit{link: best.link, plan: best.plan, from: from, rev: profile.Revision, change: profile.ChangeID(), since: time.Now(), before: map[string]bool{}}
	if d.peers != nil {
		for _, p := range d.peers.Peers() {
			v.before[p.Record.MAC] = true
		}
	}
	fm.visit = v
	d.log.Warn("most of the mesh is missing; this mesh was heard elsewhere: the link visits there to see",
		"link", best.link+1, "profile", from.Name(), "visits", best.plan.Name(), "signal_dbm", best.heard.signal, "heard", fmt.Sprint(all))
}

// onProfile reports whether a channel is one of the profile's own links.
// The mesh heard there is this part of it, on its other link: not the
// rest of the mesh, and nowhere to visit.
func onProfile(p *config.Profile, band string, channel int) bool {
	for _, l := range p.AllLinks() {
		if l.Band == band && l.Channel == channel {
			return true
		}
	}
	return false
}

// siteLinkApart reports whether the site link is what keeps nodes out of
// reach: the profile links two sites or more, and a member in reach (own
// is this node's part, nil when it is no member; live the records heard
// now) says a tunnel that was up in the last day is down. The nodes
// behind it went missing with it, and no channel leads to them. Only that
// evidence counts:
//   - a member merely out of reach is also what a node stranded on an old
//     channel sees, hearing no one, and that node must look for the mesh;
//   - a tunnel that never came up (two members behind NAT, a member
//     switched off for good) has no nodes behind it that were ever in
//     reach, and would stand the search down for as long as it is listed.
func siteLinkApart(plan config.SiteLinkPlan, own *gossip.SiteState, live map[string]gossip.Record) bool {
	sites := map[string]bool{}
	for _, m := range plan.Members {
		sites[m.Site] = true
	}
	if len(sites) < 2 {
		return false
	}
	if own != nil && own.Lost > 0 {
		return true
	}
	for _, r := range live {
		if r.Site != nil && r.Site.Lost > 0 {
			return true
		}
	}
	return false
}

// heardNet is this mesh heard on one channel.
type heardNet struct {
	band           string
	channel, width int
	signal         int // the strongest beacon, dBm
}

// plan is where a visit there runs: the width the beacons gave, else 20 MHz
// (which peers with a wider mesh on the same primary channel).
func (h heardNet) plan() config.RadioPlan {
	w := h.width
	if w == 0 {
		w = 20
	}
	return config.RadioPlan{Band: h.band, Channel: h.channel, Width: w}
}

// betterHeard: the link's own band first, then the stronger signal, then
// the lower channel.
func betterHeard(a heardNet, aHome string, b heardNet, bHome string) bool {
	if (a.band == aHome) != (b.band == bHome) {
		return a.band == aHome
	}
	if a.signal != b.signal {
		return a.signal > b.signal
	}
	return a.channel < b.channel
}

// meshHeard is the mesh meshID among networks a scan heard lately: by band
// and channel, the strongest beacon and the width it gives.
func meshHeard(bss []nl80211.BSS, meshID string, seen time.Duration) []heardNet {
	byKey := map[string]heardNet{}
	for _, b := range bss {
		if b.MeshID != meshID || meshID == "" || time.Duration(b.SeenMSAgo)*time.Millisecond > seen {
			continue
		}
		band := nl80211.BandOfFrequency(b.FreqMHz).String()
		ch := channelOf(band, b.FreqMHz)
		if f, err := config.ChannelFrequencyMHz(band, ch); ch <= 0 || err != nil || uint32(f) != b.FreqMHz {
			continue
		}
		s := int(b.SignalDBm)
		if s == 0 {
			s = -100
		}
		k := fmt.Sprintf("%s/%d", band, ch)
		if old, ok := byKey[k]; !ok || s > old.signal {
			w := b.WidthMHz
			if ok && w == 0 {
				w = old.width
			}
			byKey[k] = heardNet{band: band, channel: ch, width: w, signal: s}
		}
	}
	out := slices.Collect(maps.Values(byKey))
	sort.Slice(out, func(i, j int) bool {
		if out[i].band != out[j].band {
			return out[i].band < out[j].band
		}
		return out[i].channel < out[j].channel
	})
	return out
}

// findMeshTargets are this node's Wi-Fi mesh links, each with every
// channel of every band its radio has (HaLow left out).
func (d *Daemon) findMeshTargets(profile *config.Profile) []scanTarget {
	var targets []scanTarget
	d.facts.Read(func(f *Facts) {
		for i, l := range profile.AllLinks() {
			ifindex := f.MeshIfindex
			if i > 0 {
				if i-1 >= len(f.MoreLinks) {
					continue
				}
				ifindex = f.MoreLinks[i-1].Ifindex
			}
			if ifindex == 0 || f.Plan == nil || l.Band == config.BandS1G {
				continue
			}
			rd := f.Plan.Link(i)
			if rd == nil || rd.Wiphy == nil {
				continue
			}
			t := scanTarget{link: i + 1, ifindex: ifindex, band: l.Band}
			for _, band := range slices.Sorted(maps.Keys(rd.Wiphy.Bands)) {
				if band == nl80211.BandS1GHz || band == nl80211.Band60GHz || rd.Wiphy.Bands[band] == nil {
					continue
				}
				for _, fr := range rd.Wiphy.Bands[band].Frequencies {
					if !fr.Disabled {
						t.freqs = append(t.freqs, fr.MHz)
					}
				}
			}
			targets = append(targets, t)
		}
	})
	return targets
}

// linkHasPeer reports whether a mesh interface has an established peer.
func (d *Daemon) linkHasPeer(ifindex int) bool {
	if ifindex == 0 || d.nl == nil {
		return false
	}
	sts, err := d.nl.Stations(ifindex)
	if err != nil {
		return false
	}
	for _, s := range sts {
		if s.Established() {
			return true
		}
	}
	return false
}
