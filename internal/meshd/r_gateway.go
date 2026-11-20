package meshd

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"meridian/trellis/internal/batadv"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/nft"
	"meridian/trellis/internal/nodestate"
	"meridian/trellis/internal/probe"
	"meridian/trellis/internal/proc"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/rtnl"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/sysctl"
)

// Policy routing layout (architecture §8.4, §8.5). meshd owns IPv4 rules
// with priorities 7200–7399 and route tables 7399–7654.
const (
	ruleLo, ruleHi = 7200, 7399
	prioPin        = 7200 // + index: fwmark of a pinned connection → that gateway
	prioFromEUD    = 7300 // iif anycast0: this node's clients
	prioMeshToMesh = 7301 // iif br-mesh, to the mesh prefix: main (see applyRouting)
	prioAutoWANOif = 7302 // oif <auto WAN>: sockets bound to it (prober, dnsmasq) use its table
	prioFromMesh   = 7303 // iif br-mesh: other nodes' clients, routed to us as their gateway
	tableNone      = 7399 // unreachable default: never invent a route (S6)
	// prioLast comes after the main (32766) and default (32767) rules, so
	// it is consulted only when the system has no route at all; the main
	// table, when it has a route, always wins. It takes this node's own
	// traffic out:
	//   - through its auto WAN, when it has one. The kernel's reverse-path
	//     check on the auto WAN (for ARP too) needs a route back to the
	//     sender; without one it drops the router's ARP requests.
	//   - otherwise through the gateway its clients use, so that a node
	//     with no uplink of its own still reaches the internet itself
	//     (package updates, NTP, anything it fetches).
	prioLast    = 32800
	tableGWBase = 7400 // + node ID: default via that gateway
	tableGWHi   = tableGWBase + 254
	markBase    = 0x7300
	markMask    = nft.MarkMask
)

// Selection hysteresis (architecture §8.4): leave a working gateway only for
// a much better one, and only when that lasts.
const (
	switchFactor = 1.5
	switchMinGap = 2.0 // Mbit/s
	switchAfter  = 10 * time.Second
	monitorEvery = 250 * time.Millisecond
	confirmPing  = 400 * time.Millisecond
	gwSelClass   = 20 // batman-adv BATMAN_V: 2 Mbit/s
)

// Unicast reachability of the gateway in use (architecture §8.3). batman-adv
// hears a gateway by its broadcast routing messages, which can keep flowing
// while unicast to it is lost: after a radio reset the two ends of a link
// can hold different pairwise keys, so frames are acknowledged by the radio
// and dropped when decrypted. Every reachEvery the gateway in use is pinged
// over the mesh; after reachFails misses in a row it is not usable until a
// retry (every reachRetry) is answered.
const (
	reachEvery   = 2 * time.Second
	reachTimeout = time.Second
	reachFails   = 3
	reachRetry   = 15 * time.Second
	reachForget  = 10 * time.Minute // a gateway gone from batman-adv's list this long is forgotten
)

// unreachGW is a gateway that stopped answering unicast pings.
type unreachGW struct {
	addr     string
	callsign string
	since    time.Time
	retryAt  time.Time
	listedAt time.Time // last seen in batman-adv's gateway list
}

// gwChoice is a usable gateway.
type gwChoice struct {
	Self     bool
	MAC      string // identity MAC (from gossip)
	Orig     string
	ID       int
	Callsign string
	Addr     string
	Tput     float64 // Mbit/s, batman-adv's path estimate
	idSource string  // how it got its node ID (gossip), for sharing conflicts
	cand     int     // its index in the candidate list
}

func (c *gwChoice) key() string {
	if c == nil {
		return "none"
	}
	if c.Self {
		return "self"
	}
	return c.MAC
}

type gatewayReconciler struct {
	d *Daemon

	// The system's own DNS while the node has no uplink (r_hostdns.go).
	hostRoot    string // "/" (tests: a directory)
	hostDNSKind string // resolved, networkmanager, none; "" until looked
	hostDNSOn   string // the server applied, "" when none
	hostDNSAt   time.Time

	evalMu    sync.Mutex // one evaluation at a time (loop and monitor)
	mu        sync.Mutex
	prober    *probe.Prober
	proberKey string
	wan       string
	healthy   bool
	// portalOnly: the WAN is behind a login page. It is not shared with the
	// mesh, but this node's own clients use it while no other gateway is
	// usable, so that someone can reach the login page and sign in.
	portalOnly  bool
	web         *webChecker
	webKey      string
	carrier     string                                                                             // the WAN and its carrier when last seen
	webCheckFn  func(ctx context.Context, ifname string, urls, resolvers []string) probe.WebResult // tests
	cands       []status.GatewayCandidate
	usable      []*gwChoice
	selected    *gwChoice
	selSince    time.Time
	switches    int
	betterMAC   string
	betterSince time.Time
	signature   string

	nftPath   string
	routing   string          // nft script in force
	neighs    map[string]bool // gateway addresses with a pinned neighbour entry
	gwModeSet string
	fwdOrig   string
	monStop   chan struct{}
	monWG     sync.WaitGroup // the monitor and watchReach goroutines
	stdWarned bool

	reachMu   sync.Mutex
	unreach   map[string]*unreachGW // by identity MAC
	reachMAC  string                // the gateway the miss count is for
	reachMiss int
}

