package meshd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/dhcpc"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/rtnl"
	"meridian/trellis/internal/status"
)

// Auto port roles (architecture §7.1). A port in the auto role, or an idle
// Ethernet port nobody configured, is probed with DHCP when a cable is in:
// an offer from outside the mesh prefix makes it a WAN (meshd holds the
// lease), silence makes it a client port. An offer from inside the prefix,
// or another Trellis node probing the same cable, means the cable leads to
// another mesh node's client network; bridging it would loop the mesh, so
// such a port is left alone.
//
// A port that a system network manager (NetworkManager, systemd-networkd)
// runs with DHCP is probed the same way when a cable goes in and the
// manager holds no address there, or when the cable changes under the
// lease it holds. Silence: meshd takes the port over at runtime (the
// manager is told to let go) and makes it a client port. An offer from
// outside the mesh: the port stays the system's, and the manager takes the
// lease itself, so the node keeps its own uplink and DNS. The port goes
// back to the manager when a DHCP server answers on it later, and when
// meshd stops.
//
// Every client port is checked for a DHCP server every clientRecheck: one
// appearing (a router plugged in where a laptop was, a router that booted
// after the node) sends the port back to probing, or back to the manager
// it was taken from.
const (
	portProbing   = "probing"
	portClient    = "client"
	portWAN       = "wan"
	portPeer      = "peer"
	portNoLink    = "no-link"
	portInUse     = "in-use"
	portMissing   = "missing"
	portFailed    = "probe-failed"
	vendorClass   = "trellis-meshd"
	probeTotal    = 4 * time.Second
	nmCacheFor    = 10 * time.Second
	probeRetryIn  = 30 * time.Second
	clientRecheck = 20 * time.Second
	// Long enough for servers that ping an address before offering it
	// (dnsmasq waits up to 3 s).
	recheckTotal = 5 * time.Second
	// A port the system configured by DHCP gets a longer probe before it is
	// taken over: it is usually the node's own uplink.
	carefulTotal = 10 * time.Second
	// How long a manager may take to show it let go of a port before meshd
	// tells it again.
	claimSettle = 15 * time.Second
)

var (
	probeSchedule   = []time.Duration{0, time.Second, 2500 * time.Millisecond}
	carefulSchedule = []time.Duration{0, time.Second, 2500 * time.Millisecond, 5 * time.Second, 8 * time.Second}
	recheckSchedule = []time.Duration{0, time.Second, 2500 * time.Millisecond}
)

type portsReconciler struct {
	d    *Daemon
	mgrs []portManager // system network managers that may run ports

	mu    sync.Mutex
	ports map[string]*autoPort

	loaded    bool   // leftover addresses and claims read from the run directory
	addrsOut  string // what was last written there
	claimsOut string
}

// addrsFile records the addresses meshd put on auto WAN ports, so a meshd
// restarted after a crash recognises them as its own rather than as the
// system using the port. It lives in the run directory: addresses do not
// survive a reboot either.
func (r *portsReconciler) addrsFile() string {
	return filepath.Join(r.d.opts.RunDir, "auto-wan-addresses")
}

// claimsFile records the ports meshd took over from a system network
// manager, so a meshd restarted after a crash gives them back properly.
// Like the managers' own runtime state, it does not survive a reboot.
func (r *portsReconciler) claimsFile() string {
	return filepath.Join(r.d.opts.RunDir, "claimed-ports")
}

type autoPort struct {
	name   string
	listed bool // in node.yaml with role auto (the operator handed it over)
	role   string
	why    string
	since  time.Time
	lease  *dhcpc.Lease
	addr   *net.IPNet // address meshd put on the port (auto WAN)
	prevIP net.IP     // last leased address, asked for again
	failAt time.Time
	ifidx  int // interface the role was decided for; a new one is a new device

	// A port a system network manager runs.
	mgr      portManager // that manager, while meshd probes the port or holds it
	claimed  bool        // meshd told mgr to let go; mgr gets it back on release
	claimAt  time.Time
	leftTo   bool // a probe decided the port is not meshd's; holds while seenCC does
	leftRole string
	leftWhy  string
	seenCC   int  // the port's carrier_changes when last decided; -1 unknown
	redo     bool // the client-port check saw a DHCP server: decide again

	gen    int // worker generation: results from an older worker are ignored
	cancel context.CancelFunc
	done   chan struct{}
}

