package meshd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/status"
)

// Mesh-wide profile changes (later
// channel moves and re-keying build on it). The operator changes the
// profile on one node, which sends it to every node it knows, unicast on the
// gossip port, authenticated under the mesh's gossip key, with the moment
// they all switch to it. Each node keeps the change (on disk, so a restart
// in between does not lose it), answers, and switches at that moment; the
// nodes keep one time (G1). A node that missed it and sees, in gossip,
// another node on a higher revision asks that node for its profile and
// switches at once.
//
// What a change may not do: change the mesh ID or the secret. Both make a
// new gossip key, under which the nodes that switched and those that did
// not can no longer hear each other; that is a re-key, which needs its own
// procedure (M8).

const (
	meshChangeFile     = "mesh-change.json"
	meshChangeDefault  = 60 * time.Second
	meshChangeMin      = 15 * time.Second
	meshChangeMax      = 15 * time.Minute
	meshChangeResend   = 2 * time.Second
	meshChangeLinger   = 30 * time.Second // offers go on this long past the switch
	meshChangeShowFor  = 10 * time.Minute // status shows the last change this long
	meshChangeAskEvery = 10 * time.Second // a node behind asks at most this often
	// meshChangeClockSlack is how much further away a switch may seem after a
	// restart than when the change arrived before the clock counts as gone back.
	meshChangeClockSlack = 30 * time.Second
	// meshChangeCatchUp is how long after an answer a node that was behind
	// switches: the nodes of a part of the mesh that was left behind ask
	// within a few seconds of each other and get the same moment, so they
	// move together rather than one by one, each leaving the rest behind.
	meshChangeCatchUp = 15 * time.Second
)

// meshChange is a profile change this node has, waiting to switch or done.
type meshChange struct {
	Rev     uint64    `json:"rev"`
	ApplyAt time.Time `json:"apply_at"`
	// WaitMS is how far away the switch was when this node took the change:
	// after a restart it can only be nearer, unless the clock went back.
	WaitMS int64     `json:"wait_ms,omitempty"`
	From   string    `json:"from"` // callsign of the node that sent it
	Sender bool      `json:"sender"`
	YAML   string    `json:"yaml"`
	State  string    `json:"state"` // pending, applied, failed
	Error  string    `json:"error,omitempty"`
	Done   time.Time `json:"done,omitempty"`
}

// meshChangeTarget is one node an offer goes to, on the sender.
type meshChangeTarget struct {
	callsign, mac string
	ip            net.IP
	state         string // sent, has it, refused: …
}

type meshChanges struct {
	mu      sync.Mutex
	cur     *meshChange
	targets []*meshChangeTarget
	timer   *time.Timer
	stop    chan struct{} // ends the sender's resend loop
	asked   time.Time
	answer  map[string]time.Time // requests answered, by asker
	// unknown is when each change ID a peer runs and this node does not
	// know was first seen (askForNewerProfile waits askHoldoff after it).
	unknown  map[string]time.Time
	conflict *meshConflict // the last conflict settled (meshconflict.go)
	catchUp  time.Time     // the moment answers to nodes behind give (meshChangeCatchUp)
}

// meshChangeFromPeer reports whether a peer record can follow a mesh-wide
// change (it says so in gossip).
func meshChangeFromPeer(r gossip.Record) bool { return r.MeshChange }

// currentRevision is the highest profile revision this node runs or has
// accepted.
func (d *Daemon) currentRevision() uint64 {
	_, p := d.conf()
	rev := p.Revision
	d.pendingMu.Lock()
	if d.pending != nil && d.pending.profile.Revision > rev {
		rev = d.pending.profile.Revision
	}
	d.pendingMu.Unlock()
	m := &d.meshChanges
	m.mu.Lock()
	if m.cur != nil && m.cur.State == "pending" && m.cur.Rev > rev {
		rev = m.cur.Rev
	}
	m.mu.Unlock()
	return rev
}