func newGatewayReconciler(d *Daemon) *gatewayReconciler { return &gatewayReconciler{d: d} }

func (g *gatewayReconciler) Name() string { return "gateway" }

func (g *gatewayReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := g.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var batIndex int
	var anycastUp bool
	d.facts.Read(func(f *Facts) { batIndex, anycastUp = f.BatIfindex, f.Anycast.Present })
	cfg := d.cfg.Gateway
	st := status.GatewayState{Mode: cfg.Mode, Role: "client", Candidates: []status.GatewayCandidate{}, DNS: []string{}}

	if batIndex == 0 || d.bat == nil || !anycastUp {
		// The client network is not up; nothing to route.
		d.facts.Update(func(f *Facts) { f.Gateway, f.GossipGW, f.DNSUpstream = st, nil, nil })
		return res, nil
	}

	// WAN and its prober.
	wan, why := g.findWAN()
	var auto *autoWAN
	d.facts.Read(func(f *Facts) {
		if f.AutoWAN != nil && f.AutoWAN.Port == wan {
			auto = f.AutoWAN
		}
	})
	if cfg.Mode == config.GatewayAuto && wan == "" && why != "" {
		warn("info", "gateway-no-wan", "not sharing an uplink: %s", why)
	}
	if err := g.ensureProber(wan, cfg); err != nil {
		warn("warning", "gateway-probe", "cannot probe the WAN on %s: %v", wan, err)
	}
	g.ensureWebCheck(wan, cfg, auto)
	var ps probe.State
	if g.prober != nil {
		ps = g.prober.State()
	}
	// A WAN without carrier is down now, not after three failed probes.
	carrier := false
	if wan != "" {
		if l, err := netlink.LinkByName(wan); err == nil {
			carrier = l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0 && l.Attrs().Flags&net.FlagUp != 0
		}
		if !carrier && ps.Healthy {
			ps.LastError = "link down"
		}
	}
	g.mu.Lock()
	if c := fmt.Sprintf("%s %v", wan, carrier); c != g.carrier {
		if g.carrier != "" && g.web != nil {
			g.web.reset()
		}
		g.carrier = c
	}
	g.mu.Unlock()
	web, webOn, portal, webWaiting := g.webState()
	g.mu.Lock()
	g.wan = wan
	sharing := cfg.Mode == config.GatewayAuto && wan != "" && carrier
	g.healthy = sharing && ps.Healthy && !portal && !webWaiting
	g.portalOnly = sharing && portal
	healthy := g.healthy
	g.mu.Unlock()

	var gw *gossip.GW
	if cfg.Mode == config.GatewayAuto && wan != "" {
		gw = &gossip.GW{Healthy: healthy, DownKbps: uint32(cfg.DownMbit) * 1000, UpKbps: uint32(cfg.UpMbit) * 1000,
			RTTms: int(ps.LastRTT.Milliseconds()), LossPct: ps.LossPct}
	}
	switch {
	case portal && sharing:
		warn("warning", "wan-portal", "Uplink %s behind captive portal (%s): not shared until sign-in. Local clients reach the portal while no other gateway is available. Override: gateway.web_check: off.", wan, web.Detail)
	case wan != "" && ps.Known && !ps.Healthy:
		warn("warning", "wan-unhealthy", "Uplink %s down (no reply from %s): not shared", wan, strings.Join(cfg.ProbeTargets, ", "))
	case webOn && ps.Healthy && web.Verdict == probe.WebNone:
		warn("info", "wan-web-none", "Uplink %s: ping OK, web check failed (%s). Shared anyway", wan, web.Detail)
	}

	// batman-adv gateway mode: announce while healthy, else be a client.
	if err := g.ensureGWMode(batIndex, healthy, cfg); err != nil {
		warn("warning", "gateway-mode", "cannot set the batman-adv gateway mode: %v", err)
	}

	// Choose, then apply.
	if g.evaluate() {
		res.Changed = true
	}
	g.ensureMonitor()
	g.mu.Lock()
	sel, usable, cands := g.selected, append([]*gwChoice{}, g.usable...), append([]status.GatewayCandidate{}, g.cands...)
	selSince, switches := g.selSince, g.switches
	g.mu.Unlock()

	selfUp := healthy || sel != nil && sel.Self
	if ch, err := g.applyRouting(ctx, sel, usable, selfUp, wan, auto); err != nil {
		warn("error", "gateway-routing", "cannot apply gateway routing: %v", err)
		res.RequeueAfter = 5 * time.Second
	} else if ch {
		res.Changed = true
	}
	// No uplink of its own: the node's own traffic leaves through the
	// gateway in use (prioLast), and its DNS goes to its own dnsmasq.
	// Not before dnsmasq forwards to the gateway in use: while it still
	// forwards to the system's resolver, pointing that resolver at
	// dnsmasq would make a loop. dnsmasq takes the new upstream later in
	// this pass (the dhcp reconciler); the next pass applies.
	if server, err := anycastAddress(d.profile.Prefix); err == nil {
		want := sel != nil && !sel.Self && auto == nil && !mainHasDefault()
		var up []string
		d.facts.Read(func(f *Facts) { up = f.DNSUpstream })
		if want && g.hostDNSOn == "" && !forwardsToMesh(up) {
			res.RequeueAfter = 2 * time.Second
		} else if g.applyHostDNS(ctx, want, server, warn) {
			res.Changed = true
		}
	}

	dns := []string{}
	switch {
	case sel != nil && sel.Self && auto != nil:
		// The lease's resolvers, reached through the auto WAN (dnsmasq
		// binds to the port; the oif rule routes it).
		for _, s := range auto.DNS {
			dns = append(dns, s.String()+"@"+auto.Port)
		}
		if len(dns) == 0 && auto.Router != nil {
			dns = []string{auto.Router.String() + "@" + auto.Port}
		}
	case sel != nil && sel.Self:
		// Never the node's own mesh DNS: dnsmasq would forward to itself
		// (the system's resolver may still point there for a moment after
		// an uplink comes back).
		dns = systemResolvers(anycastIP(d.profile.Prefix))
	case sel != nil:
		dns = []string{sel.Addr}
	}

	st.WAN, st.WANHealthy, st.WANKnown, st.WANSince = wan, ps.Healthy, ps.Known, ps.Since
	st.RTTms, st.LossPct, st.ProbeError = float64(ps.LastRTT.Microseconds())/1000, ps.LossPct, ps.LastError
	if webOn {
		st.WebCheck, st.WebCheckDetail = web.Verdict, web.Detail
		if web.Verdict == "" {
			st.WebCheck = "checking"
		}
		if !web.At.IsZero() {
			st.WebCheckAt = web.At
		}
	}
	if wan != "" {
		st.ProbeTargets = cfg.ProbeTargets
	}
	if healthy {
		st.Role = "gateway"
		st.Announced = fmt.Sprintf("%d/%d Mbit/s", cfg.DownMbit, cfg.UpMbit)
	}
	if cfg.Mode == config.GatewayOff {
		st.Role = "client (sharing off)"
	}
	if g.portalOnly {
		st.Role = "client (uplink behind a login page)"
	}
	if sel != nil {
		st.Selected = &status.SelectedGateway{Self: sel.Self, Callsign: sel.Callsign, NodeID: sel.ID, Address: sel.Addr, ThroughputMbps: sel.Tput, Since: selSince}
	} else {
		warn("warning", "no-gateway", "No gateway in the mesh: mesh-only access, external DNS unavailable")
	}
	st.Candidates, st.DNS, st.Switches = cands, dns, switches
	d.facts.Update(func(f *Facts) { f.Gateway, f.GossipGW, f.DNSUpstream = st, gw, dns })
	return res, nil
}