func newAutoPort(name string) *autoPort { return &autoPort{name: name, seenCC: -1} }

// forgetSystem drops what meshd knew about the port's system manager: the
// device is gone or new, and the manager starts over with it too.
func (p *autoPort) forgetSystem() {
	p.mgr, p.claimed, p.leftTo, p.redo, p.seenCC = nil, false, false, false, -1
}

func newPortsReconciler(d *Daemon) *portsReconciler {
	r := &portsReconciler{d: d, ports: map[string]*autoPort{}}
	switch {
	case d.opts.TestPortManagerDir != "":
		r.mgrs = []portManager{&testManager{dir: d.opts.TestPortManagerDir}}
	case d.opts.NoHostConfig:
		// A lab namespace: the host's managers are not ours to ask or tell.
	default:
		r.mgrs = []portManager{newNMManager(), newNetworkdManager()}
	}
	return r
}

func (r *portsReconciler) Name() string { return "ports" }

func (r *portsReconciler) managerNamed(name string) portManager {
	for _, m := range r.mgrs {
		if m.Name() == name {
			return m
		}
	}
	return nil
}

func (r *portsReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	cands := r.candidates()

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.loaded {
		r.loaded = true
		r.loadLeftovers()
	}
	for name, p := range r.ports {
		if _, ok := cands[name]; !ok {
			r.release(p)
			r.giveBack(p, true)
			delete(r.ports, name)
			res.Changed = true
		}
	}
	names := make([]string, 0, len(cands))
	for name := range cands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := r.ports[name]
		if p == nil {
			p = newAutoPort(name)
			r.ports[name] = p
		}
		p.listed = cands[name]
		if r.step(p, &res) {
			res.Changed = true
		}
		switch {
		case p.role == portPeer:
			warn("warning", "port-peer", "port %s leads to another Trellis node (%s); not bridging it, which would loop the mesh. For a wired mesh link give it role mesh on both nodes", name, p.why)
		case p.role == portInUse && p.listed:
			warn("warning", "port-in-use", "port %s is listed with role auto but the system uses it (%s); Trellis leaves it alone", name, p.why)
		case p.role == portFailed:
			warn("warning", "port-probe", "cannot probe port %s: %s", name, p.why)
			res.RequeueAfter = probeRetryIn
		case p.role == portMissing:
			warn("warning", "port-missing", "port %s (role auto) does not exist", name)
		}
	}
	r.publish()
	return res, nil
}

// loadLeftovers reads what an earlier meshd left in the run directory.
// Called with r.mu held.
func (r *portsReconciler) loadLeftovers() {
	if data, err := os.ReadFile(r.addrsFile()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			name, cidr, ok := strings.Cut(line, " ")
			ip, n, err := net.ParseCIDR(cidr)
			if !ok || err != nil {
				continue
			}
			n.IP = ip
			p := newAutoPort(name)
			p.addr = n
			r.ports[name] = p
		}
	}
	if data, err := os.ReadFile(r.claimsFile()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			name, mgrName, ok := strings.Cut(line, " ")
			m := r.managerNamed(mgrName)
			if !ok || m == nil {
				continue
			}
			p := r.ports[name]
			if p == nil {
				p = newAutoPort(name)
				r.ports[name] = p
			}
			p.mgr, p.claimed, p.claimAt = m, true, time.Now()
			r.d.log.Info("port taken over by an earlier meshd", "port", name, "manager", mgrName)
		}
	}
}

