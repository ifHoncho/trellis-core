package meshd

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
)

// Channel survey, in two parts.
//
// Always: how busy each mesh link's own channel is where this node is, from
// the driver's survey counters, sampled every loadEvery. It costs nothing on
// the air, shows in status and travels in gossip, so every node sees the
// load of each link everywhere. Drivers that keep no survey (ath9k_htc,
// rt2800usb) report none.
//
// On request: a mesh-wide survey. The node asked sends every node a query;
// each scans the band of each of its mesh links once, passively (listening
// only, about a tenth of a second per channel, the link pausing meanwhile),
// and answers with each channel's busy time and the other networks heard
// there. The asking node judges every link's channels (survey_suggest.go).

const (
	loadEvery        = 10 * time.Second
	surveyScanWait   = 20 * time.Second // a scan of a whole band ends well within this
	surveyRoundWait  = 45 * time.Second // the asking node waits this long for answers
	surveyAskAgain   = 5 * time.Second  // and asks again those that have not answered
	surveyCacheFor   = 30 * time.Second // a node scans again only after this
	surveyRecentSeen = 30 * time.Second // networks heard longer ago do not count
)

// surveyState is the daemon's survey: the sampled loads, this node's last
// survey, and the round it asked for last.
type surveyState struct {
	mu   sync.Mutex
	prev map[int]nl80211.SurveyInfo // by ifindex: the in-use channel at the last sample
	load map[int]linkLoad           // by link (from 1)
	last time.Time

	runMu   sync.Mutex // one scan of this node's links at a time
	local   []gossip.SurveyLink
	localAt time.Time

	round *surveyRound

	// congest: the channel-busy alert (congestion.go), and the alerts it
	// raised at the last sample, shown on every pass.
	congest     congestion
	congestOut  []reconcile.Warning
	congestKeys []string // the profile's links congestOut was judged on
}

type linkLoad struct {
	busy  int    // percent; -1 unknown
	noise int    // dBm; 0 unknown
	freq  uint32 // MHz: the channel it was measured on; 0 unknown
}

type surveyRound struct {
	nonce   uint64
	started time.Time
	done    bool
	nodes   map[string]*surveyNodeState // by identity MAC
	order   []string
}

type surveyNodeState struct {
	callsign string
	addr     net.IP
	self     bool
	state    string
	links    []gossip.SurveyLink
}

// surveyReconciler samples the links' channel load.
type surveyReconciler struct{ d *Daemon }

func (r *surveyReconciler) Name() string                       { return "survey" }
func (r *surveyReconciler) Teardown(ctx context.Context) error { return nil }
func (r *surveyReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := r.d
	s := &d.survey
	s.mu.Lock()
	due := time.Since(s.last) >= loadEvery
	if due {
		s.last = time.Now()
	}
	s.mu.Unlock()
	if !due {
		s.mu.Lock()
		if d.profile == nil {
			// No profile, no links: nothing to judge, nothing kept.
			s.congestOut, s.congest = nil, congestion{}
		} else if !slices.Equal(s.congestKeys, congestKeysOf(d.profile)) {
			// The links changed (a move, say): the alerts were about
			// the old ones. The next sample judges the new.
			s.congestOut = nil
		}
		w := append([]reconcile.Warning(nil), s.congestOut...)
		s.mu.Unlock()
		return reconcile.Result{RequeueAfter: loadEvery, Warnings: w}, nil
	}
	links := map[int]int{} // link → ifindex
	d.facts.Read(func(f *Facts) {
		if f.MeshIfindex != 0 {
			links[1] = f.MeshIfindex
		}
		for i, l := range f.MoreLinks {
			if l.Ifindex != 0 {
				links[i+2] = l.Ifindex
			}
		}
	})
	loads := map[int]linkLoad{}
	prev := map[int]nl80211.SurveyInfo{}
	s.mu.Lock()
	for k, v := range s.prev {
		prev[k] = v
	}
	s.mu.Unlock()
	// Without nl80211 this node measures nothing of its own; the alert is
	// still judged on the other nodes' figures (below).
	if d.nl == nil {
		links = nil
	}
	for link, ifindex := range links {
		l := linkLoad{busy: -1}
		if infos, err := d.nl.Survey(ifindex); err == nil {
			for _, in := range infos {
				if !in.InUse {
					continue
				}
				if p, ok := prev[ifindex]; ok && p.FreqMHz == in.FreqMHz && in.ActiveMS > p.ActiveMS && in.BusyMS >= p.BusyMS {
					l.busy = min(100, int((in.BusyMS-p.BusyMS)*100/(in.ActiveMS-p.ActiveMS)))
				}
				if in.HasNoise {
					l.noise = int(in.NoiseDBm)
				}
				l.freq = in.FreqMHz
				prev[ifindex] = in
			}
		}
		loads[link] = l
	}
	s.mu.Lock()
	s.prev, s.load = prev, loads
	s.mu.Unlock()
	var warns []reconcile.Warning
	if d.profile != nil {
		names, keys, samples := d.linkSamples(d.profile)
		s.mu.Lock()
		s.congestOut = s.congest.observe(time.Now(), names, keys, samples)
		s.congestKeys = congestKeysOf(d.profile)
		warns = append(warns, s.congestOut...)
		s.mu.Unlock()
	} else {
		// No profile, no links: nothing to judge, nothing kept.
		s.mu.Lock()
		s.congestOut, s.congest = nil, congestion{}
		s.mu.Unlock()
	}
	return reconcile.Result{RequeueAfter: loadEvery, Warnings: warns}, nil
}