// findWAN picks the uplink: configured, a port with role wan, or the
// interface of the system's own default route.
func (g *gatewayReconciler) findWAN() (string, string) {
	cfg := g.d.cfg
	if cfg.Gateway.Mode == config.GatewayOff {
		return "", ""
	}
	name := cfg.Gateway.WAN
	if name == "" {
		if ports := cfg.WANPorts(); len(ports) > 0 {
			name = ports[0]
		}
	}
	if name != "" {
		if l, _ := rtnl.LinkByNameOrNil(name); l == nil {
			return "", fmt.Sprintf("WAN interface %s does not exist", name)
		}
		return name, ""
	}
	// An auto port holding a lease: plugged in for this purpose, so it
	// wins over the system's own uplink.
	var auto *autoWAN
	g.d.facts.Read(func(f *Facts) { auto = f.AutoWAN })
	if auto != nil {
		return auto.Port, ""
	}
	exclude := map[string]bool{BridgeIfname: true, BatIfname: true, AnycastIfname: true, APIfname: true, "lo": true}
	for i := 0; i < config.MaxMeshLinks; i++ {
		exclude[meshIfname(i)] = true
	}
	_, link, err := rtnl.MainDefault(exclude)
	if err != nil {
		return "", err.Error()
	}
	if link == nil {
		return "", "this node has no default route of its own"
	}
	return link.Attrs().Name, ""
}

func (g *gatewayReconciler) ensureProber(wan string, cfg config.GatewayConfig) error {
	key := ""
	if wan != "" {
		key = fmt.Sprintf("%s|%s|%d", wan, strings.Join(cfg.ProbeTargets, ","), cfg.ProbeIntervalMS)
	}
	g.mu.Lock()
	same := key == g.proberKey
	g.mu.Unlock()
	if same {
		return nil
	}
	g.stopProber()
	if key == "" {
		return nil
	}
	p := &probe.Prober{Ifname: wan, Targets: cfg.ProbeTargets, Interval: time.Duration(cfg.ProbeIntervalMS) * time.Millisecond,
		OnChange: func(s probe.State) {
			if s.Healthy {
				g.d.log.Info("WAN healthy", "interface", wan, "rtt", s.LastRTT.Round(time.Millisecond))
			} else {
				g.d.log.Warn("WAN unhealthy", "interface", wan, "err", s.LastError)
			}
			g.mu.Lock()
			if g.web != nil && s.Healthy {
				g.web.reset()
			}
			g.mu.Unlock()
			g.d.Kick("wan health changed")
		}}
	if err := p.Start(); err != nil {
		return err
	}
	g.mu.Lock()
	g.prober, g.proberKey = p, key
	g.mu.Unlock()
	g.d.log.Info("probing the WAN", "interface", wan, "targets", cfg.ProbeTargets, "interval_ms", cfg.ProbeIntervalMS)
	return nil
}

