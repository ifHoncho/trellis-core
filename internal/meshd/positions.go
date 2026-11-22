package meshd

import (
	"context"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/cot"
	"meridian/trellis/internal/gnss"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
)

// Node positions on the team's ATAK map. A node with a GNSS fix sends its
// own position as a CoT event to the mesh's SA group, as the phones do, so
// stock ATAK shows it, drawn as equipment rather than a person. Its record
// carries the position too; when the node leaves the mesh, the
// lowest-numbered node still on it re-announces its last known position,
// already stale, so the map keeps it greyed rather than dropping it.
//
// Positions stay inside the mesh: the SA group is sent with TTL 1 on the
// mesh bridge and never routed, and gossip is the mesh's own. Turning
// positions.share off in the profile stops every node at once and drops
// the last known positions; in node.yaml it stops that node.

const (
	posEvery      = 5 * time.Second  // how often the reconciler looks
	posStill      = 60 * time.Second // a still node reports this often
	posMinGap     = 5 * time.Second  // and a moving one at most this often
	posMoveM      = 15.0             // a move worth reporting at once, at least
	posStaleAfter = 3 * time.Minute  // a report's life on the map
	posFixFresh   = 10 * time.Second // older receiver reports are no fix
	posKeepFor    = 12 * time.Hour   // last known positions are dropped after
	posReannounce = 60 * time.Second // last known positions are re-sent this often
	// For a while after a node starts sharing (a start, a fix after none,
	// sharing turned on) it reports more often: the node that kept its last
	// known position hears it return only once gossip reaches it (a minute
	// or so after a restart), and re-announces it as stale meanwhile. A
	// report every posStartEvery puts it back on the map soon after each.
	posStartFor   = 2 * time.Minute
	posStartEvery = 15 * time.Second
)

// positionsState is shared between the reconciler and status.
type positionsState struct {
	mu        sync.Mutex
	conn      *net.UDPConn
	sent      uint64
	lastSent  time.Time
	lastLat   float64
	lastLon   float64
	lastAnnce time.Time
	since     time.Time                // sharing with a fix since; zero while not
	known     map[string]*lastKnownPos // identity MAC → last shared position
	st        status.Positions         // as last reconciled
}

type lastKnownPos struct {
	callsign string
	pos      gossip.Position
	heard    time.Time
}

type positionsReconciler struct{ d *Daemon }

func (r *positionsReconciler) Name() string { return "positions" }

func (r *positionsReconciler) Teardown(ctx context.Context) error {
	s := &r.d.positions
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
	return nil
}

// positionsOff says which setting turns sharing off: "profile", "node" or "".
func (d *Daemon) positionsOff() string { return positionsOffIn(d.cfg, d.profile) }

// positionsOffIn is positionsOff for a configuration read with d.conf(),
// from a goroutine other than the reconcile loop's.
func positionsOffIn(cfg *config.Node, profile *config.Profile) string {
	switch {
	case profile != nil && !profile.Positions.Shared():
		return "profile"
	case cfg != nil && !cfg.Positions.Shared():
		return "node"
	}
	return ""
}

// ownFix is the receiver's fix, if there is a fresh one.
func ownFix(w *gnss.Watcher, now time.Time) (gnss.State, bool) {
	if w == nil {
		return gnss.State{}, false
	}
	st := w.State()
	return st, st.Mode >= gnss.Mode2D && now.Sub(st.Heard) < posFixFresh && !(st.Lat == 0 && st.Lon == 0)
}

// gossipPosition is what this node's record says of its position.
func (d *Daemon) gossipPosition(now time.Time) (pos *gossip.Position, off bool) {
	if d.positionsOff() != "" {
		return nil, true
	}
	st, device, _, ok := d.positionFix(now)
	if !ok {
		return nil, false
	}
	t := st.Time
	if t.IsZero() {
		t = st.Heard
	}
	p := gossip.NewPosition(st.Lat, st.Lon, st.AltM, st.HasAlt, st.HAccM, st.Mode, t.Unix())
	p.Device = device != ""
	return p, false
}