// step moves one port along; it reports a change.
func (r *portsReconciler) step(p *autoPort, res *reconcile.Result) bool {
	d := r.d
	link, err := rtnl.LinkByNameOrNil(p.name)
	if err != nil || link == nil {
		changed := r.release(p)
		r.giveBack(p, false)
		p.forgetSystem()
		return r.set(p, portMissing, "") || changed
	}
	if idx := link.Attrs().Index; p.ifidx != idx {
		// A new interface under the same name (an adapter replugged, a
		// cable moved faster than a pass): nothing decided before holds.
		changed := false
		if p.ifidx != 0 {
			changed = r.release(p)
			r.giveBack(p, false)
			p.forgetSystem()
			r.set(p, "", "")
			changed = true
		}
		p.ifidx = idx
		if changed {
			return r.step(p, res) || true
		}
	}
	if p.redo {
		// The client-port check saw a DHCP server: leave the bridge, give
		// the port back if it was taken over, and decide again.
		p.redo = false
		r.release(p)
		r.giveBack(p, true)
		p.role = ""
		return r.step(p, res) || true
	}
	attrs := link.Attrs()
	lowerUp := attrs.RawFlags&unix.IFF_LOWER_UP != 0
	if !r.owns(p) {
		use := r.inUse(p, link)
		if use.mgr == nil && !p.claimed {
			p.mgr = nil
		}
		cc := carrierChanges(d.opts.SysRoot, p.name)
		switch {
		case use.hard != "" && use.dynamic:
			// A manager configured the port by DHCP. If the cable changed
			// since, it may lead somewhere else now (a laptop where the
			// router was): NetworkManager keeps its lease over a short
			// unplug. Look again, carefully: this is usually the uplink.
			if lowerUp && p.seenCC >= 0 && cc != p.seenCC {
				p.mgr, p.leftTo = use.mgr, false
				d.log.Info("the cable changed under a port the system configured by DHCP; checking for a DHCP server", "port", p.name, "manager", use.mgr.Name())
				r.start(p, attrs.HardwareAddr, true)
				return true
			}
			p.seenCC = cc
			changed := r.release(p)
			return r.set(p, portInUse, use.hard) || changed
		case use.hard != "":
			changed := r.release(p)
			r.giveBack(p, true)
			p.seenCC = cc
			return r.set(p, portInUse, use.hard) || changed
		case use.mgr != nil:
			p.mgr = use.mgr
			switch {
			case !lowerUp:
				p.leftTo, p.seenCC = false, cc
				changed := r.release(p)
				return r.set(p, portInUse, fmt.Sprintf("%s manages it (%s); Trellis takes it over when a device without a DHCP server is plugged in", use.mgr.Name(), use.why)) || changed
			case p.leftTo && cc == p.seenCC:
				return r.set(p, p.leftRole, p.leftWhy)
			case p.role == portFailed && time.Since(p.failAt) < probeRetryIn:
				return false
			}
			r.start(p, attrs.HardwareAddr, false)
			return true
		}
	} else if p.mgr == nil {
		if why := r.systemManager(p, link); why != "" {
			// meshd starts before NetworkManager at boot, so it can claim a
			// port that NetworkManager then configures too, usually with the
			// same DHCP lease (same client ID, same server). Hand the port
			// over: release keeps the shared address.
			r.d.log.Warn("a system network manager configures a port meshd claimed; handing it over", "port", p.name, "role", p.role, "manager", why)
			changed := r.release(p)
			return r.set(p, portInUse, why) || changed
		}
	} else if p.claimed && time.Since(p.claimAt) > claimSettle {
		if st := p.mgr.Manages(p.name, attrs.Index); st != "" {
			// The manager runs the port again (it restarted, or someone told
			// it to): tell it again, or it fights the bridge for the port.
			if err := p.mgr.Release(p.name); err != nil {
				d.log.Warn("a system network manager runs a port Trellis took over again, and will not let go", "port", p.name, "manager", p.mgr.Name(), "state", st, "err", err)
			} else {
				d.log.Info("a system network manager ran a port Trellis took over again; told it to let go", "port", p.name, "manager", p.mgr.Name(), "state", st)
			}
			p.claimAt = time.Now()
		}
	}
	adminUp := attrs.Flags&net.FlagUp != 0
	if p.role == portClient && !adminUp {
		// The bridge holds client ports down briefly after a re-address
		// (r_bridge.go); that is not a cable change.
		return false
	}
	changed := false
	if !adminUp {
		if ch, err := rtnl.EnsureUp(link); err != nil {
			return r.set(p, portFailed, fmt.Sprintf("cannot bring it up: %v", err))
		} else if ch {
			changed = true
			d.log.Info("auto port up, waiting for a cable", "port", p.name)
		}
	}
	if !lowerUp {
		if p.role != portNoLink {
			changed = r.release(p) || changed
			changed = r.set(p, portNoLink, "no cable or no link partner") || changed
		}
		return changed
	}
	if p.addr != nil && p.role != portWAN && p.role != portProbing && p.role != "" {
		// A former WAN address (or one left by a crashed meshd) on a port
		// that is not a WAN now.
		if err := netlink.AddrDel(link, &netlink.Addr{IPNet: p.addr}); err == nil || rtnl.IsNotFound(err) {
			p.addr = nil
			changed = true
		}
	}
	switch p.role {
	case "", portNoLink, portInUse, portMissing:
		r.start(p, attrs.HardwareAddr, false)
		return true
	case portFailed:
		if time.Since(p.failAt) >= probeRetryIn {
			r.start(p, attrs.HardwareAddr, false)
			return true
		}
	case portWAN:
		return r.applyWANAddr(p, link) || changed
	}
	return changed
}