func (g *gatewayReconciler) stopProber() {
	g.mu.Lock()
	p := g.prober
	g.prober, g.proberKey = nil, ""
	g.mu.Unlock()
	if p != nil {
		p.Stop()
	}
}

func (g *gatewayReconciler) ensureGWMode(batIndex int, healthy bool, cfg config.GatewayConfig) error {
	var want batadv.Settings
	key := ""
	if healthy {
		mode := uint8(batadv.BATADV_GW_MODE_SERVER)
		down, up := uint32(cfg.DownMbit)*10, uint32(cfg.UpMbit)*10
		want = batadv.Settings{GWMode: &mode, GWBandwidthDown: &down, GWBandwidthUp: &up}
		key = fmt.Sprintf("server %d/%d", down, up)
	} else {
		mode := uint8(batadv.BATADV_GW_MODE_CLIENT)
		sel := uint32(gwSelClass)
		want = batadv.Settings{GWMode: &mode, GWSelClass: &sel}
		key = "client"
	}
	if key == g.gwModeSet {
		return nil
	}
	if err := g.d.bat.SetMesh(batIndex, want); err != nil {
		return err
	}
	if healthy {
		g.d.log.Info("announcing this node as an internet gateway", "bandwidth", fmt.Sprintf("%d/%d Mbit/s", cfg.DownMbit, cfg.UpMbit))
	} else if g.gwModeSet != "" {
		g.d.log.Info("no longer a gateway; using other nodes' gateways")
	}
	g.gwModeSet = key
	return nil
}

// evaluate refreshes the candidate list and the selection. It reports
// whether the selection or the usable set changed.
func (g *gatewayReconciler) evaluate() bool {
	g.evalMu.Lock()
	defer g.evalMu.Unlock()
	d := g.d
	var batIndex int
	d.facts.Read(func(f *Facts) { batIndex = f.BatIfindex })
	cands, usable := g.candidates(batIndex)

	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	prev := g.selected
	var next *gwChoice
	switch {
	case g.healthy || g.portalOnly && len(usable) == 0:
		node, _ := d.conf() // evaluate also runs on the gateway monitor goroutine
		next = &gwChoice{Self: true, ID: d.identity().NodeID, Callsign: node.Callsign, Addr: strings.Split(d.facts.bridgeAddress(), "/")[0]}
		g.betterMAC = ""
	case len(usable) == 0:
		next = nil
	default:
		best := usable[0]
		var cur *gwChoice
		if prev != nil && !prev.Self {
			for _, u := range usable {
				if u.MAC == prev.MAC {
					cur = u
				}
			}
		}
		if cur == nil {
			next = best
			g.betterMAC = ""
		} else {
			next = cur
			if best.MAC != cur.MAC && best.Tput > cur.Tput*switchFactor && best.Tput-cur.Tput >= switchMinGap {
				if g.betterMAC != best.MAC {
					g.betterMAC, g.betterSince = best.MAC, now
				} else if now.Sub(g.betterSince) >= switchAfter {
					next = best
					g.betterMAC = ""
				}
			} else {
				g.betterMAC = ""
			}
		}
	}
	if prev.key() != next.key() {
		g.selSince = now
		if prev != nil || next != nil {
			g.switches++
		}
		switch {
		case next == nil:
			d.log.Warn("no usable internet gateway", "was", describeChoice(prev))
		case prev == nil:
			d.log.Info("selected internet gateway", "gateway", describeChoice(next))
		default:
			d.log.Info("switched internet gateway", "from", describeChoice(prev), "to", describeChoice(next))
		}
	}
	g.selected, g.usable, g.cands = next, usable, cands
	sig := next.key()
	for _, u := range usable {
		sig += "|" + u.MAC + "@" + u.Addr
	}
	changed := sig != g.signature
	g.signature = sig
	return changed
}

func describeChoice(c *gwChoice) string {
	switch {
	case c == nil:
		return "none"
	case c.Self:
		return "this node"
	}
	return fmt.Sprintf("%s (%s)", c.Callsign, c.Addr)
}

