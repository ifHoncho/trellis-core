package meshd

import (
	"cmp"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/status"
)

// Topology (architecture §12.5). Each node knows only its own links.
// When something asks this node for the whole graph (the web UI's
// topology page), it sends every node a link query on the gossip port and
// assembles the answers. Nothing is sent while nobody asks, and the
// answers are kept for a while, so many viewers cost the same as one.

const (
	topoMinInterval = 5 * time.Second
	topoPerNode     = 500 * time.Millisecond // interval grows by this per node
	topoWait        = 700 * time.Millisecond // how long a round waits for answers
	topoReplyEvery  = time.Second            // answer one asker at most this often
)

type linkReport struct {
	links *gossip.Links
	at    time.Time
}

type topoCollector struct {
	askMu sync.Mutex // one round at a time

	mu        sync.Mutex
	reports   map[string]linkReport // by the reporting node's identity MAC
	nonce     uint64
	lastAsk   time.Time
	waiting   int           // answers still expected in this round
	done      chan struct{} // closed when the round has every answer
	lastReply map[string]time.Time
}

// topoInterval is how often this node asks, at most: slower in a bigger
// mesh, because every round costs airtime on every link.
func topoInterval(peers int) time.Duration {
	return max(topoMinInterval, time.Duration(peers)*topoPerNode)
}

// localLinks reports this node's direct links, the strongest first.
func (d *Daemon) localLinks() (links []gossip.Link, more int) {
	var batIfindex int
	// The mesh radios' interfaces, and the mesh link (from 1) each carries.
	wifi := map[int]int{}
	d.facts.Read(func(f *Facts) {
		batIfindex = f.BatIfindex
		// mesh0 is a radio's interface whenever it exists (wired mesh
		// ports are hard interfaces of their own).
		if f.MeshIfindex != 0 {
			wifi[f.MeshIfindex] = 1
		}
		for i, l := range f.MoreLinks {
			if l.Ifindex != 0 {
				wifi[l.Ifindex] = i + 2
			}
		}
	})
	if d.bat == nil || batIfindex == 0 {
		return nil, 0
	}
	local := map[int]string{}
	if hifs, err := d.bat.HardIfs(batIfindex); err == nil {
		for _, h := range hifs {
			local[h.Ifindex] = h.Address.String()
		}
	}
	signal := map[string]int{}
	if d.nl != nil {
		for ifindex := range wifi {
			if stations, err := d.nl.Stations(ifindex); err == nil {
				for _, s := range stations {
					signal[s.MAC.String()] = int(s.SignalDBm)
				}
			}
		}
	}
	ns, err := d.bat.Neighbors(batIfindex)
	if err != nil {
		return nil, 0
	}
	for _, n := range ns {
		k := wifi[n.HardIfindex]
		l := gossip.Link{Local: local[n.HardIfindex], Remote: n.Address.String(), Kbps: uint32(n.Throughput.Kbps()), Wired: k == 0, MeshLink: k}
		if l.Local == "" {
			continue
		}
		if s := signal[l.Remote]; s < 0 && !l.Wired {
			l.Signal = max(s, -150)
		}
		links = append(links, l)
	}
	slices.SortStableFunc(links, func(a, b gossip.Link) int { return cmp.Compare(b.Kbps, a.Kbps) })
	if len(links) > gossip.MaxLinks {
		more = len(links) - gossip.MaxLinks
		links = links[:gossip.MaxLinks]
	}
	return links, more
}

// localClients counts the end-user devices on this node: stations on its
// AP, plus devices on its wired ports that hold a lease.
func (d *Daemon) localClients() int {
	var apIfindex int
	d.facts.Read(func(f *Facts) { apIfindex = f.APIfindex })
	onAP := map[string]bool{}
	if d.nl != nil && apIfindex != 0 {
		if stations, err := d.nl.Stations(apIfindex); err == nil {
			for _, s := range stations {
				onAP[strings.ToLower(s.MAC.String())] = true
			}
		}
	}
	n := len(onAP)
	if d.dhcp != nil {
		now := time.Now()
		for _, l := range d.dhcp.Leases() {
			if !onAP[strings.ToLower(l.MAC)] && l.Expires.After(now) {
				n++
			}
		}
	}
	return n
}