// set records a role; it reports a change.
func (r *portsReconciler) set(p *autoPort, role, why string) bool {
	if p.role == role && p.why == why {
		return false
	}
	if p.role != role {
		p.since = time.Now()
		if role != portMissing || p.role != "" {
			r.d.log.Info("port role", "port", p.name, "role", role, "why", why)
		}
	}
	p.role, p.why = role, why
	if role == portFailed {
		p.failAt = time.Now()
	}
	return true
}

// owns reports whether the port's current state is meshd's own doing. A
// port a system manager runs is meshd's only once taken over, or while
// meshd probes it.
func (r *portsReconciler) owns(p *autoPort) bool {
	switch p.role {
	case portClient, portWAN, portProbing, portPeer, portNoLink, portFailed:
		return p.mgr == nil || p.claimed || p.role == portProbing
	}
	return false
}

// portUse is how the system uses a port.
type portUse struct {
	hard    string      // why the system uses the port right now, or ""
	dynamic bool        // hard is only addresses mgr leased (and routes through them)
	mgr     portManager // a system manager that runs the port
	why     string      // the manager's description of that
}

func (r *portsReconciler) inUse(p *autoPort, link netlink.Link) portUse {
	var u portUse
	attrs := link.Attrs()
	if attrs.MasterIndex != 0 {
		if m, err := netlink.LinkByIndex(attrs.MasterIndex); err == nil && m.Attrs().Name != BridgeIfname {
			u.hard = "it is a member of " + m.Attrs().Name
			return u
		}
	}
	if !p.listed {
		// Listed with role auto: the operator handed it over, and meshd has
		// told NetworkManager and systemd-networkd to leave it alone.
		for _, m := range r.mgrs {
			if st := m.Manages(p.name, attrs.Index); st != "" {
				u.mgr, u.why = m, st
				break
			}
		}
	}
	leased := true
	if addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL); err == nil {
		for _, a := range addrs {
			if p.addr != nil && a.IPNet.String() == p.addr.String() {
				continue
			}
			if a.IP.To4() != nil || a.Scope == unix.RT_SCOPE_UNIVERSE {
				if u.hard == "" {
					u.hard = "it has the address " + a.IPNet.String()
				}
				if a.Flags&unix.IFA_F_PERMANENT != 0 {
					leased = false
				}
			}
		}
	}
	if u.hard == "" {
		if def, l, err := rtnl.MainDefault(nil); err == nil && def != nil && l != nil && l.Attrs().Index == attrs.Index {
			u.hard, leased = "it carries the system's default route", false
		}
	}
	if u.hard != "" {
		u.dynamic = leased && u.mgr != nil
		return u
	}
	if u.mgr == nil && !p.listed {
		u.hard = r.otherConfig(p.name)
	}
	return u
}

// systemManager names the system service that configures the port, or "".
func (r *portsReconciler) systemManager(p *autoPort, link netlink.Link) string {
	if p.listed {
		return ""
	}
	for _, m := range r.mgrs {
		if st := m.Manages(p.name, link.Attrs().Index); st != "" {
			return fmt.Sprintf("%s manages it (%s)", m.Name(), st)
		}
	}
	return r.otherConfig(p.name)
}

// otherConfig explains why a configuration meshd cannot take over at
// runtime may run the port, or returns "".
func (r *portsReconciler) otherConfig(name string) string {
	if r.d.opts.NoHostConfig {
		return ""
	}
	if ifupdownConfigures(name) {
		return "it is configured in /etc/network/interfaces; to hand it to Trellis, list it in node.yaml with role auto"
	}
	if processRunning("dhcpcd") {
		return "dhcpcd is running and may manage it; to hand it to Trellis, list it in node.yaml with role auto"
	}
	return ""
}

// carrierChanges reads the port's carrier change counter, or -1.
func carrierChanges(sysRoot, name string) int {
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	data, err := os.ReadFile(filepath.Join(sysRoot, "class/net", name, "carrier_changes"))
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1
	}
	return n
}