// candidates lists batman-adv's gateways and which can be used: known from
// gossip (for the address), WAN healthy as gossip reports it, and recently
// heard by batman-adv (architecture §8.3–8.4). Usable ones are sorted best
// first.
func (g *gatewayReconciler) candidates(batIndex int) ([]status.GatewayCandidate, []*gwChoice) {
	d := g.d
	out := []status.GatewayCandidate{}
	var usable []*gwChoice
	if batIndex == 0 || d.bat == nil {
		return out, nil
	}
	gws, err := d.bat.Gateways(batIndex)
	if err != nil {
		return out, nil
	}
	lastSeen := map[string]time.Duration{}
	if origs, err := d.bat.Originators(batIndex); err == nil {
		for _, o := range origs {
			if o.Best {
				lastSeen[o.Address.String()] = o.LastSeen
			}
		}
	}
	// Two missed originator messages (plus jitter) make a gateway suspect;
	// the confirmation ping guards against a lost message or two.
	_, profile := d.conf() // candidates also runs on the gateway monitor goroutine
	stale := 2*time.Duration(profile.Batman.OrigIntervalMS)*time.Millisecond + 200*time.Millisecond
	if stale < time.Second {
		stale = time.Second
	}
	g.mu.Lock()
	cur := g.selected
	g.mu.Unlock()
	seen := map[string]bool{}
	for _, gw := range gws {
		orig := gw.Address.String()
		if seen[orig] {
			continue
		}
		seen[orig] = true
		c := status.GatewayCandidate{Originator: orig, ThroughputMbps: gw.Throughput.Mbps()}
		ls, known := lastSeen[orig]
		c.LastSeenMS = uint32(ls.Milliseconds())
		peer, ok := d.peers.ByOriginator(orig)
		switch {
		case !ok:
			c.Why = "no gossip from this node yet, so its address is unknown"
		case peer.Record.GW == nil || !peer.Record.GW.Healthy:
			c.Callsign, c.NodeID, c.Address = peer.Record.Callsign, peer.Record.ID, peer.Record.Addr
			c.Why = "its WAN is unhealthy"
		default:
			c.Callsign, c.NodeID, c.Address = peer.Record.Callsign, peer.Record.ID, peer.Record.Addr
			if !known || ls > stale {
				// Quiet in batman-adv: a dead node or a split mesh. The
				// current gateway gets one unicast ping before it is
				// dropped; others are simply skipped.
				if cur != nil && cur.MAC == peer.Record.MAC && probe.Ping(BridgeIfname, peer.Record.Addr, confirmPing) {
					break
				}
				c.Why = fmt.Sprintf("not heard for %s", ls.Round(100*time.Millisecond))
			}
			if c.Why == "" {
				if u := g.unreachable(peer.Record.MAC); u != nil {
					c.Why = fmt.Sprintf("heard by batman-adv, but no answer to unicast pings for %s (a broken radio link?); retrying every %s",
						time.Since(u.since).Round(time.Second), reachRetry)
				}
			}
		}
		if c.Why == "" {
			c.Usable = true
			usable = append(usable, &gwChoice{MAC: strings.ToLower(peer.Record.MAC), Orig: orig, ID: peer.Record.ID, Callsign: peer.Record.Callsign,
				Addr: peer.Record.Addr, Tput: c.ThroughputMbps, idSource: peer.Record.IDSource, cand: len(out)})
		}
		out = append(out, c)
	}
	usable = dropSharedIDs(usable, out, d.identity())
	sort.SliceStable(usable, func(i, j int) bool {
		if usable[i].Tput != usable[j].Tput {
			return usable[i].Tput > usable[j].Tput
		}
		return usable[i].ID < usable[j].ID
	})
	return out, usable
}

// dropSharedIDs keeps one gateway per node ID. A gateway's routing table
// and connection mark come from its node ID, so two that share one (before
// gossip settles the conflict, architecture §7.3) would take turns in one
// table and pin each other's connections. The one that keeps the ID, by
// the rule both nodes apply, stays usable; a gateway with this node's own
// ID is left out while the conflict lasts. The candidates say why.
func dropSharedIDs(usable []*gwChoice, cands []status.GatewayCandidate, me *nodestate.Identity) []*gwChoice {
	byID := map[int]*gwChoice{}
	drop := map[*gwChoice]string{}
	for _, u := range usable {
		if me != nil && u.ID == me.NodeID {
			drop[u] = fmt.Sprintf("it uses this node's ID %d; not used until the conflict is settled", u.ID)
			continue
		}
		other := byID[u.ID]
		switch {
		case other == nil:
			byID[u.ID] = u
		case nodestate.KeepsID(u.idSource, u.MAC, other.idSource, other.MAC):
			drop[other] = fmt.Sprintf("it shares node ID %d with %s; not used until the conflict is settled", u.ID, u.Callsign)
			byID[u.ID] = u
		default:
			drop[u] = fmt.Sprintf("it shares node ID %d with %s; not used until the conflict is settled", u.ID, other.Callsign)
		}
	}
	if len(drop) == 0 {
		return usable
	}
	kept := usable[:0:0]
	for _, u := range usable {
		if why, ok := drop[u]; ok {
			cands[u.cand].Usable, cands[u.cand].Why = false, why
			continue
		}
		kept = append(kept, u)
	}
	return kept
}

// ensureMonitor starts the fast evaluation loop: batman-adv tables are read
// every half second, and the reconcile loop is kicked only on a change, so
// failover does not wait for the slow timer.
func (g *gatewayReconciler) ensureMonitor() {
	if g.monStop != nil {
		return
	}
	g.monStop = make(chan struct{})
	stop := g.monStop
	g.monWG.Add(2)
	go func() {
		defer g.monWG.Done()
		g.watchReach(stop)
	}()
	go func() {
		defer g.monWG.Done()
		t := time.NewTicker(monitorEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if g.evaluate() {
					g.d.Kick("gateway selection changed")
				}
			}
		}
	}()
}

// unreachable returns the record for a gateway that stopped answering
// unicast pings, or nil. It also notes that batman-adv still lists it.
func (g *gatewayReconciler) unreachable(mac string) *unreachGW {
	g.reachMu.Lock()
	defer g.reachMu.Unlock()
	u := g.unreach[strings.ToLower(mac)]
	if u != nil {
		u.listedAt = time.Now()
		c := *u
		return &c
	}
	return nil
}

