package meshd

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/sysctl"
)

// Gossip timing (architecture §12.5): on change, a slow refresh, and an
// early reply when a node is heard for the first time so newcomers learn
// the mesh in seconds rather than a refresh period.
const (
	gossipRefresh      = 30 * time.Second
	gossipJitter       = 5 * time.Second
	gossipWelcomeMax   = time.Second     // random delay before answering a new node
	gossipWelcomeEvery = 3 * time.Second // at most one early reply per this period
)

// gossipReconciler owns the gossip socket on br-mesh and this node's
// published record. Received records go into d.peers.
type gossipReconciler struct {
	d *Daemon

	mu       sync.Mutex
	conn     *gossip.Conn
	key      []byte
	keyFor   string
	boot     uint64
	seq      uint64
	record   *gossip.Record // last record sent
	lastSend time.Time
	welcome  time.Time // last early reply
	stop     chan struct{}
	sendNow  chan struct{}
	stats    gossipStats
}

type gossipStats struct {
	Sent, Received, AuthFailed, Ignored uint64
	BytesSent, BytesReceived            uint64
	LastRecordBytes                     int
	LastError                           string
}

func newGossipReconciler(d *Daemon) *gossipReconciler {
	var b [8]byte
	_, _ = crand.Read(b[:])
	boot := binary.BigEndian.Uint64(b[:])
	if boot == 0 {
		boot = 1
	}
	return &gossipReconciler{d: d, boot: boot, sendNow: make(chan struct{}, 1)}
}

func (g *gossipReconciler) Name() string { return "gossip" }