// start launches the probe-and-hold worker. Called with r.mu held. careful
// probes longer, for a port that is probably the node's own uplink.
func (r *portsReconciler) start(p *autoPort, mac net.HardwareAddr, careful bool) {
	d := r.d
	r.stopWorker(p)
	why := "asking for a DHCP server"
	if p.mgr != nil && !p.claimed {
		why += fmt.Sprintf(" (%s runs the port; Trellis takes it over if none answers)", p.mgr.Name())
	}
	r.set(p, portProbing, why)
	if p.mgr != nil {
		p.seenCC = carrierChanges(d.opts.SysRoot, p.name)
	}
	p.gen++
	gen, name, prev, callsign, ifindex := p.gen, p.name, p.prevIP, d.cfg.Callsign, p.ifidx
	mgr, claimed := p.mgr, p.claimed
	if mgr != nil {
		prev = nil // the address is the manager's to ask for
	}
	_, prefix, _ := net.ParseCIDR(d.profile.Prefix)
	ctx, cancel := context.WithCancel(d.ctx)
	done := make(chan struct{})
	p.cancel, p.done = cancel, done
	report := func(role, why string, l *dhcpc.Lease, setLease bool) {
		r.mu.Lock()
		cur := r.ports[name]
		if cur == nil || cur.gen != gen {
			r.mu.Unlock()
			return
		}
		r.set(cur, role, why)
		if setLease {
			cur.lease = l
			if l != nil {
				cur.prevIP = l.Addr.IP
			}
		}
		r.mu.Unlock()
		d.Kick("port " + name + ": " + role)
	}
	// leave: the port is not meshd's. A port taken over goes back to its
	// manager (step does that, after leaving the bridge). It stays as it
	// is until the cable changes.
	leave := func(role, why string) {
		r.mu.Lock()
		cur := r.ports[name]
		if cur == nil || cur.gen != gen {
			r.mu.Unlock()
			return
		}
		r.set(cur, role, why)
		cur.leftTo, cur.leftRole, cur.leftWhy = true, role, why
		cur.redo = cur.claimed
		r.mu.Unlock()
		d.Kick("port " + name + ": " + role)
	}
	go func() {
		defer close(done)
		if mgr != nil && !claimed {
			if why := mgr.Static(name, ifindex, mac); why != "" {
				leave(portInUse, why)
				return
			}
		}
		conn, err := dhcpc.Open(name)
		if err != nil {
			report(portFailed, err.Error(), nil, false)
			return
		}
		defer func() {
			if conn != nil {
				conn.Close()
			}
		}()
		c := &dhcpc.Client{Conn: conn, MAC: mac, Hostname: callsign, VendorClass: vendorClass}
		sched, total := probeSchedule, probeTotal
		if careful {
			sched, total = carefulSchedule, carefulTotal
		}
		offer, err := c.Probe(ctx, sched, total, prev)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, dhcpc.ErrPeer):
			if mgr != nil {
				leave(portPeer, "another Trellis node is probing the same cable")
				return
			}
			report(portPeer, "another Trellis node is probing the same cable", nil, false)
			return
		case errors.Is(err, dhcpc.ErrNoOffer):
			why := "no DHCP server answered"
			if mgr != nil && !claimed {
				if err := mgr.Release(name); err != nil {
					report(portFailed, fmt.Sprintf("no DHCP server answered, but %s would not let go of the port: %v", mgr.Name(), err), nil, false)
					return
				}
				d.log.Info("took a port over from the system network manager: no DHCP server answered on it", "port", name, "manager", mgr.Name())
				// Recorded even if this worker is stale by now, so whoever
				// stopped it gives the port back.
				r.mu.Lock()
				cur := r.ports[name]
				if cur != nil {
					cur.mgr, cur.claimed, cur.claimAt = mgr, true, time.Now()
				}
				r.mu.Unlock()
				if cur == nil {
					_ = mgr.Restore(name)
					return
				}
			}
			report(portClient, why, nil, false)
			conn.Close()
			conn = nil
			r.watchClient(ctx, name, mac, callsign, prefix, gen)
			return
		case err != nil:
			report(portFailed, err.Error(), nil, false)
			return
		case prefix != nil && prefix.Contains(offer.YIAddr):
			why := fmt.Sprintf("a DHCP server offered %s, inside the mesh prefix", offer.YIAddr)
			if mgr != nil {
				leave(portPeer, why)
				return
			}
			report(portPeer, why, nil, false)
			return
		case mgr != nil:
			leave(portInUse, fmt.Sprintf("DHCP server %s answered on it, so it stays %s's", offer.ServerID(), mgr.Name()))
			return
		}
		l, err := c.Request(ctx, offer)
		if ctx.Err() != nil {
			return
		}
		server := offer.ServerID().String()
		if err != nil {
			report(portWAN, fmt.Sprintf("DHCP server %s offered %s but the request failed (%v); retrying", server, offer.YIAddr, err), nil, true)
			l = nil
		} else {
			report(portWAN, "DHCP server "+server+" leased an address outside the mesh", l, true)
		}
		c.Hold(ctx, l, func(l *dhcpc.Lease) {
			if l == nil {
				report(portWAN, "the lease ran out or was refused; asking again", nil, true)
				return
			}
			report(portWAN, "DHCP server "+l.Server.String()+" leased an address outside the mesh", l, true)
		})
	}()
}