// watchReach pings the gateway in use every reachEvery, and retries the
// gateways that stopped answering. It runs beside the monitor, so a ping's
// wait never delays the batman-adv checks.
func (g *gatewayReconciler) watchReach(stop chan struct{}) {
	d := g.d
	t := time.NewTicker(reachEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		changed := false
		g.mu.Lock()
		sel := g.selected
		g.mu.Unlock()
		if sel != nil && !sel.Self && sel.Addr != "" {
			ok := probe.Ping(BridgeIfname, sel.Addr, reachTimeout)
			g.reachMu.Lock()
			if g.reachMAC != sel.MAC {
				g.reachMAC, g.reachMiss = sel.MAC, 0
			}
			if ok {
				g.reachMiss = 0
			} else {
				g.reachMiss++
			}
			if g.reachMiss >= reachFails {
				if g.unreach == nil {
					g.unreach = map[string]*unreachGW{}
				}
				now := time.Now()
				g.unreach[sel.MAC] = &unreachGW{addr: sel.Addr, callsign: sel.Callsign, since: now, retryAt: now.Add(reachRetry), listedAt: now}
				g.reachMiss = 0
				changed = true
				d.log.Warn("internet gateway is heard over the mesh but does not answer unicast pings; not using it",
					"gateway", describeChoice(sel), "missed", reachFails, "every", reachEvery)
			}
			g.reachMu.Unlock()
		} else {
			g.reachMu.Lock()
			g.reachMAC, g.reachMiss = "", 0
			g.reachMu.Unlock()
		}
		// Retries, without holding the lock while a ping waits.
		type retry struct{ mac, addr, callsign string }
		var due []retry
		now := time.Now()
		g.reachMu.Lock()
		for mac, u := range g.unreach {
			switch {
			case now.Sub(u.listedAt) > reachForget:
				delete(g.unreach, mac)
			case !now.Before(u.retryAt):
				due = append(due, retry{mac, u.addr, u.callsign})
				u.retryAt = now.Add(reachRetry)
			}
		}
		g.reachMu.Unlock()
		for _, r := range due {
			if stopped(stop) {
				return // Teardown waits for at most the ping in flight
			}
			if !probe.Ping(BridgeIfname, r.addr, reachTimeout) {
				continue
			}
			g.reachMu.Lock()
			if u := g.unreach[r.mac]; u != nil {
				d.log.Info("internet gateway answers unicast pings again", "gateway", fmt.Sprintf("%s (%s)", r.callsign, r.addr),
					"after", time.Since(u.since).Round(time.Second))
				delete(g.unreach, r.mac)
				changed = true
			}
			g.reachMu.Unlock()
		}
		if changed && g.evaluate() {
			d.Kick("gateway reachability changed")
		}
	}
}