func (g *gossipReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := g.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var bridgeUp bool
	var batIndex int
	var gw *gossip.GW
	d.facts.Read(func(f *Facts) {
		bridgeUp, batIndex = f.BridgePresent && f.BridgeAddress != "", f.BatIfindex
		gw = f.GossipGW
	})
	if !bridgeUp {
		g.close()
		return res, nil
	}

	// Key follows the profile (a passphrase change re-keys gossip).
	keyID := d.profile.SAEPassphrase + "\x00" + d.profile.MeshID
	if g.key == nil || g.keyFor != keyID {
		k, err := gossip.Key(d.profile.SAEPassphrase, d.profile.MeshID)
		if err != nil {
			return res, err
		}
		g.mu.Lock()
		g.key, g.keyFor = k, keyID
		g.mu.Unlock()
	}

	// Two nodes with the same node ID share an address, and Linux drops a
	// packet whose source is one of its own addresses as martian. Gossip is
	// how such a conflict is found and settled (architecture §7.3), so it
	// must get through: accept local sources on br-mesh.
	if ch, err := sysctl.Set("net.ipv4.conf."+BridgeIfname+".accept_local", "1"); err != nil {
		warn("warning", "gossip-accept-local", "cannot set accept_local on %s: %v; a node ID conflict may go unnoticed", BridgeIfname, err)
	} else if ch {
		d.log.Info("sysctl", "key", "net.ipv4.conf."+BridgeIfname+".accept_local", "value", "1")
		res.Changed = true
	}

	if g.conn == nil {
		c, err := gossip.Listen(BridgeIfname, "", 0)
		if err != nil {
			warn("error", "gossip-socket", "cannot open the gossip socket on %s: %v", BridgeIfname, err)
			res.RequeueAfter = 10 * time.Second
			return res, nil
		}
		g.mu.Lock()
		g.conn = c
		g.stop = make(chan struct{})
		g.mu.Unlock()
		go g.receive(c, g.stop)
		go g.sender(g.stop)
		d.log.Info("gossip started", "interface", BridgeIfname, "group", gossip.Group, "port", gossip.Port)
		res.Changed = true
	}

	// Node ID and identity conflicts (architecture §7.3), settled before
	// this node's record goes out.
	// The record lists the radios' and wired ports' addresses. Site link
	// tunnels are left out: any node works them out from the profile
	// (siteTunnelMACs), and a member with many would not fit a record.
	orig := meshIfMACs(batIndex)
	if n := d.peers.Expire(); n > 0 {
		d.log.Info("nodes left the gossip view", "count", n)
		res.Changed = true
	}
	g.resolveConflicts(orig, warn, &res)
	me := d.identity()

	// This node's record. The address follows the node ID even in the
	// pass where the ID changed and the bridge has not caught up yet.
	addr, _, _ := strings.Cut(d.facts.bridgeAddress(), "/")
	if a, err := nodeAddress(d.profile.Prefix, me.NodeID); err == nil {
		addr, _, _ = strings.Cut(a, "/")
	}
	r := &gossip.Record{
		V: 1, MAC: me.MAC, ID: me.NodeID, IDSource: me.NodeIDSource, Callsign: d.cfg.Callsign,
		Addr: addr, Version: d.opts.Version, Boot: g.boot, GW: gw, Orig: orig, Radios: d.meshRadioEntries(),
		Links: profileLinkEntries(d.profile), ProfileRev: d.profile.Revision, ProfileChange: d.profile.ChangeID(), MeshChange: true,
		Regulatory: gossip.RegulatoryEntry(d.profile.Regulatory.Mode, d.profile.Regulatory.Country),
		Load:       d.gossipLoads(d.profile.AllLinks()),
		Degraded:   d.gossipDegraded(),
		SiteKey:    d.sitePublicKey(),
		Site:       d.siteLinkGossip(),
	}
	r.Pos, r.NoPos = d.gossipPosition(time.Now())
	r.Arch, r.Rollouts, _ = d.rolloutSelfCheck(ctx)
	r.HaLowFiles = d.halowHeld()
	r.Rollout = d.rolloutGossip()
	if d.time != nil {
		r.Time = d.time.Role()
	}
	g.mu.Lock()
	changed := !gossip.Same(g.record, r)
	if !changed && g.record != nil {
		// Only live figures moved (channel load, the WAN's RTT and loss):
		// not sent now, but the refresh sends this record, not the old one.
		g.record = r
	}
	g.mu.Unlock()
	if changed {
		if err := g.send(r); err != nil {
			warn("warning", "gossip-send", "cannot send gossip: %v", err)
			res.RequeueAfter = 5 * time.Second
		} else {
			res.Changed = true
		}
	}

	// A node ahead on the profile's revision: this node missed a
	// mesh-wide change, and asks for it (meshchange.go).
	if key := g.currentKey(); key != nil {
		d.askForNewerProfile(key)
	}
	d.noteRoster(d.peers.Peers(), false)

	// Nodes that run the mesh's links differently: same secret, different
	// profile. A link whose ends differ in width can run at a fraction of
	// its speed (an MT7916 sent at its lowest rate to a 40 MHz peer of its
	// 80 MHz link); one on another channel does not form at all.
	for _, p := range d.peers.Peers() {
		if diffs := linkDifferences(r.Links, p.Record.Links); len(diffs) > 0 {
			warn("warning", "profile-differs", "Profile mismatch with %s: %s. Install the same profile on all nodes",
				p.Record.Callsign, strings.Join(diffs, "; "))
		}
	}

	// Nodes on another release: every node of a mesh should run the same
	// release series, so that all of them understand each other's records,
	// profiles and mesh-wide changes.
	others := map[string]string{}
	for _, p := range d.peers.Peers() {
		others[p.Record.Callsign] = p.Record.Version
	}
	// While a mesh-wide upgrade is on, its progress says the same.
	if level, code, msg := releaseDifferences(d.opts.Version, others); code != "" && !d.rolloutActive() {
		warn(level, code, "%s", msg)
	}

	g.mu.Lock()
	if g.stats.AuthFailed > 0 && g.stats.Received == 0 {
		warn("warning", "gossip-auth", "Gossip auth failed on %d packets, none passed: possible mesh profile mismatch", g.stats.AuthFailed)
	}
	g.mu.Unlock()
	return res, nil
}

// resolveConflicts gives up this node's ID (or its cloned identity MAC)
// when the gossip view shows another node keeps it, and reports the
// conflicts this node wins or cannot settle.
func (g *gossipReconciler) resolveConflicts(orig []string, warn func(level, code, format string, args ...any), res *reconcile.Result) {
	d := g.d
	me := d.identity()
	peers := d.peers.Peers()
	recs := make([]gossip.Record, len(peers))
	for i, p := range peers {
		recs[i] = p.Record
	}
	var clone *gossip.Record
	if c, ok := d.peers.Clone(); ok {
		clone = &c
	}
	dec, err := resolveIdentity(me, orig, g.boot, recs, clone)
	switch {
	case err != nil:
		warn("error", "node-id-exhausted", "%v", err)
	case dec.Next != nil:
		if err := d.setIdentityFromLoop(dec.Next); err != nil {
			warn("error", "node-id-conflict", "cannot take a new identity: %v", err)
			break
		}
		d.log.Warn("identity conflict: re-addressing", "reason", dec.Reason, "node_id", dec.Next.NodeID, "mac", dec.Next.MAC)
		res.Changed = true
		me = dec.Next
	}

	// What remains is shown, not acted on.
	if clone != nil && strings.EqualFold(clone.MAC, me.MAC) {
		warn("warning", "identity-clone", "Node %q (%s) uses this node's identity MAC %s (cloned identity file); it will take a new one", clone.Callsign, clone.Addr, me.MAC)
	}
	for _, p := range recs {
		if p.ID == me.NodeID {
			warn("warning", "node-id-conflict", "Node ID conflict: %q (%s) also uses ID %d; this node keeps it, %q moves", p.Callsign, p.MAC, p.ID, p.Callsign)
		}
	}
	if pin := d.cfg.NodeID; pin != 0 && pin != me.NodeID {
		holder := "another node"
		for _, p := range recs {
			if p.ID == pin {
				holder = fmt.Sprintf("%q (%s)", p.Callsign, p.MAC)
			}
		}
		warn("error", "node-id-taken", "node_id %d (node.yaml) held by %s; using %d. Assign a different node_id to one of them", pin, holder, me.NodeID)
	}
}