// SetMeshProfile sends a profile to every node of the mesh (POST
// /profile/mesh).
func (d *Daemon) SetMeshProfile(ctx context.Context, in api.MeshProfileInput, dryRun bool) (*api.ApplyResult, error) {
	// One at a time: two sent at the same moment would both find no
	// change on its way, and the second replace the first unasked.
	d.meshSendMu.Lock()
	defer d.meshSendMu.Unlock()
	cur, curProfile, setup := d.confWritten()
	if setup.Needed {
		return nil, api.Invalid("this node has no mesh yet: install a profile on it first")
	}
	// A change made in an editor (it says what it was made from) never
	// undoes one already in force that the editor has not seen.
	if in.Base != "" {
		if err := checkBase(in.Base, d.Config().ProfileDigest, "The mesh profile"); err != nil {
			return nil, err
		}
	}
	// Nor does any change undo one on its way, which it would replace
	// without holding what that one changes, unless it says so. A review
	// from a file (no base) is still shown, with what it would replace.
	pending := d.meshChangeState()
	if pending != nil && pending.State != "pending" {
		pending = nil
	}
	if pending != nil && !in.Replace && (in.Base != "" || !dryRun) {
		return nil, api.Conflict("Mesh change from %s pending (switchover at %s): sending now supersedes it. Wait for switchover, or replace explicitly (web UI: Replace; meshctl profile push --replace)", pending.From, pending.ApplyAt.Format("15:04:05"))
	}
	p, err := d.profileFrom(in.ProfileInput, curProfile, false)
	if err != nil {
		return nil, err
	}
	// A profile from a file or a code is the whole profile as it was when
	// the file was made: older than the mesh's, it undoes what changed
	// since.
	given := *p
	if p.MeshID != curProfile.MeshID || p.SAEPassphrase != curProfile.SAEPassphrase {
		return nil, api.Invalid("a mesh-wide change cannot change the mesh ID or the secret: nodes that switched and nodes that did not could no longer hear each other (that is a re-key, not built yet). Change them on each node instead")
	}
	delay := meshChangeDefault
	if in.DelaySeconds != 0 {
		delay = time.Duration(in.DelaySeconds) * time.Second
	}
	if delay < meshChangeMin || delay > meshChangeMax {
		return nil, api.Invalid("the switch can be %v to %v from now", meshChangeMin, meshChangeMax)
	}
	q := *p
	q.Revision, q.Change = curProfile.Revision, curProfile.Change
	res := &api.ApplyResult{Changed: !sameYAML(*curProfile, q), Effects: profileEffects(curProfile, p, false, d.profileView(cur, curProfile, p))}
	res.Effects = meshWideEffects(res.Effects, curProfile, p)
	at := time.Now().Add(delay).Truncate(time.Second)
	res.ApplyAt = &at
	var behind []string
	for _, peer := range d.peers.Peers() {
		r := peer.Record
		if !meshChangeFromPeer(r) {
			behind = append(behind, r.Callsign)
			continue
		}
		res.Nodes = append(res.Nodes, r.Callsign)
	}
	slices.Sort(res.Nodes)
	slices.Sort(behind)
	res.Effects = append(res.Effects, effect("info", "mesh-change", "Switchover at %s (in %s): this node and %s.",
		at.Format("15:04:05"), delay.Round(time.Second), map[bool]string{true: "no other node known yet", false: listAnd(res.Nodes)}[len(res.Nodes) == 0]))
	// The regulatory setting goes to every node with the profile: name the
	// nodes whose setting this changes, so it is never changed unseen.
	want := gossip.RegulatoryEntry(p.Regulatory.Mode, p.Regulatory.Country)
	regChange := map[string][]string{}
	for _, peer := range d.peers.Peers() {
		r := peer.Record
		if meshChangeFromPeer(r) && r.Regulatory != "" && r.Regulatory != want {
			regChange[r.Regulatory] = append(regChange[r.Regulatory], r.Callsign)
		}
	}
	for _, from := range slices.Sorted(maps.Keys(regChange)) {
		names := regChange[from]
		slices.Sort(names)
		res.Effects = append(res.Effects, effect("warning", "mesh-change-regulatory", "Regulatory mode on %s: %s → %s.",
			listAnd(names), regWords(from), regWords(want)))
	}
	if len(behind) > 0 {
		res.Effects = append(res.Effects, effect("warning", "mesh-change-old", "%s: version cannot follow mesh changes. Install the profile manually, or the old one stays.",
			listAnd(behind)))
	}
	// A profile with talk groups or a site link is not one a release
	// before them reads (profiles are read strictly): those nodes refuse
	// the whole change.
	var newer []string
	if len(p.Voice.Groups) > 0 || p.Voice.Bitrate != 0 {
		newer = append(newer, "talk groups")
	}
	if !reflect.ValueOf(p.SiteLink).IsZero() {
		newer = append(newer, "a site link")
	}
	if len(newer) > 0 {
		var old []string
		for _, peer := range d.peers.Peers() {
			if r := peer.Record; meshChangeFromPeer(r) && beforeVoice(r.Version) {
				old = append(old, r.Callsign)
			}
		}
		if len(old) > 0 {
			slices.Sort(old)
			res.Effects = append(res.Effects, effect("warning", "mesh-change-voice-old", "%s: release before 0.3.0, cannot read a profile with %s; change refused there. Upgrade all nodes first.",
				listAnd(old), listAnd(newer)))
		}
	}
	// Nodes heard in the last day but not now (roster.go) will miss it.
	live := map[string]bool{}
	for _, peer := range d.peers.Peers() {
		live[peer.Record.MAC] = true
	}
	var away []string
	for mac, callsign := range d.rosterKnown() {
		if !live[mac] {
			away = append(away, callsign)
		}
	}
	if len(away) > 0 {
		slices.Sort(away)
		res.Effects = append(res.Effects, effect("warning", "mesh-change-away", "%s: heard in the last day, not in range now. Will miss this change and search for the mesh on return. Send with all nodes up and in range.",
			listAnd(away)))
	}
	if c := d.time.Clock(); !c.Synced {
		res.Effects = append(res.Effects, effect("warning", "mesh-change-clock", "Clock not synced (%s). Nodes switch over by local clock; switchover may be staggered.", c.Note))
	}
	// cancels: the profile in force, sent in the place of a change on its
	// way.
	cancels := pending != nil && in.Replace && !res.Changed
	if pending != nil {
		what := ""
		if st := d.meshChangeStatus(); st != nil && st.Summary != "" {
			what = " (" + st.Summary + ")"
		}
		if cancels {
			res.Effects = append(res.Effects, effect("warning", "mesh-change-cancelled", "Cancels the change from %s due at %s%s. All nodes keep the current profile (new revision).", pending.From, pending.ApplyAt.Format("15:04:05"), what))
		} else {
			res.Effects = append(res.Effects, effect("warning", "mesh-change-replaced", "Supersedes the change from %s due at %s%s; its settings not in this profile are dropped.", pending.From, pending.ApplyAt.Format("15:04:05"), what))
		}
	}
	if in.New == nil && res.Changed {
		switch {
		case given.Revision > 0 && given.Revision < curProfile.Revision:
			res.Effects = append(res.Effects, effect("warning", "profile-older", "Profile rev %d; mesh runs rev %d. Sending reverts later mesh changes. Start from the current profile (meshctl profile show) unless intended.", given.Revision, curProfile.Revision))
		case given.Revision == curProfile.Revision && given.ChangeID() != "" && curProfile.ChangeID() != "" && given.ChangeID() != curProfile.ChangeID():
			res.Effects = append(res.Effects, effect("warning", "profile-older", "Profile from a different change than the mesh runs (both rev %d). Sending reverts that change.", given.Revision))
		}
	}
	sortEffects(res.Effects)
	// Said at the review, not found out after "sent": a profile too large
	// for one offer reaches no node.
	if err := profileFits(p); err != nil {
		return nil, err
	}
	if dryRun || (!res.Changed && !cancels) {
		return res, nil
	}

	p.Revision = max(d.currentRevision(), d.highestPeerRevision()) + 1
	p.StartChange(d.latestProfile(), at, cur.Callsign)
	data, err := p.YAML("")
	if err != nil {
		return nil, err
	}
	me := d.identity()
	if me == nil || d.gossip == nil || d.gossip.currentKey() == nil {
		return nil, errors.New("Gossip not started; retry shortly")
	}
	c := &meshChange{Rev: p.Revision, ApplyAt: at, From: cur.Callsign, Sender: true, YAML: string(data), State: "pending"}
	if err := d.acceptMeshChange(c); err != nil {
		return nil, err
	}
	m := &d.meshChanges
	m.mu.Lock()
	m.targets = nil
	for _, peer := range d.peers.Peers() {
		r := peer.Record
		if ip := net.ParseIP(r.Addr); ip != nil && meshChangeFromPeer(r) {
			m.targets = append(m.targets, &meshChangeTarget{callsign: r.Callsign, mac: r.MAC, ip: ip, state: "sent"})
		}
	}
	if m.stop != nil {
		close(m.stop)
	}
	stop := make(chan struct{})
	m.stop = stop
	m.mu.Unlock()
	go d.sendMeshChange(c, stop)
	d.log.Info("mesh-wide profile change sent", "revision", c.Rev, "apply_at", at.Format(time.RFC3339), "nodes", strings.Join(res.Nodes, " "))
	res.Applied, res.Revision = true, c.Rev
	return res, nil
}

