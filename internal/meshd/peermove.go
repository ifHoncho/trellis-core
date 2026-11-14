package meshd

import (
	"context"
	"net"
	"time"
)

// A peer that moves its one radio from one mesh link to another keeps its
// address. This node may then hold it twice: live on the link it moved to,
// and as a leftover on the link it left, until that peer entry times out
// (five minutes in wpa_supplicant). On a card whose radios share one chip
// (a dual-band-concurrent card), the firmware matches the peer's unicast
// frames to the leftover entry, and nothing unicast passes on the new link:
// the link shows as established and carries no traffic. meshd removes the
// leftover.

// Thresholds: a peer on a link is heard several times a second (its beacons
// count), so one that has been silent this long there, while it is heard on
// another link, has left.
const (
	peerLiveMS  = 1500
	peerStaleMS = 3000
)

// linkPeer is one peer entry on one of this node's mesh links.
type linkPeer struct {
	link       int
	mac        string
	inactiveMS uint32
}

// movedPeers picks the leftover entries: a peer address present on several
// links, silent on one and heard on another. pending reports an address on
// several links that is not silent long enough yet to be sure.
func movedPeers(peers []linkPeer) (stale []linkPeer, pending bool) {
	byMAC := map[string][]linkPeer{}
	for _, p := range peers {
		byMAC[p.mac] = append(byMAC[p.mac], p)
	}
	for _, entries := range byMAC {
		if len(entries) < 2 {
			continue
		}
		live := false
		for _, e := range entries {
			if e.inactiveMS <= peerLiveMS {
				live = true
			}
		}
		for _, e := range entries {
			switch {
			case e.inactiveMS <= peerLiveMS:
			case !live:
				// Silent everywhere: gone, and its entries time out.
			case e.inactiveMS >= peerStaleMS:
				stale = append(stale, e)
			default:
				pending = true
			}
		}
	}
	return stale, pending
}

// linkReconciler is the reconciler of mesh link i, or nil.
func (d *Daemon) linkReconciler(i int) *meshLinkReconciler {
	switch {
	case i == 0:
		return d.meshlink
	case d.morelinks != nil && i >= 1 && i <= len(d.morelinks.links):
		return d.morelinks.links[i-1]
	}
	return nil
}

// leaveLink stops wpa_supplicant on mesh link i before its interface goes
// away, so that it leaves the mesh properly: it tells every peer, and they
// drop this node from that link at once instead of holding a dead peer for
// minutes. Deleting the interface under a running wpa_supplicant sends
// nothing ("mesh leave error=-19").
func (d *Daemon) leaveLink(i int, why string) {
	if m := d.linkReconciler(i); m != nil && m.sup != nil {
		d.log.Info("leaving the mesh on a link before its interface goes", "interface", meshIfname(i), "reason", why)
		m.stop()
	}
}

// dropMovedPeers removes leftover peer entries (see above). It returns
// whether it removed any, and whether to look again soon.
func (d *Daemon) dropMovedPeers(ctx context.Context) (changed, again bool) {
	if d.nl == nil {
		return false, false
	}
	type link struct{ i, ifindex int }
	var links []link
	d.facts.Read(func(f *Facts) {
		if f.MeshIfindex != 0 {
			links = append(links, link{0, f.MeshIfindex})
		}
		for i, l := range f.MoreLinks {
			if l.Ifindex != 0 {
				links = append(links, link{i + 1, l.Ifindex})
			}
		}
	})
	if len(links) < 2 {
		return false, false
	}
	ifindex := map[int]int{}
	var peers []linkPeer
	for _, l := range links {
		ifindex[l.i] = l.ifindex
		sts, err := d.nl.Stations(l.ifindex)
		if err != nil {
			continue
		}
		for _, s := range sts {
			peers = append(peers, linkPeer{link: l.i, mac: s.MAC.String(), inactiveMS: s.InactiveMS})
		}
	}
	stale, pending := movedPeers(peers)
	if d.movedAt == nil {
		d.movedAt = map[string]time.Time{}
	}
	for _, p := range stale {
		name := meshIfname(p.link)
		key := name + " " + p.mac
		// Both: wpa_supplicant closes the peering it holds, which leaves the
		// kernel's entry in place (plink state "holding"),
		// and it is the kernel's entry, and the radio's with it, that has
		// to go.
		if m := d.linkReconciler(p.link); m != nil && m.ctrl != nil {
			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, _ = m.ctrl.Request(rctx, "MESH_PEER_REMOVE "+p.mac)
			cancel()
		}
		mac, err := net.ParseMAC(p.mac)
		if err == nil {
			err = d.nl.DelStation(ifindex[p.link], mac)
		}
		// Said once per move, not on every pass while an entry resists.
		first := time.Since(d.movedAt[key]) > time.Minute
		d.movedAt[key] = time.Now()
		switch {
		case err != nil && first:
			d.log.Warn("could not remove a peer's leftover entry; the link it moved to may carry no traffic until the entry times out", "interface", name, "peer", p.mac, "err", err)
		case err == nil && first:
			d.log.Info("removed a peer's leftover entry: it moved its radio to another mesh link", "interface", name, "peer", p.mac, "silent", time.Duration(p.inactiveMS)*time.Millisecond)
			changed = true
		}
	}
	return changed, pending
}