// linkLoad is the sampled load of link (from 1): busy -1 when unknown.
func (d *Daemon) linkLoad(link int) linkLoad {
	s := &d.survey
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.load[link]; ok {
		return l
	}
	return linkLoad{busy: -1}
}

// gossipLoads is the Load field of this node's record, one per profile link.
func (d *Daemon) gossipLoads(links []config.RadioPlan) []int {
	out := make([]int, 0, len(links))
	known := false
	for i := 1; i <= len(links) && i <= gossip.MaxLinkEntries; i++ {
		ll := d.linkLoad(i)
		b := ll.busy
		// A figure from another channel than the plan's (a recovery
		// visit, a peer's switch) is not the link's: the others take Load
		// as the plan's channel (congestion.go).
		if !onPlan(links[i-1], ll.freq) {
			b = -1
		}
		known = known || b >= 0
		out = append(out, b)
	}
	if !known {
		return nil
	}
	return out
}

// channelOf turns a frequency into its channel number in a band, as
// config.ChannelFrequencyMHz numbers them (4.9 GHz as 5 GHz channels 182–196,
// 6 GHz channel 2 at 5935 MHz), or 0 for a frequency that numbering has no
// channel for. The frequency is unsigned: computed blindly, one below a
// band's base would wrap round to a channel in the hundreds of millions.
func channelOf(band string, mhz uint32) int {
	f := int(mhz)
	ch := 0
	switch band {
	case config.Band2G:
		ch = (f - 2407) / 5
		if f == 2484 {
			ch = 14
		}
	case config.Band5G:
		ch = (f - 5000) / 5
		if f >= 4910 && f <= 4980 {
			ch = (f - 4000) / 5
		}
	case config.Band6G:
		ch = (f - 5950) / 5
		if f == 5935 {
			ch = 2
		}
	}
	if back, err := config.ChannelFrequencyMHz(band, ch); ch <= 0 || err != nil || back != f {
		return 0
	}
	return ch
}

// surveyLocal scans the band of each of this node's mesh links once, or
// returns the last survey when it is recent.
func (d *Daemon) surveyLocal(ctx context.Context) []gossip.SurveyLink {
	s := &d.survey
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if time.Since(s.localAt) < surveyCacheFor && s.local != nil {
		return s.local
	}
	_, profile := d.conf()
	var out []gossip.SurveyLink
	for _, t := range d.scanTargets(profile) {
		sl := gossip.SurveyLink{Link: t.link, Band: t.band}
		chs, err := d.surveyBand(ctx, t.link, t.ifindex, t.band, t.freqs, profile.MeshID)
		if err != nil {
			sl.Err = gossip.TrimErr(err.Error())
		}
		sl.Channels = chs
		out = append(out, sl)
	}
	s.local, s.localAt = out, time.Now()
	d.log.Info("channel survey done", "links", len(out))
	return out
}

// scanTarget is one mesh link to scan: its interface, band and the channels
// its radio may use there.
type scanTarget struct {
	link, ifindex int // link from 1
	band          string
	freqs         []uint32
}