func (d *Daemon) selectedGateway() int {
	var id int
	d.facts.Read(func(f *Facts) {
		if f.Gateway.Selected != nil {
			id = f.Gateway.Selected.NodeID
		}
	})
	return id
}

// ownLinks is this node's own report.
func (d *Daemon) ownLinks(nonce uint64) *gossip.Links {
	me := d.identity()
	links, more := d.localLinks()
	return &gossip.Links{V: 1, MAC: me.MAC, Nonce: nonce, Links: links, More: more, Clients: d.localClients(), Selected: d.selectedGateway()}
}

// handleLinkQuery answers another node's link query.
func (d *Daemon) handleLinkQuery(key, pkt []byte, from *net.UDPAddr) error {
	q, err := gossip.OpenQuery(key, pkt)
	if err != nil {
		return err
	}
	t := &d.topo
	t.mu.Lock()
	if t.lastReply == nil {
		t.lastReply = map[string]time.Time{}
	}
	who := from.IP.String()
	if time.Since(t.lastReply[who]) < topoReplyEvery {
		t.mu.Unlock()
		return nil
	}
	t.lastReply[who] = time.Now()
	for ip, at := range t.lastReply { // forget askers that went quiet
		if time.Since(at) > time.Minute {
			delete(t.lastReply, ip)
		}
	}
	t.mu.Unlock()
	if d.identity() == nil || d.gossip == nil {
		return nil
	}
	reply, err := gossip.SealMessage(key, gossip.LinksMagic, d.ownLinks(q.Nonce))
	if err != nil {
		return err
	}
	return d.gossip.sendTo(reply, from.IP)
}

// handleLinkReply stores an answer to this node's current round.
func (d *Daemon) handleLinkReply(key, pkt []byte) error {
	l, err := gossip.OpenLinks(key, pkt)
	if err != nil {
		return err
	}
	t := &d.topo
	t.mu.Lock()
	defer t.mu.Unlock()
	if l.Nonce != t.nonce || t.nonce == 0 {
		return nil // an answer to an old round, or a replay
	}
	mac := strings.ToLower(l.MAC)
	if _, known := d.peers.ByMAC(mac); !known {
		return nil
	}
	if prev, ok := t.reports[mac]; !ok || prev.links.Nonce != l.Nonce {
		t.waiting--
		if t.waiting <= 0 && t.done != nil {
			close(t.done)
			t.done = nil
		}
	}
	t.reports[mac] = linkReport{links: l, at: time.Now()}
	return nil
}

// askLinks runs one round of link queries if the last is old enough, and
// waits briefly for the answers.
func (d *Daemon) askLinks(ctx context.Context) {
	t := &d.topo
	t.askMu.Lock()
	defer t.askMu.Unlock()
	peers := d.peers.Peers()
	t.mu.Lock()
	if t.reports == nil {
		t.reports = map[string]linkReport{}
	}
	due := time.Since(t.lastAsk) >= topoInterval(len(peers))
	t.mu.Unlock()
	me := d.identity()
	if !due || len(peers) == 0 || me == nil || d.gossip == nil {
		return
	}
	key := d.gossip.currentKey()
	if key == nil {
		return
	}
	var b [8]byte
	_, _ = crand.Read(b[:])
	nonce := binary.BigEndian.Uint64(b[:]) | 1
	pkt, err := gossip.SealMessage(key, gossip.QueryMagic, &gossip.Query{V: 1, MAC: me.MAC, Nonce: nonce})
	if err != nil {
		return
	}
	done := make(chan struct{})
	t.mu.Lock()
	t.nonce, t.lastAsk, t.waiting, t.done = nonce, time.Now(), 0, done
	t.mu.Unlock()
	sent := 0
	for _, p := range peers {
		ip := net.ParseIP(p.Record.Addr)
		if ip == nil {
			continue
		}
		if err := d.gossip.sendTo(pkt, ip); err == nil {
			sent++
		}
	}
	t.mu.Lock()
	// Answers may already have come in while the queries went out.
	t.waiting += sent
	if t.waiting <= 0 && t.done != nil {
		close(t.done)
		t.done = nil
	}
	t.mu.Unlock()
	select {
	case <-done:
	case <-time.After(topoWait):
	case <-ctx.Done():
	}
}