// watchClient looks for a DHCP server on a client port every clientRecheck
// until ctx ends. One that answers means the port is no longer just a
// device's: step decides again. The port is in the bridge, so the probe
// listens with a tap socket.
func (r *portsReconciler) watchClient(ctx context.Context, name string, mac net.HardwareAddr, callsign string, prefix *net.IPNet, gen int) {
	d := r.d
	for {
		t := time.NewTimer(clientRecheck)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		conn, err := dhcpc.OpenTap(name)
		if err != nil {
			d.log.Debug("cannot check a client port for a DHCP server", "port", name, "err", err)
			continue
		}
		c := &dhcpc.Client{Conn: conn, MAC: mac, Hostname: callsign, VendorClass: vendorClass}
		offer, err := c.Probe(ctx, recheckSchedule, recheckTotal, nil)
		conn.Close()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// No server, or another node probing the cable (it will find
			// this node's client network and leave its end alone).
			continue
		}
		where := "outside the mesh"
		if prefix != nil && prefix.Contains(offer.YIAddr) {
			where = "inside the mesh prefix: another Trellis node's client network"
		}
		why := fmt.Sprintf("DHCP server %s answered on the client port, offering %s (%s); deciding again", offer.ServerID(), offer.YIAddr, where)
		d.log.Info("a DHCP server answered on a client port", "port", name, "server", offer.ServerID().String(), "offered", offer.YIAddr.String())
		r.mu.Lock()
		cur := r.ports[name]
		if cur == nil || cur.gen != gen {
			r.mu.Unlock()
			return
		}
		r.set(cur, portProbing, why)
		cur.redo, cur.leftTo = true, false
		r.mu.Unlock()
		d.Kick("port " + name + ": DHCP server on a client port")
		return
	}
}

// stopWorker ends a port's worker and waits for it. Called with r.mu held;
// the worker's report takes r.mu, so it is released while waiting.
func (r *portsReconciler) stopWorker(p *autoPort) {
	if p.cancel == nil {
		return
	}
	p.cancel()
	p.gen++ // results still in flight are now stale
	done := p.done
	p.cancel, p.done = nil, nil
	r.mu.Unlock()
	<-done
	r.mu.Lock()
}

// release undoes what meshd did to a port: worker, bridge membership,
// address. A port taken over from a system manager stays taken (giveBack
// returns it). It reports a change.
func (r *portsReconciler) release(p *autoPort) bool {
	changed := p.cancel != nil
	r.stopWorker(p)
	p.lease = nil
	link, _ := rtnl.LinkByNameOrNil(p.name)
	if link == nil {
		p.addr = nil
		return changed
	}
	if link.Attrs().MasterIndex != 0 {
		// Only meshd puts auto ports in the bridge.
		if m, err := netlink.LinkByIndex(link.Attrs().MasterIndex); err == nil && m.Attrs().Name == BridgeIfname {
			if err := netlink.LinkSetNoMaster(link); err == nil {
				r.d.log.Info("auto port left the bridge", "port", p.name)
				changed = true
			}
		}
	}
	if p.addr != nil {
		// Deleting the address under another network manager takes the node
		// off its uplink: the manager holds the same lease and does not put
		// the address back until it renews (hours): stopping meshd would cut
		// the node off its LAN.
		if why := r.systemManager(p, link); why != "" {
			r.d.log.Info("leaving the auto WAN address in place: a system network manager configures the port and most likely holds the same lease",
				"port", p.name, "address", p.addr.String(), "manager", why)
		} else if err := netlink.AddrDel(link, &netlink.Addr{IPNet: p.addr}); err == nil {
			r.d.log.Info("removed auto WAN address", "port", p.name, "address", p.addr.String())
			changed = true
		}
		p.addr = nil
	}
	return changed
}