// scanTargets are this node's Wi-Fi mesh links that can scan (HaLow is left
// out: its beacons come once a second, too slow to catch passing by).
func (d *Daemon) scanTargets(profile *config.Profile) []scanTarget {
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
			band, ok := nl80211.ParseBand(l.Band)
			if rd == nil || rd.Wiphy == nil || !ok || rd.Wiphy.Bands[band] == nil {
				continue
			}
			t := scanTarget{link: i + 1, ifindex: ifindex, band: l.Band}
			for _, fr := range rd.Wiphy.Bands[band].Frequencies {
				if !fr.Disabled {
					t.freqs = append(t.freqs, fr.MHz)
				}
			}
			targets = append(targets, t)
		}
	})
	return targets
}

// scanOnce has the radio of one interface scan freqs, passively, and waits
// until it has.
func (d *Daemon) scanOnce(ifindex int, freqs []uint32) error {
	if d.nl == nil || len(freqs) == 0 {
		return errors.New("no channels to scan")
	}
	ev, err := nl80211.DialScanEvents()
	if err != nil {
		return err
	}
	defer ev.Close()
	if err := d.nl.TriggerScan(ifindex, freqs); err != nil {
		return err
	}
	deadline := time.Now().Add(surveyScanWait)
	_ = ev.SetDeadline(deadline)
	for time.Now().Before(deadline) {
		evs, err := ev.Next()
		if err != nil {
			return fmt.Errorf("waiting for the scan: %w", err)
		}
		for _, e := range evs {
			if e.Ifindex != ifindex {
				continue
			}
			switch e.Command {
			case nl80211.NL80211_CMD_NEW_SCAN_RESULTS:
				return nil
			case nl80211.NL80211_CMD_SCAN_ABORTED:
				return errors.New("the scan was aborted")
			}
		}
	}
	return errors.New("the scan did not finish")
}

// surveyBand scans freqs on one interface and reads what the radio found.
func (d *Daemon) surveyBand(ctx context.Context, link, ifindex int, band string, freqs []uint32, meshID string) ([]gossip.SurveyCh, error) {
	if d.nl == nil || len(freqs) == 0 {
		return nil, errors.New("no channels to scan")
	}
	before, _ := d.nl.Survey(ifindex)
	if err := d.scanOnce(ifindex, freqs); err != nil {
		return nil, err
	}
	after, _ := d.nl.Survey(ifindex)
	bss, _ := d.nl.Scan(ifindex)

	was := map[uint32]nl80211.SurveyInfo{}
	for _, b := range before {
		was[b.FreqMHz] = b
	}
	now := map[uint32]nl80211.SurveyInfo{}
	for _, a := range after {
		now[a.FreqMHz] = a
	}
	own := d.linkLoad(link)
	perVisit := countsPerVisit(was, now)
	var out []gossip.SurveyCh
	for _, f := range freqs {
		c := gossip.SurveyCh{Ch: channelOf(band, f), Busy: -1}
		if c.Ch <= 0 {
			continue
		}
		if a, ok := now[f]; ok {
			if a.InUse {
				// The scan barely visits its own channel: the link's
				// sampled figure, when it was measured on this channel.
				if own.freq == 0 || own.freq == f {
					c.Busy = own.busy
				}
			} else {
				c.Busy = visitBusy(was[f], a, perVisit)
			}
			if a.HasNoise {
				c.Noise = int(a.NoiseDBm)
			}
		}
		for _, n := range bss {
			if n.FreqMHz != f || (n.MeshID != "" && n.MeshID == meshID) || time.Duration(n.SeenMSAgo)*time.Millisecond > surveyRecentSeen {
				continue
			}
			c.Nets++
			if s := int(n.SignalDBm); s != 0 && (c.Strongest == 0 || s > c.Strongest) {
				c.Strongest = s
			}
		}
		out = append(out, c)
		if len(out) == gossip.MaxSurveyChannels {
			break
		}
	}
	if saturated(out, own.busy) {
		for i := range out {
			if out[i].Busy >= 0 && !(own.busy >= 0 && out[i].Busy == own.busy) {
				out[i].Busy = -1
			}
		}
		return out, errors.New("Radio busy-time data unusable during scan (all channels reported fully busy); networks heard only")
	}
	return out, nil
}

