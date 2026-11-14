package meshd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"meridian/trellis/internal/chrony"
	"meridian/trellis/internal/gnss"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
)

// Mesh time (roadmap G1). chrony keeps the clock: from the node's own GNSS
// receiver (gpsd) where it has one, else from the mesh's time servers, else
// from the internet through a gateway; with nothing, the nodes agree among
// themselves (orphan mode, packaging/chrony/trellis.conf). This reconciler
// tells chrony which nodes serve time, from gossip, and reads back how the
// clock is kept, for status and for this node's own gossip record.

const (
	timeCheckEvery = 15 * time.Second
	// maxMeshTimeServers bounds the servers chrony is given: a few good
	// ones are enough, and every one costs a little airtime.
	maxMeshTimeServers = 4
)

type timeReconciler struct {
	d      *Daemon
	chrony *chrony.Client
	dir    string

	last     time.Time
	warnings []reconcile.Warning

	mu    sync.Mutex
	clock status.Clock
	role  string // gossip.TimeGNSS, gossip.TimeServer or ""
}

func newTimeReconciler(d *Daemon) *timeReconciler {
	return &timeReconciler{d: d, chrony: &chrony.Client{}, dir: chrony.SourceDir,
		clock: status.Clock{Service: "none", Note: "not checked yet"}}
}

func (t *timeReconciler) Name() string { return "time" }

// Teardown leaves chrony's list of mesh servers: they are unreachable
// while meshd is down, and chrony goes on with GNSS or the internet.
func (t *timeReconciler) Teardown(ctx context.Context) error { return nil }

// Clock is the last look at how the clock is kept.
func (t *timeReconciler) Clock() status.Clock {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.clock
	c.MeshServers = slices.Clone(c.MeshServers)
	return c
}

// Role is what this node's gossip record says about serving time.
func (t *timeReconciler) Role() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.role
}

func (t *timeReconciler) Reconcile(ctx context.Context) (res reconcile.Result, _ error) {
	// chrony is asked every so often, not on every pass; its warnings stand
	// in between.
	if !t.last.IsZero() && time.Since(t.last) < timeCheckEvery {
		res.Warnings = t.warnings
		return res, nil
	}
	t.last = time.Now()
	t.warnings = nil
	warn := func(level, code, format string, args ...any) {
		t.warnings = append(t.warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	defer func() { res.Warnings = t.warnings }()

	if t.d.opts.NoHostConfig {
		// The namespace lab: the host's chrony is not this node's.
		t.set(status.Clock{Service: "none", Note: "not managed in the lab"}, "")
		return res, nil
	}
	tr, err := t.chrony.Tracking(ctx)
	if errors.Is(err, chrony.ErrNotInstalled) {
		t.set(status.Clock{Service: "none", Note: "chrony is not installed: this node neither serves nor follows mesh time"}, "")
		warn("info", "time-no-chrony", "chrony is not installed: this node neither serves nor follows mesh time (scripts/install-node.sh installs it)")
		return res, nil
	}
	if err != nil {
		t.set(status.Clock{Service: "chrony", Note: "chronyd does not answer: " + err.Error()}, "")
		warn("warning", "time-chrony", "chronyd does not answer, so the clock is not kept and this node serves no time: %v", err)
		return res, nil
	}

	// The servers chrony follows: the nodes whose clock is on GNSS, or,
	// with none in the mesh, every node that serves time, so that nodes
	// without any upstream still settle on one clock.
	servers := t.meshServers()
	addrs := make([]string, len(servers))
	for i, s := range servers {
		addrs[i] = s.Addr
	}
	if changed, err := chrony.WriteSources(t.dir, addrs); err != nil {
		warn("warning", "time-sources", "cannot write the mesh's time servers for chrony: %v", err)
	} else if changed {
		if err := t.chrony.ReloadSources(ctx); err != nil {
			warn("warning", "time-sources", "chrony did not take the mesh's time servers: %v", err)
		} else {
			t.d.log.Info("mesh time servers for chrony", "servers", strings.Join(addrs, " "))
			res.Changed = true
		}
	}

	c := status.Clock{Service: "chrony", Synced: tr.Synced(), Stratum: tr.Stratum,
		OffsetMS: float64(tr.Offset.Microseconds()) / 1000, Serving: true}
	for _, s := range servers {
		c.MeshServers = append(c.MeshServers, s.Callsign)
	}
	c.Source, c.SourceKind = t.describeSource(tr)
	role := gossip.TimeServer
	if tr.FromGNSS() {
		role = gossip.TimeGNSS
	}
	switch {
	case !tr.Synced():
		c.Note = "not synchronised yet: no GNSS fix, no mesh time server and no internet in reach"
	case c.SourceKind == "local":
		// Orphan mode: the mesh agrees on this node's clock, which follows
		// nothing.
		c.Synced = false
		c.Note = "no GNSS fix, mesh time server or internet in reach: the mesh keeps to this node's clock for now"
	}
	t.set(c, role)
	if p := t.d.profile.Prefix; p != "" && p != "10.73.0.0/16" {
		warn("warning", "time-prefix", "chrony serves time to 10.73.0.0/16 (packaging/chrony/trellis.conf), but this mesh uses %s: nodes cannot reach each other's time", p)
	}
	return res, nil
}

func (t *timeReconciler) set(c status.Clock, role string) {
	t.mu.Lock()
	t.clock, t.role = c, role
	t.mu.Unlock()
}

type meshTimeServer struct {
	Callsign, Addr string
	ID             int
}

// meshServers picks the nodes to follow, from gossip: those on GNSS, or,
// with none, all that serve time; the lowest node IDs first, so that every
// node picks the same ones.
func (t *timeReconciler) meshServers() []meshTimeServer {
	if t.d.peers == nil {
		return nil
	}
	var gnssNodes, servers []meshTimeServer
	for _, p := range t.d.peers.Peers() {
		r := p.Record
		if r.Addr == "" || net.ParseIP(r.Addr) == nil {
			continue
		}
		s := meshTimeServer{Callsign: r.Callsign, Addr: r.Addr, ID: r.ID}
		switch r.Time {
		case gossip.TimeGNSS:
			gnssNodes = append(gnssNodes, s)
		case gossip.TimeServer:
			servers = append(servers, s)
		}
	}
	pick := servers
	if len(gnssNodes) > 0 {
		pick = gnssNodes
	}
	slices.SortFunc(pick, func(a, b meshTimeServer) int { return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.Addr, b.Addr)) })
	if len(pick) > maxMeshTimeServers {
		pick = pick[:maxMeshTimeServers]
	}
	return pick
}