func (r *positionsReconciler) Reconcile(ctx context.Context) (res reconcile.Result, err error) {
	d := r.d
	res.RequeueAfter = posEvery
	now := time.Now()
	s := &d.positions
	off := d.positionsOff()
	me := d.identity()
	// This node's position: its receiver's fix, else a device's.
	var fix gnss.State
	var device, how string
	var ok bool
	if off == "" {
		fix, device, how, ok = d.positionFix(now)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.known == nil {
		s.known = map[string]*lastKnownPos{}
	}
	st := status.Positions{Off: off, Sent: s.sent, LastSentAgoMS: -1}
	if !s.lastSent.IsZero() {
		st.LastSentAgoMS = now.Sub(s.lastSent).Milliseconds()
	}
	defer func() { d.phoneStatus(&st); s.st = st }()
	d.startPhoneGPS()

	// Last known positions, from the records of the nodes on the mesh.
	live := map[string]bool{}
	lowest := true
	if d.peers != nil {
		for _, p := range d.peers.Peers() {
			rec := p.Record
			live[rec.MAC] = true
			if me != nil && rec.ID > 0 && me.NodeID > 0 && rec.ID < me.NodeID {
				lowest = false
			}
			switch {
			case rec.NoPos:
				delete(s.known, rec.MAC)
			case rec.Pos != nil:
				s.known[rec.MAC] = &lastKnownPos{callsign: rec.Callsign, pos: *rec.Pos, heard: p.LastHeard}
			}
		}
	}
	if me != nil {
		delete(s.known, me.MAC)
	}
	if off == "profile" {
		// Emissions control for the whole mesh: nothing is sent, and
		// nothing is kept.
		clear(s.known)
		s.since = time.Time{}
		st.Note = "turned off for the whole mesh (positions.share in the profile)"
		return res, nil
	}

	if err := s.open(); err != nil {
		st.Note = "cannot send: " + err.Error()
		return res, nil
	}

	if !ok || off != "" || me == nil {
		s.since = time.Time{}
	}
	switch {
	case off == "node":
		st.Note = "turned off for this node (positions.share in node.yaml)"
	case me == nil:
		st.Note = "waiting for the node's identity"
	case !ok:
		st.Note = "no GNSS fix, and no position from a device on this node"
	default:
		if s.since.IsZero() {
			s.since = now
		}
		every := posStill
		if now.Sub(s.since) < posStartFor {
			every = posStartEvery
		}
		moved := distanceM(s.lastLat, s.lastLon, fix.Lat, fix.Lon)
		due := s.lastSent.IsZero() || now.Sub(s.lastSent) >= every ||
			(now.Sub(s.lastSent) >= posMinGap && moved >= max(posMoveM, fix.HAccM))
		if due {
			ev := nodeEvent(me.MAC, d.cfg.Callsign, fix, now)
			if device != "" {
				// Not this node's receiver: no GPS claim of its own, and
				// the device's how.
				ev.Remarks, ev.GPS, ev.How = "Trellis mesh node. Position from a device on this node.", false, how
			}
			if err := s.send(ev); err != nil {
				st.Note = "cannot send: " + err.Error()
				return res, nil
			}
			s.sent++
			s.lastSent, s.lastLat, s.lastLon = now, fix.Lat, fix.Lon
			st.Sent, st.LastSentAgoMS = s.sent, 0
		}
		st.Shared, st.Device = true, device
		st.Note = fmt.Sprintf("on the ATAK map, re-sent every %s while still and at once on a move", posStill)
		if device != "" {
			st.Note += "; position from the device at " + device
		}
	}

	// Nodes that left: the lowest-numbered node re-announces them.
	st.Announcing = lowest
	reannounce := lowest && now.Sub(s.lastAnnce) >= posReannounce
	macs := make([]string, 0, len(s.known))
	for mac := range s.known {
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	for _, mac := range macs {
		k := s.known[mac]
		if live[mac] {
			continue
		}
		if now.Sub(k.heard) > posKeepFor {
			delete(s.known, mac)
			continue
		}
		st.LastKnown = append(st.LastKnown, status.LastKnown{Callsign: k.callsign, MAC: mac, Position: positionStatus(k.pos), Heard: k.heard})
		if reannounce {
			if err := s.send(lastKnownEvent(mac, k, now)); err != nil {
				st.Note += "; cannot re-announce: " + err.Error()
			} else {
				d.log.Debug("re-announced a last known position", "node", k.callsign, "heard", k.heard.Format(time.RFC3339))
			}
		}
	}
	if reannounce {
		s.lastAnnce = now
	}
	return res, nil
}

// nodeUID is a node's CoT UID, from its identity MAC: the same across
// restarts and callsign changes.
func nodeUID(mac string) string { return "trellis-node-" + strings.ReplaceAll(mac, ":", "") }

func nodeEvent(mac, callsign string, fix gnss.State, now time.Time) cot.Event {
	hae, le := cot.Unknown, cot.Unknown
	if fix.HasAlt {
		hae = fix.AltM
		if fix.VAccM > 0 {
			le = fix.VAccM
		}
	}
	ce := cot.Unknown
	if fix.HAccM > 0 {
		ce = fix.HAccM
	}
	rm := fmt.Sprintf("Trellis mesh node. %s fix", strings.ToUpper(gnss.Fix(fix.Mode)))
	if fix.SatsUsed > 0 {
		rm += fmt.Sprintf(", %d satellites", fix.SatsUsed)
	}
	if fix.HAccM > 0 {
		rm += fmt.Sprintf(", ±%.0f m", fix.HAccM)
	}
	return cot.Event{
		UID: nodeUID(mac), Type: cot.NodeType, Time: now, Start: now, Stale: now.Add(posStaleAfter),
		Lat: fix.Lat, Lon: fix.Lon, HAE: hae, CE: ce, LE: le, Callsign: callsign, Remarks: rm + ".", GPS: true,
	}
}

// lastKnownEvent re-announces a node that left the mesh: its own UID at
// its last shared position, stale since it was last heard. The label says
// "(last known)" too: ATAK does not always draw a stale marker differently,
// and a node that is gone must not pass for one that is there. Its own
// next report puts the plain callsign back.
func lastKnownEvent(mac string, k *lastKnownPos, now time.Time) cot.Event {
	hae := cot.Unknown
	if k.pos.HasAlt {
		hae = k.pos.Alt
	}
	ce := cot.Unknown
	if k.pos.Acc > 0 {
		ce = float64(k.pos.Acc)
	}
	start := k.heard
	rm := fmt.Sprintf("Trellis mesh node, last known position: not on the mesh since %s UTC.", k.heard.UTC().Format("15:04"))
	if k.pos.Device {
		rm += " Position from a device on the node."
	}
	ev := cot.Event{
		UID: nodeUID(mac), Type: cot.NodeType, Time: now, Start: start, Stale: start.Add(posStaleAfter),
		Lat: k.pos.Lat, Lon: k.pos.Lon, HAE: hae, CE: ce, LE: cot.Unknown, Callsign: k.callsign + " (last known)", GPS: !k.pos.Device,
		Remarks: rm,
	}
	if k.pos.Device {
		ev.How = "m-f" // not "machine, GPS": a device's, its source not known here
	}
	return ev
}

func positionStatus(p gossip.Position) status.Position {
	out := status.Position{Lat: p.Lat, Lon: p.Lon, AccM: p.Acc, Fix: fmt.Sprintf("%dd", p.Fix), Time: time.Unix(p.Time, 0), Device: p.Device}
	if p.HasAlt {
		alt := p.Alt
		out.AltM = &alt
	}
	return out
}

// status returns the positions block for status.
func (s *positionsState) status() status.Positions {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.LastKnown = append([]status.LastKnown(nil), st.LastKnown...)
	return st
}

// open makes the sending socket: on the mesh bridge, TTL 1, so reports
// reach the mesh and its devices and nothing beyond. Called with s.mu held.
func (s *positionsState) open() error {
	if s.conn != nil {
		return nil
	}
	ifi, err := net.InterfaceByName(BridgeIfname)
	if err != nil {
		return err
	}
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			fdi := int(fd)
			if serr = unix.BindToDevice(fdi, BridgeIfname); serr != nil {
				return
			}
			if serr = unix.SetsockoptIPMreqn(fdi, unix.IPPROTO_IP, unix.IP_MULTICAST_IF, &unix.IPMreqn{Ifindex: int32(ifi.Index)}); serr != nil {
				return
			}
			if serr = unix.SetsockoptInt(fdi, unix.IPPROTO_IP, unix.IP_MULTICAST_TTL, 1); serr != nil {
				return
			}
			serr = unix.SetsockoptInt(fdi, unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP, 0)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	c, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
	if err != nil {
		return err
	}
	s.conn = c.(*net.UDPConn)
	return nil
}

// send writes one event to the SA group. Called with s.mu held.
func (s *positionsState) send(ev cot.Event) error {
	b, err := ev.Marshal()
	if err != nil {
		return err
	}
	_, err = s.conn.WriteToUDP(b, &net.UDPAddr{IP: net.ParseIP(cot.SAGroup), Port: cot.SAPort})
	if err != nil {
		// The bridge may have been recreated: open again next time.
		s.conn.Close()
		s.conn = nil
	}
	return err
}

// distanceM is the great-circle distance in metres.
func distanceM(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dlat, dlon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dlat/2)*math.Sin(dlat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dlon/2)*math.Sin(dlon/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(a)))
}