// minVisitMS: a passive scan stays about 110 ms on a channel; a counter
// that grew by less did not see a visit of its own. maxVisitMS: no scan
// stays longer than this on one channel.
const (
	minVisitMS = 20
	maxVisitMS = 300
)

// countsPerVisit reports a radio whose off-channel survey counters hold
// the last scan visit alone (mt76 zeroes them each time it tunes there)
// rather than a running total. Either sign says so: some off-channel
// counter went down across the scan, which a running total never does;
// or none after the scan is more than one visit long (a running total in
// its first scans since boot reads the same, and its figure is then near
// enough the visit's). Judged at each scan, nothing remembered.
func countsPerVisit(before, after map[uint32]nl80211.SurveyInfo) bool {
	short, seen := true, false
	for f, a := range after {
		if a.InUse {
			continue
		}
		if b, ok := before[f]; ok && !b.InUse && (a.ActiveMS < b.ActiveMS || a.BusyMS < b.BusyMS) {
			return true
		}
		if a.ActiveMS > 0 {
			seen = true
			short = short && a.ActiveMS <= maxVisitMS
		}
	}
	return seen && short
}

// visitBusy is an off-channel's busy share during a scan, in percent, from
// its counters before (b) and after (a), or -1 unknown. perVisit: a holds
// the visit alone (countsPerVisit); otherwise the visit is a less b.
func visitBusy(b, a nl80211.SurveyInfo, perVisit bool) int {
	switch {
	case perVisit:
		// The figures are the last visit's: this scan's, or (counters the
		// same as before it) the one before, minutes old at most. Equal
		// figures are common on a quiet channel and prove nothing.
		if a.ActiveMS >= minVisitMS && a.BusyMS <= a.ActiveMS {
			return min(100, int(a.BusyMS*100/a.ActiveMS))
		}
	case a.ActiveMS >= b.ActiveMS+minVisitMS && a.BusyMS >= b.BusyMS:
		return min(100, int((a.BusyMS-b.BusyMS)*100/(a.ActiveMS-b.ActiveMS)))
	}
	return -1
}

// saturated reports a radio whose busy times during a scan mean nothing:
// nearly every channel it measured off its own reads 90% or more (the
// MT7610U on 5 GHz counts the whole visit as busy). own is its own
// channel's load, measured the usual way, which is left out.
func saturated(chs []gossip.SurveyCh, own int) bool {
	measured, full := 0, 0
	for _, c := range chs {
		if c.Busy < 0 || (own >= 0 && c.Busy == own) {
			continue
		}
		measured++
		if c.Busy >= 90 {
			full++
		}
	}
	return measured >= 4 && full*10 >= measured*7
}

// StartSurvey begins a mesh-wide survey (POST /survey), or returns the one
// under way.
func (d *Daemon) StartSurvey(ctx context.Context) (*status.Survey, error) {
	me := d.identity()
	if me == nil || d.gossip == nil || d.gossip.currentKey() == nil || d.peers == nil {
		return nil, errors.New("Gossip not started; retry shortly")
	}
	s := &d.survey
	s.mu.Lock()
	if r := s.round; r != nil && !r.done {
		s.mu.Unlock()
		return d.Survey(), nil
	}
	var nb [8]byte
	_, _ = rand.Read(nb[:])
	cur, _ := d.conf()
	r := &surveyRound{nonce: binary.BigEndian.Uint64(nb[:]) | 1, started: time.Now(), nodes: map[string]*surveyNodeState{}}
	r.nodes[me.MAC] = &surveyNodeState{callsign: cur.Callsign, self: true, state: "asked"}
	r.order = append(r.order, me.MAC)
	for _, p := range d.peers.Peers() {
		rec := p.Record
		ip := net.ParseIP(rec.Addr)
		if ip == nil {
			continue
		}
		r.nodes[rec.MAC] = &surveyNodeState{callsign: rec.Callsign, addr: ip, state: "asked"}
		r.order = append(r.order, rec.MAC)
	}
	s.round = r
	s.mu.Unlock()
	d.log.Info("mesh-wide channel survey started", "nodes", len(r.order))

	go func() {
		links := d.surveyLocal(context.Background())
		s.mu.Lock()
		if n := r.nodes[me.MAC]; n != nil {
			n.state, n.links = "answered", links
		}
		s.mu.Unlock()
	}()
	go d.runSurveyRound(r)
	return d.Survey(), nil
}