// describeSource says in words what the clock follows.
func (t *timeReconciler) describeSource(tr chrony.Tracking) (text, kind string) {
	switch {
	case !tr.Synced():
		return "", ""
	case tr.RefName == chrony.RefPPS:
		return "GNSS receiver (pulse per second)", "gnss"
	case tr.RefName == chrony.RefGNSS:
		return "GNSS receiver", "gnss"
	case tr.RefID == "7F7F0101" || tr.RefName == "127.127.1.1":
		return "this node's own clock (no other source in reach)", "local"
	}
	if ip := net.ParseIP(tr.RefName); ip != nil {
		if p := t.d.peers; p != nil {
			for _, peer := range p.Peers() {
				if peer.Record.Addr == ip.String() {
					return fmt.Sprintf("%s (%s)", peer.Record.Callsign, ip), "mesh"
				}
			}
		}
		if _, mesh, err := net.ParseCIDR(t.d.profile.Prefix); err == nil && mesh.Contains(ip) {
			return fmt.Sprintf("mesh node %s", ip), "mesh"
		}
		return fmt.Sprintf("internet (%s)", ip), "internet"
	}
	return tr.RefName, "internet"
}

// gnssStatus renders the receiver's state for status.
func gnssStatus(w *gnss.Watcher, now time.Time) status.GNSS {
	g := status.GNSS{Fix: "none"}
	if w == nil {
		g.Note = "not watched"
		return g
	}
	st := w.State()
	switch {
	case !st.Running:
		g.Note = "no GNSS receiver (gpsd is not running)"
		return g
	case !st.Receiver:
		g.Note = "gpsd runs but sees no receiver"
		return g
	}
	g.Receiver, g.Device, g.Driver = true, st.Device, st.Driver
	g.SatsUsed, g.SatsSeen = st.SatsUsed, st.SatsSeen
	if !st.Heard.IsZero() {
		g.FixAgeMS = now.Sub(st.Heard).Milliseconds()
	}
	g.PPS = !st.PPS.IsZero() && now.Sub(st.PPS) < 10*time.Second
	// A report older than 10 s is no fix, whatever it said.
	if st.Mode >= gnss.Mode2D && now.Sub(st.Heard) < 10*time.Second {
		g.Fix = strings.ToLower(gnss.Fix(st.Mode))
		lat, lon := st.Lat, st.Lon
		g.Lat, g.Lon, g.HAccM = &lat, &lon, st.HAccM
		if st.HasAlt {
			alt := st.AltM
			g.AltM, g.VAccM = &alt, st.VAccM
		}
	}
	if g.Fix == "none" {
		g.Note = "no fix (indoors, covered, or still searching)"
	}
	return g
}