// giveBack returns a port meshd took over to its system manager. Call
// release first: the port must be out of the bridge. present is false when
// the interface is gone (the manager may not know it any more).
func (r *portsReconciler) giveBack(p *autoPort, present bool) {
	if !p.claimed || p.mgr == nil {
		p.claimed = false
		return
	}
	if err := p.mgr.Restore(p.name); err != nil {
		if present {
			r.d.log.Warn("cannot give a port back to its system network manager", "port", p.name, "manager", p.mgr.Name(), "err", err)
		}
	} else {
		r.d.log.Info("gave a port back to its system network manager", "port", p.name, "manager", p.mgr.Name())
	}
	p.claimed = false
}

// applyWANAddr keeps the leased address on the port. NOPREFIXROUTE keeps
// the subnet out of the main table: routes for mesh clients go into
// meshd's own table (gateway reconciler), and the node's own traffic is
// left as the system set it up.
func (r *portsReconciler) applyWANAddr(p *autoPort, link netlink.Link) bool {
	var want *net.IPNet
	if p.lease != nil {
		a := p.lease.Addr
		want = &a
	}
	changed := false
	if p.addr != nil && (want == nil || p.addr.String() != want.String()) {
		if err := netlink.AddrDel(link, &netlink.Addr{IPNet: p.addr}); err == nil || rtnl.IsNotFound(err) {
			changed = true
		}
		p.addr = nil
	}
	if want != nil {
		have := false
		if addrs, err := netlink.AddrList(link, netlink.FAMILY_V4); err == nil {
			for _, a := range addrs {
				if a.IPNet.String() == want.String() {
					have = true
				}
			}
		}
		if !have {
			if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: want, Flags: unix.IFA_F_NOPREFIXROUTE}); err != nil {
				r.d.log.Warn("cannot put the leased address on the auto WAN port", "port", p.name, "address", want.String(), "err", err)
				return changed
			}
			r.d.log.Info("auto WAN address", "port", p.name, "address", want.String(), "router", p.lease.Router)
			changed = true
		}
		p.addr = want
	}
	return changed
}

// publish writes the port view into facts. Called with r.mu held.
func (r *portsReconciler) publish() {
	d := r.d
	var out []status.Port
	var clients []string
	var wan *autoWAN
	for _, port := range d.cfg.Ports {
		if port.Role == config.PortRoleAuto {
			continue
		}
		l, _ := rtnl.LinkByNameOrNil(port.Name)
		sp := status.Port{Name: port.Name, Configured: port.Role, Role: port.Role}
		if l == nil {
			sp.Role, sp.Why = portMissing, "the interface does not exist"
		} else {
			sp.Carrier = l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
		}
		out = append(out, sp)
	}
	names := make([]string, 0, len(r.ports))
	for name := range r.ports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := r.ports[name]
		sp := status.Port{Name: name, Role: p.role, Why: p.why, Since: p.since}
		if p.claimed && p.mgr != nil {
			sp.Why += "; taken over from " + p.mgr.Name() + " until a DHCP server answers on it or meshd stops"
		}
		if p.listed {
			sp.Configured = config.PortRoleAuto
		}
		if l, _ := rtnl.LinkByNameOrNil(name); l != nil {
			sp.Carrier = l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
		}
		if l := p.lease; l != nil {
			pl := &status.PortLease{Address: l.Addr.String(), Server: l.Server.String(), Expires: l.Expires()}
			if l.Router != nil {
				pl.Router = l.Router.String()
			}
			for _, s := range l.DNS {
				pl.DNS = append(pl.DNS, s.String())
			}
			sp.Lease = pl
		}
		out = append(out, sp)
		switch {
		case p.role == portClient:
			clients = append(clients, name)
		case p.role == portWAN && p.lease != nil && p.addr != nil && wan == nil:
			a := p.lease.Addr
			wan = &autoWAN{Port: name, Addr: &a, Router: p.lease.Router, DNS: p.lease.DNS}
		}
	}
	d.facts.Update(func(f *Facts) { f.Ports, f.AutoClients, f.AutoWAN = out, clients, wan })

	var b strings.Builder
	for _, name := range names {
		if a := r.ports[name].addr; a != nil {
			fmt.Fprintf(&b, "%s %s\n", name, a)
		}
	}
	if b.String() != r.addrsOut {
		if err := os.WriteFile(r.addrsFile(), []byte(b.String()), 0o644); err == nil {
			r.addrsOut = b.String()
		}
	}
	r.writeClaims()
}