// applyRouting installs the rules, tables, marks and NAT for a selection.
func (g *gatewayReconciler) applyRouting(ctx context.Context, sel *gwChoice, usable []*gwChoice, selfHealthy bool, wan string, auto *autoWAN) (bool, error) {
	d := g.d
	changed := false
	mainT := unix.RT_TABLE_MAIN
	// selfT carries this node's own uplink for mesh clients: the main table
	// for the system's uplink, or meshd's own table for an auto WAN, whose
	// lease meshd holds and whose routes never enter the main table.
	selfT := mainT
	selfID := d.identity().NodeID

	// Tables.
	keep := map[int]bool{tableNone: true}
	if r, err := rtnl.TableDefault(tableNone); err != nil {
		return changed, err
	} else if r == nil || r.Type != unix.RTN_UNREACHABLE {
		rt := &netlink.Route{Table: tableNone, Type: unix.RTN_UNREACHABLE, Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}}
		if err := netlink.RouteReplace(rt); err != nil {
			return changed, fmt.Errorf("unreachable default in table %d: %w", tableNone, err)
		}
		changed = true
	}
	br, err := netlink.LinkByName(BridgeIfname)
	if err != nil {
		return changed, err
	}
	for _, u := range usable {
		t := tableGWBase + u.ID
		keep[t] = true
		gwIP := net.ParseIP(u.Addr)
		if gwIP == nil {
			continue
		}
		r, err := rtnl.TableDefault(t)
		if err != nil {
			return changed, err
		}
		if r == nil || !r.Gw.Equal(gwIP) || r.LinkIndex != br.Attrs().Index {
			rt := &netlink.Route{Table: t, Gw: gwIP, LinkIndex: br.Attrs().Index, Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}}
			if err := netlink.RouteReplace(rt); err != nil {
				return changed, fmt.Errorf("default via %s in table %d: %w", u.Addr, t, err)
			}
			changed = true
		}
	}
	if auto != nil {
		selfT = tableGWBase + selfID
		keep[selfT] = true
		ch, err := ensureAutoWANTable(selfT, auto)
		if err != nil {
			return changed, err
		}
		changed = changed || ch
	}
	if ch, err := rtnl.FlushTables(tableNone, tableGWHi, keep); err != nil {
		return changed, err
	} else if ch {
		changed = true
	}

	// Gateways' link-layer addresses come from gossip: pin them, so the
	// path to a gateway never waits on (or depends on) a broadcast ARP
	// across the mesh, and a failover needs no resolution at all.
	want := map[string]bool{}
	for _, u := range usable {
		ip, mac := net.ParseIP(u.Addr), mustMAC(u.MAC)
		if ip == nil || mac == nil {
			continue
		}
		want[u.Addr] = true
		n := &netlink.Neigh{LinkIndex: br.Attrs().Index, Family: netlink.FAMILY_V4, State: netlink.NUD_PERMANENT, IP: ip, HardwareAddr: mac}
		if err := netlink.NeighSet(n); err != nil {
			d.log.Warn("cannot pin the gateway's neighbour entry", "gateway", u.Addr, "err", err)
			continue
		}
		if !g.neighs[u.Addr] {
			changed = true
		}
	}
	for addr := range g.neighs {
		if !want[addr] {
			_ = netlink.NeighDel(&netlink.Neigh{LinkIndex: br.Attrs().Index, Family: netlink.FAMILY_V4, IP: net.ParseIP(addr)})
			changed = true
		}
	}
	g.neighs = want

	// Rules.
	mask := uint32(markMask)
	var rules []netlink.Rule
	pin := func(id, table int) {
		r := *netlink.NewRule()
		r.Priority, r.Table, r.Mark, r.Mask = prioPin+len(rules), table, uint32(markBase|id), &mask
		rules = append(rules, r)
	}
	if selfHealthy {
		pin(selfID, selfT)
	}
	ids := []int{}
	for _, u := range usable {
		ids = append(ids, u.ID)
	}
	sort.Ints(ids)
	for _, id := range ids {
		if len(rules) < prioFromEUD-prioPin {
			pin(id, tableGWBase+id)
		}
	}
	iif := func(prio int, name string, table int) {
		r := *netlink.NewRule()
		r.Priority, r.IifName, r.Table = prio, name, table
		rules = append(rules, r)
	}
	switch {
	case sel == nil:
		iif(prioFromEUD, AnycastIfname, tableNone)
		iif(prioFromMesh, BridgeIfname, tableNone)
	case sel.Self:
		iif(prioFromEUD, AnycastIfname, selfT)
		iif(prioFromMesh, BridgeIfname, selfT)
	default:
		iif(prioFromEUD, AnycastIfname, tableGWBase+sel.ID)
		iif(prioFromMesh, BridgeIfname, tableNone)
	}
	// The reverse-path check of a client's packet forwarded from anycast0
	// to br-mesh looks the source up as if it arrived on the outgoing
	// interface, br-mesh (Linux 6.1; 7.x does not). On a node that is not a
	// gateway, "iif br-mesh" leads to the unreachable table, so the check
	// failed and every client packet bound for another node's gateway was
	// dropped (IPReversePathFilter). Mesh destinations from br-mesh use
	// main, before that rule.
	if _, prefix, err := net.ParseCIDR(d.profile.Prefix); err == nil {
		r := *netlink.NewRule()
		r.Priority, r.IifName, r.Dst, r.Table = prioMeshToMesh, BridgeIfname, prefix, unix.RT_TABLE_MAIN
		rules = append(rules, r)
	}
	if auto != nil {
		// Always, healthy or not: the prober must reach the internet to
		// find out.
		r := *netlink.NewRule()
		r.Priority, r.OifName, r.Table = prioAutoWANOif, auto.Port, selfT
		rules = append(rules, r)
	}
	if ch, err := rtnl.EnsureRules(ruleLo, ruleHi, rules); err != nil {
		return changed, err
	} else if ch {
		changed = true
	}
	var last []netlink.Rule
	switch {
	case auto != nil:
		r := *netlink.NewRule()
		r.Priority, r.Table = prioLast, selfT
		last = append(last, r)
	case sel != nil && !sel.Self:
		r := *netlink.NewRule()
		r.Priority, r.Table = prioLast, tableGWBase+sel.ID
		last = append(last, r)
	}
	if ch, err := rtnl.EnsureRules(prioLast, prioLast, last); err != nil {
		return changed, err
	} else if ch {
		changed = true
	}

	// Forwarding: clients' traffic is routed by every node.
	if g.fwdOrig == "" {
		if v, err := sysctl.Get("net.ipv4.ip_forward"); err == nil {
			g.fwdOrig = v
		}
	}
	if ch, err := sysctl.Set("net.ipv4.ip_forward", "1"); err != nil {
		return changed, err
	} else if ch {
		d.log.Info("enabled IPv4 forwarding")
		changed = true
	}

	// Marks and NAT.
	if g.nftPath == "" {
		p, err := proc.LookPath("nft")
		if err != nil {
			return changed, fmt.Errorf("nft not installed")
		}
		g.nftPath = p
	}
	var markV uint32
	if sel != nil {
		id := sel.ID
		if sel.Self {
			id = d.identity().NodeID
		}
		markV = uint32(markBase | id)
	}
	script := nft.Routing{AnycastIf: AnycastIfname, Mark: markV, NAT: selfHealthy, WAN: wan, Prefix: d.profile.Prefix,
		UntrackSYN: sel != nil && !sel.Self}.Render()
	if script != g.routing {
		if err := nft.Apply(ctx, g.nftPath, script); err != nil {
			return changed, err
		}
		g.routing = script
		changed = true
		if selfHealthy {
			d.log.Info("NAT on", "wan", wan, "source", d.profile.Prefix)
		}
		// One-way TCP through non-gateway nodes (replies return directly
		// over layer 2): conntrack must pick connections up from their
		// handshake ACK (UntrackSYN) and not check windows it never sees.
		for _, key := range []string{"net.netfilter.nf_conntrack_tcp_be_liberal", "net.netfilter.nf_conntrack_tcp_loose"} {
			if _, err := sysctl.Set(key, "1"); err != nil && !g.stdWarned {
				g.stdWarned = true
				d.log.Warn("cannot set a conntrack sysctl; connection pinning may not hold", "key", key, "err", err)
			}
		}
	}
	return changed, nil
}