// send seals and multicasts r as the next record.
func (g *gossipReconciler) send(r *gossip.Record) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn == nil || g.key == nil {
		return errors.New("gossip not started")
	}
	g.seq++
	r.Seq = g.seq
	pkt, err := gossip.Seal(g.key, r)
	if err != nil {
		return err
	}
	if err := g.conn.Send(pkt); err != nil {
		g.stats.LastError = err.Error()
		return err
	}
	g.record, g.lastSend = r, time.Now()
	g.stats.Sent++
	g.stats.BytesSent += uint64(len(pkt))
	g.stats.LastRecordBytes = len(pkt)
	return nil
}

// announceAt: when a node that has just found its first neighbour sends
// its record. At once, and twice more: batman-adv sends a multicast
// datagram only to the listeners it knows, and a new node learns them
// from the others' translation tables within a few seconds.
var announceAt = []time.Duration{0, 4 * time.Second, 10 * time.Second}

// announce sends this node's record soon (each within gossipWelcomeMax),
// as the early reply to a new node does. Called when bat0 first has a
// neighbour: records sent before reached nobody, and until the others hear
// one, a node ID this node shares with another (a derived ID's collision,
// a cloned card) stands unresolved, as long as a refresh period.
func (g *gossipReconciler) announce() {
	for _, after := range announceAt {
		time.AfterFunc(after, func() {
			select {
			case g.sendNow <- struct{}{}:
			default:
			}
		})
	}
}

// sender refreshes the record periodically and answers new nodes early.
func (g *gossipReconciler) sender(stop chan struct{}) {
	for {
		wait := gossipRefresh + time.Duration(rand.Int64N(int64(2*gossipJitter))) - gossipJitter
		select {
		case <-stop:
			return
		case <-g.sendNow:
			time.Sleep(time.Duration(rand.Int64N(int64(gossipWelcomeMax))))
		case <-time.After(wait):
		}
		g.mu.Lock()
		r := g.record
		g.mu.Unlock()
		if r != nil {
			cp := *r
			_ = g.send(&cp)
		}
	}
}