// writeClaims records the ports taken over from a system manager. Called
// with r.mu held.
func (r *portsReconciler) writeClaims() {
	names := make([]string, 0, len(r.ports))
	for name := range r.ports {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		if p := r.ports[name]; p.claimed && p.mgr != nil {
			fmt.Fprintf(&b, "%s %s\n", name, p.mgr.Name())
		}
	}
	if b.String() != r.claimsOut {
		if err := os.WriteFile(r.claimsFile(), []byte(b.String()), 0o644); err == nil {
			r.claimsOut = b.String()
		}
	}
}

// candidates are the ports this reconciler looks after, with whether each
// is listed in node.yaml (role auto) or an unlisted Ethernet port.
func (r *portsReconciler) candidates() map[string]bool {
	d := r.d
	out := map[string]bool{}
	for _, name := range d.cfg.AutoPorts() {
		out[name] = true
	}
	links, err := netlink.LinkList()
	if err != nil {
		return out
	}
	for _, l := range links {
		name := l.Attrs().Name
		if _, ok := out[name]; ok || d.cfg.PortRole(name) != "" || ownInterface(name) {
			continue
		}
		if isEthernetPort(d.opts.SysRoot, l) {
			out[name] = false
		}
	}
	for _, m := range r.mgrs {
		if cl, ok := m.(candidateLister); ok {
			for _, name := range cl.Candidates() {
				if _, ok := out[name]; !ok && d.cfg.PortRole(name) == "" && !ownInterface(name) {
					out[name] = false
				}
			}
		}
	}
	return out
}

func ownInterface(name string) bool {
	switch name {
	case "lo", BridgeIfname, BatIfname, AnycastIfname, APIfname:
		return true
	}
	// Any mesh* or ap* name: Trellis's own (mesh0 … mesh9, ap0) and those an
	// older release or a test left behind; never a port to take over.
	return strings.HasPrefix(name, "mesh") || strings.HasPrefix(name, "ap")
}

// isEthernetPort: a physical, wired Ethernet interface (not wireless, not
// virtual).
func isEthernetPort(sysRoot string, l netlink.Link) bool {
	if l.Type() != "device" || l.Attrs().EncapType != "ether" {
		return false
	}
	base := filepath.Join(sysRoot, "class/net", l.Attrs().Name)
	if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
		return false
	}
	for _, wifi := range []string{"wireless", "phy80211"} {
		if _, err := os.Stat(filepath.Join(base, wifi)); err == nil {
			return false
		}
	}
	return true
}

// networkdState returns systemd-networkd's ADMIN_STATE for a link, "" when
// networkd has no record of it.
func networkdState(ifindex int) string {
	f, err := os.Open(fmt.Sprintf("/run/systemd/netif/links/%d", ifindex))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "ADMIN_STATE="); ok {
			return v
		}
	}
	return ""
}

// ifupdownConfigures reports whether /etc/network/interfaces (or its .d
// directory) mentions the interface.
func ifupdownConfigures(name string) bool {
	files := []string{"/etc/network/interfaces"}
	if more, err := filepath.Glob("/etc/network/interfaces.d/*"); err == nil {
		files = append(files, more...)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && (f[0] == "iface" || f[0] == "auto" || f[0] == "allow-hotplug") && slices.Contains(f[1:], name) {
				return true
			}
		}
	}
	return false
}

// processRunning reports whether a process with this command name exists.
func processRunning(comm string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		if b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm")); err == nil && strings.TrimSpace(string(b)) == comm {
			return true
		}
	}
	return false
}

func (r *portsReconciler) Teardown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, p := range r.ports {
		r.release(p)
		r.giveBack(p, true)
		delete(r.ports, name)
	}
	r.writeClaims()
	r.d.facts.Update(func(f *Facts) { f.Ports, f.AutoClients, f.AutoWAN = nil, nil, nil })
	return nil
}