// ensureAutoWANTable makes table t hold exactly the auto WAN's subnet and
// its default route.
func ensureAutoWANTable(t int, auto *autoWAN) (bool, error) {
	link, err := netlink.LinkByName(auto.Port)
	if err != nil {
		return false, err
	}
	idx := link.Attrs().Index
	subnet := &net.IPNet{IP: auto.Addr.IP.Mask(auto.Addr.Mask), Mask: auto.Addr.Mask}
	want := []netlink.Route{{Table: t, LinkIndex: idx, Dst: subnet, Scope: netlink.SCOPE_LINK, Src: auto.Addr.IP}}
	def := netlink.Route{Table: t, LinkIndex: idx, Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, Src: auto.Addr.IP}
	if auto.Router != nil {
		def.Gw = auto.Router
	} else {
		def.Scope = netlink.SCOPE_LINK
	}
	want = append(want, def)
	key := func(r netlink.Route) string {
		dst := "0.0.0.0/0"
		if r.Dst != nil && !r.Dst.IP.IsUnspecified() {
			dst = r.Dst.String()
		}
		return fmt.Sprintf("%s %v %d %v", dst, r.Gw, r.LinkIndex, r.Src)
	}
	have, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: t}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return false, err
	}
	wanted := map[string]bool{}
	for _, r := range want {
		wanted[key(r)] = true
	}
	changed := false
	present := map[string]bool{}
	for _, r := range have {
		if wanted[key(r)] {
			present[key(r)] = true
			continue
		}
		r := r
		if err := netlink.RouteDel(&r); err != nil && !rtnl.IsNotFound(err) {
			return changed, err
		}
		changed = true
	}
	for _, r := range want {
		if present[key(r)] {
			continue
		}
		r := r
		if err := netlink.RouteReplace(&r); err != nil {
			return changed, fmt.Errorf("auto WAN route %v in table %d: %w", r.Dst, t, err)
		}
		changed = true
	}
	return changed, nil
}

func mustMAC(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		return nil
	}
	return m
}

func (g *gatewayReconciler) Teardown(ctx context.Context) error {
	d := g.d
	if br, err := netlink.LinkByName(BridgeIfname); err == nil {
		for addr := range g.neighs {
			_ = netlink.NeighDel(&netlink.Neigh{LinkIndex: br.Attrs().Index, Family: netlink.FAMILY_V4, IP: net.ParseIP(addr)})
		}
	}
	g.neighs = nil
	if g.monStop != nil {
		close(g.monStop)
		g.monStop = nil
		// Neither may evaluate, ping or kick once routing is gone: a ping
		// in flight takes at most reachTimeout.
		g.monWG.Wait()
	}
	g.stopProber()
	g.stopWebCheck()
	g.applyHostDNS(ctx, false, "", func(string, string, string, ...any) {})
	var errs []string
	if _, err := rtnl.EnsureRules(ruleLo, ruleHi, nil); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := rtnl.EnsureRules(prioLast, prioLast, nil); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := rtnl.FlushTables(tableNone, tableGWHi, nil); err != nil {
		errs = append(errs, err.Error())
	}
	if g.nftPath != "" {
		if err := nft.DeleteTable(ctx, g.nftPath, "inet", nft.RouteTable); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if g.fwdOrig != "" && g.fwdOrig != "1" {
		_, _ = sysctl.Set("net.ipv4.ip_forward", g.fwdOrig)
	}
	g.routing, g.gwModeSet = "", ""
	d.log.Info("removed gateway routing")
	if len(errs) > 0 {
		return fmt.Errorf("gateway teardown: %s", strings.Join(errs, "; "))
	}
	return nil
}

// systemResolvers reads the system's nameservers for dnsmasq to forward to.
func systemResolvers(exclude ...string) []string {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" && net.ParseIP(fields[1]) != nil && !slices.Contains(exclude, fields[1]) {
			out = append(out, fields[1])
		}
	}
	return out
}

// anycastIP is the mesh's anycast address, or "" for a bad prefix.
func anycastIP(prefix string) string {
	ip, err := anycastAddress(prefix)
	if err != nil {
		return ""
	}
	return ip
}

// forwardsToMesh reports whether dnsmasq's upstreams are mesh servers, not
// a resolver on this host (loopback), which may be the system's own.
func forwardsToMesh(upstreams []string) bool {
	if len(upstreams) == 0 {
		return false
	}
	for _, u := range upstreams {
		host, _, _ := strings.Cut(u, "@")
		if ip := net.ParseIP(host); ip == nil || ip.IsLoopback() {
			return false
		}
	}
	return true
}

// stopped reports whether stop is closed.
func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}