func (g *gossipReconciler) receive(c *gossip.Conn, stop chan struct{}) {
	buf := make([]byte, gossip.MaxOfferPacket+64)
	for {
		n, from, err := c.Receive(buf)
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		g.mu.Lock()
		key := g.key
		g.mu.Unlock()
		// Link queries and their answers (topology.go), and mesh-wide
		// profile changes (meshchange.go), share the socket.
		switch kind := gossip.Kind(buf[:n]); kind {
		case gossip.QueryMagic, gossip.LinksMagic, gossip.OfferMagic, gossip.AckMagic, gossip.RequestMagic, gossip.SurveyQueryMagic, gossip.SurveyReplyMagic,
			gossip.RolloutMagic, gossip.RolloutAskMagic:
			var err error
			switch kind {
			case gossip.QueryMagic:
				err = g.d.handleLinkQuery(key, buf[:n], from)
			case gossip.LinksMagic:
				err = g.d.handleLinkReply(key, buf[:n])
			case gossip.OfferMagic:
				err = g.d.handleProfileOffer(key, buf[:n], from)
			case gossip.AckMagic:
				err = g.d.handleProfileAck(key, buf[:n])
			case gossip.RequestMagic:
				err = g.d.handleProfileRequest(key, buf[:n], from)
			case gossip.SurveyQueryMagic:
				err = g.d.handleSurveyQuery(key, buf[:n], from)
			case gossip.SurveyReplyMagic:
				err = g.d.handleSurveyReply(key, buf[:n])
			case gossip.RolloutMagic:
				err = g.d.handleRollout(key, buf[:n])
			case gossip.RolloutAskMagic:
				err = g.d.handleRolloutAsk(key, buf[:n], from)
			}
			if err != nil {
				g.mu.Lock()
				if errors.Is(err, gossip.ErrAuth) {
					g.stats.AuthFailed++
				} else {
					g.stats.Ignored++
				}
				g.mu.Unlock()
			}
			continue
		}
		r, err := gossip.Open(key, buf[:n])
		if err != nil {
			g.mu.Lock()
			if errors.Is(err, gossip.ErrAuth) {
				g.stats.AuthFailed++
			} else {
				g.stats.Ignored++
			}
			g.mu.Unlock()
			if errors.Is(err, gossip.ErrInvalid) {
				g.d.log.Warn("ignored a gossip record", "from", from.IP.String(), "err", err)
			}
			continue
		}
		g.mu.Lock()
		g.stats.Received++
		g.stats.BytesReceived += uint64(n)
		g.mu.Unlock()
		switch g.d.peers.Update(r, from.IP.String()) {
		case gossip.New:
			g.d.log.Info("node joined the gossip view", "callsign", r.Callsign, "node_id", r.ID, "address", r.Addr, "mac", r.MAC)
			g.mu.Lock()
			early := time.Since(g.welcome) > gossipWelcomeEvery
			if early {
				g.welcome = time.Now()
			}
			g.mu.Unlock()
			if early {
				select {
				case g.sendNow <- struct{}{}:
				default:
				}
			}
			g.d.Kick("gossip: new node")
		case gossip.Changed:
			g.d.Kick("gossip: node changed")
		case gossip.Clone:
			g.d.log.Warn("another node uses this node's identity MAC", "callsign", r.Callsign, "address", r.Addr, "from", from.IP.String())
			g.d.Kick("gossip: identity clone")
		}
	}
}

// sendTo sends one packet to a single node on the gossip port.
func (g *gossipReconciler) sendTo(pkt []byte, ip net.IP) error {
	g.mu.Lock()
	c := g.conn
	g.mu.Unlock()
	if c == nil {
		return errors.New("gossip not started")
	}
	return c.SendTo(pkt, ip)
}

// currentKey returns the gossip key in force, or nil before gossip starts.
func (g *gossipReconciler) currentKey() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn == nil {
		return nil
	}
	return g.key
}

func (g *gossipReconciler) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn != nil {
		close(g.stop)
		g.conn.Close()
		g.conn = nil
		g.record = nil
	}
}

func (g *gossipReconciler) Teardown(ctx context.Context) error {
	g.close()
	return nil
}

// Stats returns the counters for status.
func (g *gossipReconciler) Stats() gossipStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// meshIfMACs is hardIfMACs without the site link's tunnels.
func meshIfMACs(batIndex int) []string { return hardIfMACsFiltered(batIndex, false) }

// siteTunnelMACs are the addresses of the site link tunnels the node of
// record r holds, worked out from the profile in force; none when it is
// not a member.
func (d *Daemon) siteTunnelMACs(r gossip.Record) []string {
	if r.SiteKey == "" {
		return nil
	}
	_, profile := d.conf()
	if profile == nil || len(profile.SiteLink.Members) == 0 {
		return nil
	}
	return profile.SiteLink.TunnelMACs(r.SiteKey)
}

// originatorsOf is every batman-adv address of the node of record r: the
// ones its record lists and its site link tunnels.
func (d *Daemon) originatorsOf(r gossip.Record) []string {
	extra := d.siteTunnelMACs(r)
	if len(extra) == 0 {
		return r.Orig
	}
	return append(append([]string(nil), r.Orig...), extra...)
}

// hardIfMACs lists the MACs of bat0's hard interfaces: the originator
// addresses other nodes see in batman-adv.
func hardIfMACs(batIndex int) []string { return hardIfMACsFiltered(batIndex, true) }