// profileFits refuses a profile that does not fit one offer
// (gossip.MaxOfferPacket): nodes pass a profile to each other whole, as a
// mesh-wide change or when one asks for it, so one that does not fit
// stays on the node it was installed on. Measured with the longest change
// record a profile can come to carry, so that a profile that fits today
// still fits after any number of changes.
func profileFits(p *config.Profile) error {
	trial := *p
	earlier := make([]string, config.MaxEarlierChanges)
	for i := range earlier {
		earlier[i] = "ffffffffffffffff"
	}
	trial.Revision = 1 << 40
	trial.Change = &config.ChangeInfo{ID: "ffffffffffffffff", At: time.Now().UTC().Truncate(time.Second), By: strings.Repeat("x", 63), Earlier: earlier}
	data, err := trial.YAML("")
	if err != nil {
		return err
	}
	o := &gossip.Offer{V: 1, MAC: "ff:ff:ff:ff:ff:ff", Callsign: strings.Repeat("x", 63), Rev: trial.Revision,
		ApplyAtMS: time.Now().Add(24 * time.Hour).UnixMilli(), InMS: 24 * 3600 * 1000, Profile: string(data)}
	if _, err := gossip.SealOffer(make([]byte, 32), o); err != nil {
		var es config.Errors
		field := "(document)"
		switch {
		case len(p.SiteLink.Members) > 8:
			field = "site_link.members"
		case len(p.Voice.Groups) > 8:
			field = "voice.groups"
		}
		es = append(es, &config.FieldError{Field: field, Msg: fmt.Sprintf("the profile is too large to pass from node to node: %d bytes as a file, and about %d fit (one message carries a whole profile). Shorten the site link's members (names, endpoints) or the talk groups", len(data), len(data)-overCap(err))})
		return es
	}
	return nil
}