// runSurveyRound asks every node that has not answered, again and again,
// until all have or the time is up.
func (d *Daemon) runSurveyRound(r *surveyRound) {
	me := d.identity()
	key := d.gossip.currentKey()
	pkt, err := gossip.SealMessage(key, gossip.SurveyQueryMagic, &gossip.SurveyQuery{V: 1, MAC: me.MAC, Nonce: r.nonce})
	if err != nil {
		return
	}
	s := &d.survey
	deadline := r.started.Add(surveyRoundWait)
	for {
		s.mu.Lock()
		var ask []net.IP
		waiting := 0
		for _, n := range r.nodes {
			if n.state == "asked" {
				waiting++
				if !n.self {
					ask = append(ask, n.addr)
				}
			}
		}
		over := waiting == 0 || time.Now().After(deadline)
		if over {
			for _, n := range r.nodes {
				if n.state == "asked" {
					n.state = "no answer"
				}
			}
			r.done = true
		}
		s.mu.Unlock()
		if over {
			d.log.Info("mesh-wide channel survey finished")
			return
		}
		for _, ip := range ask {
			_ = d.gossip.sendTo(pkt, ip)
		}
		time.Sleep(surveyAskAgain)
	}
}

// handleSurveyQuery surveys this node's links and answers.
func (d *Daemon) handleSurveyQuery(key, pkt []byte, from *net.UDPAddr) error {
	q, err := gossip.OpenSurveyQuery(key, pkt)
	if err != nil {
		return err
	}
	me := d.identity()
	if me == nil || q.MAC == me.MAC || d.gossip == nil {
		return nil
	}
	go func() {
		links := d.surveyLocal(context.Background())
		cur, _ := d.conf()
		reply, err := gossip.SealSurveyReply(key, &gossip.SurveyReply{V: 1, MAC: me.MAC, Callsign: cur.Callsign, Nonce: q.Nonce, Links: links})
		if err != nil {
			d.log.Warn("cannot answer a channel survey", "err", err)
			return
		}
		_ = d.gossip.sendTo(reply, from.IP)
	}()
	return nil
}

// handleSurveyReply keeps a node's answer to this node's round.
func (d *Daemon) handleSurveyReply(key, pkt []byte) error {
	rep, err := gossip.OpenSurveyReply(key, pkt)
	if err != nil {
		return err
	}
	s := &d.survey
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.round
	if r == nil || rep.Nonce != r.nonce {
		return nil
	}
	n := r.nodes[rep.MAC]
	if n == nil {
		n = &surveyNodeState{callsign: rep.Callsign}
		r.nodes[rep.MAC] = n
		r.order = append(r.order, rep.MAC)
	}
	n.state, n.links = "answered", rep.Links
	if rep.Err != "" {
		n.state = "failed: " + rep.Err
	}
	return nil
}

// Survey is the last mesh-wide survey (GET /survey); nil before the first.
func (d *Daemon) Survey() *status.Survey {
	s := &d.survey
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.round
	if r == nil {
		return nil
	}
	out := &status.Survey{Started: onClock(r.started, time.Now()), Done: r.done, Nodes: []status.SurveyNode{}, Suggestions: []status.ChannelSuggestion{}}
	var reports []surveyReport
	for _, mac := range r.order {
		n := r.nodes[mac]
		sn := status.SurveyNode{Callsign: n.callsign, State: n.state}
		for _, l := range n.links {
			sl := status.SurveyLink{Link: l.Link, Band: l.Band, Error: l.Err, Channels: []status.SurveyChannel{}}
			for _, c := range l.Channels {
				sl.Channels = append(sl.Channels, status.SurveyChannel{Ch: c.Ch, Busy: c.Busy, Noise: c.Noise, Nets: c.Nets, Strongest: c.Strongest})
			}
			sn.Links = append(sn.Links, sl)
		}
		out.Nodes = append(out.Nodes, sn)
		if n.state == "answered" {
			reports = append(reports, surveyReport{Callsign: n.callsign, Links: slices.Clone(n.links)})
		}
	}
	if len(reports) > 0 {
		_, profile := d.conf()
		out.Suggestions = suggestChannels(profile, reports)
	}
	return out
}