func hardIfMACsFiltered(batIndex int, tunnels bool) []string {
	if batIndex == 0 {
		return nil
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range links {
		if !tunnels && strings.HasPrefix(l.Attrs().Name, siteIfPrefix) {
			continue
		}
		if l.Attrs().MasterIndex == batIndex && l.Attrs().HardwareAddr != nil {
			out = append(out, l.Attrs().HardwareAddr.String())
		}
	}
	return out
}

// meshNodes renders the gossip view for status.
func (d *Daemon) meshNodes() []status.MeshNode {
	out := []status.MeshNode{}
	if d.peers == nil {
		return out
	}
	now := time.Now()
	self := d.identity()
	for _, p := range d.peers.Peers() {
		r := p.Record
		n := status.MeshNode{
			Callsign: r.Callsign, NodeID: r.ID, Address: r.Addr, MAC: r.MAC, Originators: d.originatorsOf(r), Version: r.Version,
			HeardAgoMS: now.Sub(p.LastHeard).Milliseconds(), IDConflict: self != nil && r.ID == self.NodeID,
		}
		if r.GW != nil {
			n.Gateway, n.WANHealthy = true, r.GW.Healthy
		}
		for _, e := range r.Radios {
			n.MeshRadios = append(n.MeshRadios, gossip.RadioBands(e))
		}
		n.ChannelBusy = r.Load
		for _, g := range r.Degraded {
			n.Degraded = append(n.Degraded, status.NodeDegraded{Bands: gossip.RadioBands(g.Radio), Reason: g.Why})
		}
		if r.Pos != nil {
			p := positionStatus(*r.Pos)
			n.Position = &p
		}
		n.PositionOff = r.NoPos
		n.SiteKey = r.SiteKey
		if s := r.Site; s != nil {
			n.SiteLink = &status.NodeSiteLink{Site: s.Site, Up: s.Up, Peers: s.Peers}
		}
		out = append(out, n)
	}
	return out
}

// meshRadioEntries lists this node's radios that can carry a mesh link, for
// its gossip record: one entry of bands per radio, in a stable order. A
// radio the system uses, an ignored one or a broken one is left out, as the
// planner leaves it out.
func (d *Daemon) meshRadioEntries() []string {
	var out []string
	d.facts.Read(func(f *Facts) {
		for _, r := range f.Radios {
			if r.Ignored || r.Broken != "" || r.HardBlocked || r.InUse != "" || !r.Caps.MeshPoint || len(r.Caps.Bands) == 0 {
				continue
			}
			out = append(out, gossip.RadioEntry(r.Caps.Bands))
		}
	})
	slices.Sort(out)
	if len(out) > gossip.MaxRadios {
		out = out[:gossip.MaxRadios]
	}
	return out
}

// profileLinkEntries lists a profile's links for this node's gossip record.
func profileLinkEntries(p *config.Profile) []string {
	var out []string
	for _, l := range p.AllLinks() {
		out = append(out, gossip.LinkEntry(l.Band, l.Channel, l.Width))
	}
	if len(out) > gossip.MaxLinkEntries {
		out = out[:gossip.MaxLinkEntries]
	}
	return out
}

// linkDifferences says how another node's links (from its gossip record)
// differ from this node's, link by link: a different width or channel on a
// band both use, or a link one profile has and the other lacks. Empty when
// they agree, or when the other node does not say (an older version). The
// order of the links is not compared.
func linkDifferences(mine, theirs []string) []string {
	if len(theirs) == 0 {
		return nil
	}
	type link struct {
		band         string
		channel, mhz int
	}
	parse := func(es []string) []link {
		var out []link
		for _, e := range es {
			if b, c, w, ok := gossip.ParseLinkEntry(e); ok {
				out = append(out, link{b, c, w})
			}
		}
		return out
	}
	a, b := parse(mine), parse(theirs)
	name := func(l link) string { return config.RadioPlan{Band: l.band, Channel: l.channel}.Name() }
	var out []string
	matched := map[int]bool{}
	for _, m := range a {
		found := -1
		for j, t := range b {
			if t.band == m.band && t.channel == m.channel {
				found = j
				break
			}
		}
		if found >= 0 {
			matched[found] = true
			if t := b[found]; t.mhz != m.mhz {
				out = append(out, fmt.Sprintf("it runs the %s link at %d MHz, this node at %d MHz", name(m), t.mhz, m.mhz))
			}
			continue
		}
		other := -1
		for j, t := range b {
			if t.band == m.band && !matched[j] {
				other = j
				break
			}
		}
		if other >= 0 {
			matched[other] = true
			out = append(out, fmt.Sprintf("it has its %s link on channel %d, this node on channel %d", config.BandName(m.band), b[other].channel, m.channel))
			continue
		}
		out = append(out, fmt.Sprintf("its profile has no %s link", name(m)))
	}
	for j, t := range b {
		if !matched[j] {
			out = append(out, fmt.Sprintf("its profile has a %s link this node's lacks", name(t)))
		}
	}
	return out
}