// Topology returns the mesh graph: this node's view of who exists (gossip)
// joined with every node's report of its own links.
func (d *Daemon) Topology(ctx context.Context) (*status.Topology, error) {
	d.askLinks(ctx)
	me := d.identity()
	peers := d.peers.Peers()
	interval := topoInterval(len(peers))
	out := &status.Topology{Time: time.Now(), Nodes: []status.TopoNode{}, Links: []status.TopoLink{}, IntervalMS: interval.Milliseconds()}
	if me == nil {
		return out, nil
	}
	node, _ := d.conf()
	var batIfindex int
	var addr string
	var gw status.GatewayState
	d.facts.Read(func(f *Facts) { batIfindex, addr, gw = f.BatIfindex, f.BridgeAddress, f.Gateway })
	addr, _, _ = strings.Cut(addr, "/")

	owner := map[string]string{} // interface MAC → node ID (identity MAC)
	selfID := strings.ToLower(me.MAC)
	for _, m := range hardIfMACs(batIfindex) {
		owner[strings.ToLower(m)] = selfID
	}
	reports := map[string]linkReport{selfID: {links: d.ownLinks(0), at: out.Time}}
	self := status.TopoNode{ID: selfID, Callsign: node.Callsign, NodeID: me.NodeID, Address: addr, Self: true, Version: d.opts.Version,
		Gateway: gw.WANKnown && gw.Role == "gateway", WANHealthy: gw.WANHealthy, Reported: true}
	nodes := map[string]*status.TopoNode{selfID: &self}

	d.topo.mu.Lock()
	for _, p := range peers {
		r := p.Record
		id := strings.ToLower(r.MAC)
		n := &status.TopoNode{ID: id, Callsign: r.Callsign, NodeID: r.ID, Address: r.Addr, Version: r.Version}
		if r.GW != nil {
			n.Gateway, n.WANHealthy = true, r.GW.Healthy
		}
		for _, o := range r.Orig {
			owner[strings.ToLower(o)] = id
		}
		// A report is used for three rounds, then the node counts as silent.
		if rep, ok := d.topo.reports[id]; ok && out.Time.Sub(rep.at) < 3*interval+topoWait {
			reports[id] = rep
			n.Reported, n.ReportAgeMS = true, out.Time.Sub(rep.at).Milliseconds()
		}
		nodes[id] = n
	}
	for id := range d.topo.reports { // forget nodes that left
		if _, ok := nodes[id]; !ok {
			delete(d.topo.reports, id)
		}
	}
	d.topo.mu.Unlock()

	links := map[string]*status.TopoLink{}
	for id, rep := range reports {
		n := nodes[id]
		n.Clients, n.SelectedGateway = rep.links.Clients, rep.links.Selected
		for _, l := range rep.links.Links {
			a, b := strings.ToLower(l.Local), strings.ToLower(l.Remote)
			other, ok := owner[b]
			if !ok {
				// A neighbour gossip has not named (yet): show it by its
				// radio's address.
				other = b
				if _, exists := nodes[other]; !exists {
					nodes[other] = &status.TopoNode{ID: other}
				}
			}
			if other == id {
				continue
			}
			key := min(a, b) + "|" + max(a, b)
			medium := "wifi"
			if l.Wired {
				medium = "wired"
			}
			mbps := float64(l.Kbps) / 1000
			if tl, ok := links[key]; !ok {
				links[key] = &status.TopoLink{A: id, B: other, ABMbps: mbps, ASignalDBm: l.Signal, Medium: medium, AMAC: a, BMAC: b, MeshLink: l.MeshLink}
			} else if tl.A == other {
				tl.BAMbps, tl.BSignalDBm = mbps, l.Signal
				if tl.MeshLink == 0 {
					tl.MeshLink = l.MeshLink // the other end may be the older one
				}
			}
		}
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, *n)
	}
	slices.SortFunc(out.Nodes, func(a, b status.TopoNode) int {
		return cmp.Or(cmp.Compare(a.NodeID, b.NodeID), cmp.Compare(a.ID, b.ID))
	})
	for _, l := range links {
		out.Links = append(out.Links, *l)
	}
	slices.SortFunc(out.Links, func(a, b status.TopoLink) int {
		return cmp.Or(cmp.Compare(a.A, b.A), cmp.Compare(a.B, b.B), cmp.Compare(a.AMAC, b.AMAC))
	})
	return out, nil
}