// overCap is by how many bytes a sealed message missed its cap; 0 when
// the error does not say.
func overCap(err error) int {
	var size, limit int
	if _, serr := fmt.Sscanf(err.Error(), "gossip message is %d bytes, over the %d-byte cap", &size, &limit); serr != nil {
		return 0
	}
	return size - limit
}

// highestPeerRevision is the highest revision another node says it runs.
func (d *Daemon) highestPeerRevision() uint64 {
	var rev uint64
	if d.peers == nil {
		return 0
	}
	for _, p := range d.peers.Peers() {
		rev = max(rev, p.Record.ProfileRev)
	}
	return rev
}

// sendMeshChange offers c to every target that has not answered, until all
// have or the switch is well past.
func (d *Daemon) sendMeshChange(c *meshChange, stop chan struct{}) {
	me := d.identity()
	key := d.gossip.currentKey()
	cur, _ := d.conf()
	tick := time.NewTicker(meshChangeResend)
	defer tick.Stop()
	for {
		m := &d.meshChanges
		m.mu.Lock()
		var waiting []*meshChangeTarget
		for _, t := range m.targets {
			if t.state == "sent" {
				waiting = append(waiting, t)
			}
		}
		m.mu.Unlock()
		if len(waiting) == 0 || time.Now().After(c.ApplyAt.Add(meshChangeLinger)) {
			return
		}
		// Sealed afresh each round: the delay to the switch shrinks.
		pkt, err := gossip.SealOffer(key, &gossip.Offer{V: 1, MAC: me.MAC, Callsign: cur.Callsign, Rev: c.Rev, ApplyAtMS: c.ApplyAt.UnixMilli(),
			InMS: max(0, time.Until(c.ApplyAt)).Milliseconds(), Profile: c.YAML})
		if err != nil {
			d.log.Error("cannot send the mesh-wide change", "err", err)
			return
		}
		for _, t := range waiting {
			_ = d.gossip.sendTo(pkt, t.ip)
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

// handleProfileOffer takes another node's offer.
func (d *Daemon) handleProfileOffer(key, pkt []byte, from *net.UDPAddr) error {
	o, err := gossip.OpenOffer(key, pkt)
	if err != nil {
		return err
	}
	me := d.identity()
	if me == nil || o.MAC == me.MAC {
		return nil
	}
	ok, why := true, ""
	p, err := config.ParseProfile([]byte(o.Profile))
	_, curProfile := d.conf()
	switch {
	case err != nil:
		ok, why = false, "the profile is not valid here: "+err.Error()
	case p.Revision != o.Rev:
		ok, why = false, "the offer's revision does not match its profile"
	case p.MeshID != curProfile.MeshID || p.SAEPassphrase != curProfile.SAEPassphrase:
		ok, why = false, "a mesh-wide change cannot change the mesh ID or the secret"
	}
	// The moment: by the delay when the offer gives one (whatever the
	// clocks say), else by the time of day, else now.
	at := time.Now()
	switch {
	case o.InMS > 0:
		at = at.Add(time.Duration(o.InMS) * time.Millisecond)
	case o.ApplyAtMS != 0:
		at = time.UnixMilli(o.ApplyAtMS)
	}
	if ok && at.After(time.Now().Add(meshChangeMax+time.Minute)) {
		ok, why = false, "the switch is too far in the future (clocks apart?)"
	}
	if ok {
		have := d.currentRevision()
		mine := d.latestProfile()
		rel := config.Compare(mine, p)
		switch {
		case rel == config.RelSame:
			// A repeat, or this node has it already: answer again.
		case rel == config.RelAhead:
			ok, why = false, fmt.Sprintf("this node already has a later change (revision %d)", have)
		case rel == config.RelDiverged:
			// Changed apart from this node's profile: settle it.
			if err := d.reconcileProfiles(mine, p, o.Callsign); err != nil {
				ok, why = false, err.Error()
			}
		case o.Rev < have:
			ok, why = false, fmt.Sprintf("this node already has revision %d", have)
		case o.Rev == have:
			// A repeat, or this node has it already: answer again.
		default:
			c := &meshChange{Rev: o.Rev, ApplyAt: at, From: o.Callsign, YAML: o.Profile, State: "pending"}
			if err := d.acceptMeshChange(c); err != nil {
				ok, why = false, err.Error()
			} else {
				d.log.Info("mesh-wide profile change received", "revision", o.Rev, "from", o.Callsign, "apply_at", at.Format(time.RFC3339))
			}
		}
	}
	if d.gossip == nil {
		return nil // not started (tests)
	}
	ack, err := gossip.SealMessage(key, gossip.AckMagic, &gossip.Ack{V: 1, MAC: me.MAC, Rev: o.Rev, OK: ok, Err: gossip.TrimErr(why)})
	if err != nil {
		return err
	}
	return d.gossip.sendTo(ack, from.IP)
}

// handleProfileAck records a node's answer on the sender.
func (d *Daemon) handleProfileAck(key, pkt []byte) error {
	a, err := gossip.OpenAck(key, pkt)
	if err != nil {
		return err
	}
	m := &d.meshChanges
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || !m.cur.Sender || a.Rev != m.cur.Rev {
		return nil
	}
	for _, t := range m.targets {
		if t.mac == a.MAC {
			t.state = "has it"
			if !a.OK {
				t.state = "refused: " + a.Err
			}
		}
	}
	return nil
}

// handleProfileRequest answers a node that asks for the profile of a
// revision this node runs, or is waiting to switch to.
func (d *Daemon) handleProfileRequest(key, pkt []byte, from *net.UDPAddr) error {
	r, err := gossip.OpenRequest(key, pkt)
	if err != nil {
		return err
	}
	me := d.identity()
	if me == nil {
		return nil
	}
	m := &d.meshChanges
	m.mu.Lock()
	if m.answer == nil {
		m.answer = map[string]time.Time{}
	}
	who := from.IP.String()
	recent := time.Since(m.answer[who]) < 5*time.Second
	if !recent {
		m.answer[who] = time.Now()
	}
	pending := m.cur
	m.mu.Unlock()
	if recent {
		return nil
	}
	o := &gossip.Offer{V: 1, MAC: me.MAC, Rev: r.Rev}
	cur, curProfile := d.conf()
	o.Callsign = cur.Callsign
	switch {
	case curProfile.Revision == r.Rev:
		data, err := curProfile.YAML("")
		if err != nil {
			return err
		}
		o.Profile = string(data)
		// Older versions switch at once (no "at"); newer ones at a moment
		// shared by every node that asks around now (meshChangeCatchUp).
		m.mu.Lock()
		if !time.Now().Before(m.catchUp) {
			m.catchUp = time.Now().Add(meshChangeCatchUp)
		}
		o.InMS = max(time.Millisecond, time.Until(m.catchUp)).Milliseconds()
		m.mu.Unlock()
	case pending != nil && pending.Rev == r.Rev && pending.State == "pending":
		o.Profile, o.ApplyAtMS = pending.YAML, pending.ApplyAt.UnixMilli()
		o.InMS = max(0, time.Until(pending.ApplyAt)).Milliseconds()
	default:
		return nil
	}
	pkt, err = gossip.SealOffer(key, o)
	if err != nil {
		return err
	}
	return d.gossip.sendTo(pkt, from.IP)
}

// askForNewerProfile asks another node for its profile when it runs a
// higher revision (this node missed a change), or a mesh-wide change this
// node does not know (it is ahead, or the mesh was changed apart: the
// answer settles it, meshconflict.go). Called on every gossip pass; asks at
// most every meshChangeAskEvery, and about an unknown change only
// askHoldoff after first hearing of it.
func (d *Daemon) askForNewerProfile(key []byte) {
	me := d.identity()
	if me == nil {
		return
	}
	have := d.currentRevision()
	mine := d.latestProfile()
	now := time.Now()
	m := &d.meshChanges
	m.mu.Lock()
	var best *gossip.Record
	heard := map[string]bool{}
	for _, p := range d.peers.Peers() {
		r := p.Record
		if net.ParseIP(r.Addr) == nil {
			continue
		}
		unknown := unknownChange(mine, r)
		if unknown {
			heard[r.ProfileChange] = true
			if m.unknown == nil {
				m.unknown = map[string]time.Time{}
			}
			if _, ok := m.unknown[r.ProfileChange]; !ok {
				m.unknown[r.ProfileChange] = now
			}
			if now.Sub(m.unknown[r.ProfileChange]) < askHoldoff(me.NodeID) {
				continue
			}
		} else if r.ProfileRev <= have {
			continue
		}
		if best == nil || r.ProfileRev > best.ProfileRev {
			rr := r
			best = &rr
		}
	}
	for id := range m.unknown {
		if !heard[id] {
			delete(m.unknown, id)
		}
	}
	due := best != nil && time.Since(m.asked) >= meshChangeAskEvery && d.gossip != nil
	if due {
		m.asked = now
	}
	m.mu.Unlock()
	if !due {
		return
	}
	pkt, err := gossip.SealMessage(key, gossip.RequestMagic, &gossip.Request{V: 1, MAC: me.MAC, Rev: best.ProfileRev})
	if err != nil {
		return
	}
	if best.ProfileRev > have {
		d.log.Info("another node runs a newer mesh profile; asking it", "node", best.Callsign, "revision", best.ProfileRev, "this_node", have)
	} else {
		d.log.Info("another node runs a mesh-wide change this node does not know; asking it", "node", best.Callsign, "revision", best.ProfileRev, "change", best.ProfileChange, "this_node", have)
	}
	_ = d.gossip.sendTo(pkt, net.ParseIP(best.Addr))
}

// acceptMeshChange keeps c, on disk too, and arranges the switch.
func (d *Daemon) acceptMeshChange(c *meshChange) error {
	if c.WaitMS == 0 {
		c.WaitMS = max(0, time.Until(c.ApplyAt)).Milliseconds()
	}
	// Kept on disk and made current in one step, so that an older change
	// that is being installed meanwhile cannot remove this one's file
	// (applyMeshChange).
	m := &d.meshChanges
	m.mu.Lock()
	if err := d.saveMeshChange(c); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("cannot keep the change: %w", err)
	}
	m.cur = c
	if m.timer != nil {
		m.timer.Stop()
	}
	m.timer = time.AfterFunc(max(0, time.Until(c.ApplyAt)), func() { d.applyMeshChange(c.Rev) })
	m.mu.Unlock()
	return nil
}

// applyMeshChange installs the change with revision rev, if it is still the
// one waiting.
func (d *Daemon) applyMeshChange(rev uint64) {
	m := &d.meshChanges
	m.mu.Lock()
	c := m.cur
	m.mu.Unlock()
	if c == nil || c.Rev != rev || c.State != "pending" {
		return
	}
	err := d.installMeshProfile(c)
	m.mu.Lock()
	if m.cur == c {
		c.State, c.Done = "applied", time.Now()
		if err != nil {
			c.State, c.Error = "failed", err.Error()
		}
		// The file is this change's only while it is the current one: a
		// newer change accepted meanwhile has put its own there.
		_ = os.Remove(filepath.Join(d.opts.StateDir, meshChangeFile))
	}
	m.mu.Unlock()
	if err != nil {
		d.log.Error("mesh-wide profile change failed", "revision", rev, "err", err)
	} else {
		d.log.Info("mesh-wide profile change in force", "revision", rev, "from", c.From)
	}
	d.Kick("mesh-wide profile change")
}

// installMeshProfile writes the change's profile and reloads, as a
// profile change through the API does.
func (d *Daemon) installMeshProfile(c *meshChange) error {
	p, err := config.ParseProfile([]byte(c.YAML))
	if err != nil {
		return err
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	cur, curProfile := d.conf()
	if curProfile.Revision >= p.Revision {
		return nil // in force already
	}
	data, err := p.YAML(fmt.Sprintf(profileFileHeader, stamp()) + fmt.Sprintf("\nRevision %d, a mesh-wide change from %s.", p.Revision, c.From))
	if err != nil {
		return err
	}
	return d.commit(&fileChange{path: d.profilePath(cur), data: data, mode: 0o600})
}

func (d *Daemon) saveMeshChange(c *meshChange) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// The profile in it holds the mesh secret.
	return writeFileAtomic(filepath.Join(d.opts.StateDir, meshChangeFile), data, 0o600)
}

// loadMeshChange picks up a change accepted before a restart.
func (d *Daemon) loadMeshChange() {
	data, err := os.ReadFile(filepath.Join(d.opts.StateDir, meshChangeFile))
	if err != nil {
		return
	}
	var c meshChange
	if json.Unmarshal(data, &c) != nil || c.State != "pending" || c.Rev <= d.currentRevision() {
		_ = os.Remove(filepath.Join(d.opts.StateDir, meshChangeFile))
		return
	}
	// A node without a clock of its own (no RTC) starts from a saved,
	// stale time and only gets the right one from the mesh. Waiting for the
	// moment by that clock leaves it on the old settings while every other
	// node has switched, where it cannot reach the mesh to correct its clock.
	// It switches now: the moment is near, or past.
	//
	// A clock can also be wrong and still count as synchronised (a stale
	// source, an RTC that drifted). The switch can only have come nearer
	// since the change arrived; one that seems further away than it was
	// then (than the longest delay a sender may set, for a change kept by
	// an older version) means the clock went back, and waiting for it would
	// strand the node the same way. Then too it switches now: a few nodes
	// that took the change and restarted together go to the new settings
	// together, rather than staying on the old ones as a group that hears
	// itself and never looks for the mesh.
	most := meshChangeMax
	if c.WaitMS > 0 {
		most = time.Duration(c.WaitMS) * time.Millisecond
	}
	switch left := time.Until(c.ApplyAt); {
	case !clockSynced():
		d.log.Info("a mesh-wide profile change from before the restart is due; the clock is not synchronised yet, so it is made now", "revision", c.Rev, "apply_at", c.ApplyAt.Format(time.RFC3339))
		c.ApplyAt = time.Now()
	case left > most+meshChangeClockSlack:
		d.log.Warn("a mesh-wide profile change from before the restart seems further away than when it arrived: the clock went back, so it is made now",
			"revision", c.Rev, "apply_at", c.ApplyAt.Format(time.RFC3339), "seems_in", left.Round(time.Second), "was_in", most.Round(time.Second))
		c.ApplyAt = time.Now()
	default:
		d.log.Info("a mesh-wide profile change from before the restart is still due", "revision", c.Rev, "apply_at", c.ApplyAt.Format(time.RFC3339))
	}
	_ = d.acceptMeshChange(&c)
}

// clockSynced reports whether the kernel holds the system clock
// synchronised (chrony marks it once it has a source; timedatectl shows the
// same). A variable for tests.
var clockSynced = func() bool {
	var tx unix.Timex
	state, err := unix.Adjtimex(&tx)
	return err == nil && state != unix.TIME_ERROR && tx.Status&unix.STA_UNSYNC == 0
}

func (d *Daemon) meshChangeState() *meshChange {
	m := &d.meshChanges
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return nil
	}
	c := *m.cur
	return &c
}

// meshChangeStatus is the change for status: pending, or done in the last
// meshChangeShowFor.
func (d *Daemon) meshChangeStatus() *status.MeshChange {
	m := &d.meshChanges
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.cur
	if c == nil || (c.State != "pending" && time.Since(c.Done) > meshChangeShowFor) {
		return nil
	}
	st := &status.MeshChange{Revision: c.Rev, ApplyAt: c.ApplyAt, From: c.From, Sender: c.Sender, State: c.State, Error: c.Error}
	if p, err := config.ParseProfile([]byte(c.YAML)); err == nil {
		var links []string
		for _, l := range p.AllLinks() {
			links = append(links, fmt.Sprintf("%s at %d MHz", l.Name(), l.Width))
		}
		if n := len(p.SiteLink.Members); n > 0 {
			links = append(links, fmt.Sprintf("site link with %d member%s", n, map[bool]string{true: "", false: "s"}[n == 1]))
		}
		st.Summary = strings.Join(links, ", ")
	}
	if c.Sender {
		for _, t := range m.targets {
			st.Nodes = append(st.Nodes, status.MeshChangeNode{Callsign: t.callsign, State: t.state})
		}
	}
	return st
}

// regWords says a gossip regulatory value in words: "unrestricted",
// "country mode (US)".
func regWords(v string) string {
	if mode, country, ok := strings.Cut(v, ":"); ok {
		return fmt.Sprintf("%s mode (%s)", mode, country)
	}
	return v
}

// oneNodeOnly are the effects of installing a profile on one node that do
// not hold when every node switches at once: its links to the others drop
// only because the others keep the old profile.
var oneNodeOnly = []string{"this-node-only", "leaves-mesh", "links-drop", "link-restarts"}

// meshWideEffects turns the effects of installing p on this node into those
// of a change every node makes at the same moment: links whose settings
// change restart everywhere at the switch, for a few seconds, and nothing
// is cut off by the others keeping the old profile.
func meshWideEffects(es []status.Warning, cur, p *config.Profile) []status.Warning {
	es = slices.DeleteFunc(es, func(w status.Warning) bool { return slices.Contains(oneNodeOnly, w.Code) })
	var restart []string
	for _, l := range cur.AllLinks() {
		if !slices.Contains(p.AllLinks(), l) {
			restart = append(restart, linkName(l))
		}
	}
	if len(restart) > 0 {
		es = append(es, effect("warning", "mesh-change-restart", "At switchover all nodes restart %s: down a few seconds, longer on DFS channels (CAC). Traffic uses other links meanwhile.",
			map[bool]string{true: "the " + restart[0] + " link", false: "the " + listAnd(restart) + " links"}[len(restart) == 1]))
	}
	return es
}
