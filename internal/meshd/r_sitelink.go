package meshd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/nft"
	"meridian/trellis/internal/proc"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/rtnl"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/sysctl"
	"meridian/trellis/internal/wg"
)

// Site link (architecture §13, Site link): a member of the profile's site_link
// holds a WireGuard interface, site0, with a peer for every member of
// another site, and a gretap to each peer inside it. The gretaps are
// batman-adv hard interfaces, so the sites are one mesh.
//
// Routing, all in rules and tables of meshd's own (main is never touched):
//   - the tunnel addresses (169.254.a.b, from each member's key) are
//     routed into site0 by table siteTable, consulted for 169.254.0.0/16
//     only; with no route there the lookup falls through to main;
//   - WireGuard's own packets carry siteMark. Main routes them when it
//     has a route; otherwise they go out of the auto WAN, or nowhere. They
//     never reach rule 32800, which would send them into the mesh, and
//     perhaps through the tunnel they carry.
const (
	SiteIfname       = "site0"
	siteIfPrefix     = "site-"
	siteMark         = 0x73530000
	siteMarkMask     = 0xffff0000
	prioSiteAddrs    = 7190
	prioSiteAutoWAN  = 32798
	prioSiteStop     = 32799
	siteTable        = 7398
	siteTableStop    = 7397
	siteKeepalive    = 25 * time.Second
	siteHandshakeOld = 180 * time.Second // WireGuard drops a session's keys at 180 s
	siteAnswerWait   = 20 * time.Second  // before a tunnel's silence is reported
	siteResolveEvery = 5 * time.Minute
	siteResolveRetry = 30 * time.Second
)

var siteLinkLocal = &net.IPNet{IP: net.IPv4(169, 254, 0, 0).To4(), Mask: net.CIDRMask(16, 32)}

// siteLinkFacts is what the site link reconciler set up, for status.
type siteLinkFacts struct {
	PublicKey string // this node's, member or not
	Member    bool
	Site      string
	Address   string
	Port      int
	Endpoint  string // this node's, from the profile
	// Since is when this node became a member: a tunnel is given a
	// moment before its silence is worth a word.
	Since time.Time
	Peers []sitePeerFacts
	Error string
	// GuardDropped counts what the guard dropped: packets for or from
	// this node's tunnel address that were not in the tunnel.
	GuardDropped uint64
}

type sitePeerFacts struct {
	Name, Site, PublicKey, Endpoint, Ifname, Address string
	Resolved                                         string // the endpoint's address, when it is a name
	// EndpointErr is why the endpoint cannot be used (a name that does
	// not resolve); NoRoute: this node has no uplink that leads to it.
	EndpointErr string
	NoRoute     bool
}

type siteLinkReconciler struct {
	d  *Daemon
	wg *wg.Client
	// live is wg for status requests, which run beside the loop.
	live       atomic.Pointer[wg.Client]
	modprobeAt time.Time
	// nftPath is nft; guard the guard table's script as last applied
	// (nft.SiteGuard).
	nftPath string
	guard   string
	// set is the endpoint last given to each peer (by public key): it is
	// given again only when it changes, or when the peer has gone quiet,
	// so that a peer WireGuard has followed to a new address (NAT) is
	// left there.
	set map[wg.Key]netip.AddrPort
	// active says whether the last pass set anything up, so that a node
	// that never was a member does not look for things to remove.
	active bool
	// since is when this node became a member: a peer is reported quiet
	// only after it had time to answer.
	since time.Time

	mu      sync.Mutex
	resolve map[string]*siteResolve // host → its addresses
}

type siteResolve struct {
	addr     netip.Addr
	err      error
	at       time.Time
	inflight bool
}

func newSiteLinkReconciler(d *Daemon) *siteLinkReconciler {
	return &siteLinkReconciler{d: d, set: map[wg.Key]netip.AddrPort{}, resolve: map[string]*siteResolve{}, active: true}
}

func (s *siteLinkReconciler) Name() string { return "sitelink" }

func (s *siteLinkReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := s.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	plan := d.profile.SiteLink
	facts := siteLinkFacts{}
	// What the last pass set up: a pass that fails before it knows its
	// peers leaves them as they were, so that batman-adv keeps the
	// tunnels that stand.
	var prev siteLinkFacts
	d.facts.Read(func(f *Facts) { prev = f.SiteLink })
	peersKnown := false
	priv, err := wg.ParseKey(d.identity().SiteKey)
	if err == nil {
		if pub, perr := priv.PublicKey(); perr == nil {
			facts.PublicKey = pub.String()
		}
	}
	me, member := plan.Member(facts.PublicKey)
	set := func() { d.facts.Update(func(f *Facts) { f.SiteLink = facts }) }
	if !member || facts.PublicKey == "" {
		set()
		s.since = time.Time{}
		if s.active {
			ch, err := s.remove(ctx)
			s.active = err != nil
			res.Changed = ch
			return res, err
		}
		return res, nil
	}
	s.active = true
	if s.since.IsZero() {
		s.since = time.Now()
	}
	facts.Member, facts.Site, facts.Port, facts.Endpoint, facts.Since = true, me.Site, plan.ListenPort(), me.Endpoint, s.since
	self := config.SiteAddr(facts.PublicKey)
	facts.Address = self.String()
	peers := plan.Peers(facts.PublicKey)
	if len(peers) == 0 {
		warn("info", "site-link-alone", "this node holds the site link for site %q, but the profile lists no member of another site", me.Site)
	}
	fail := func(err error) (reconcile.Result, error) {
		facts.Error = err.Error()
		if !peersKnown && prev.Member && prev.PublicKey == facts.PublicKey {
			facts.Peers = append([]sitePeerFacts(nil), prev.Peers...)
		}
		set()
		return res, err
	}

	if s.wg == nil {
		c, err := wg.Dial()
		if errors.Is(err, wg.ErrNotLoaded) && time.Since(s.modprobeAt) > 30*time.Second {
			s.modprobeAt = time.Now()
			if out, merr := exec.CommandContext(ctx, "modprobe", "wireguard").CombinedOutput(); merr != nil {
				warn("error", "site-link-no-wireguard", "the site link needs WireGuard, and modprobe wireguard failed: %v: %s", merr, strings.TrimSpace(string(out)))
			}
			c, err = wg.Dial()
		}
		if err != nil {
			res.RequeueAfter = 30 * time.Second
			return fail(err)
		}
		s.wg = c
		s.live.Store(c)
	}

	// The guard first: no tunnel stands without it.
	if ch, dropped, guarded, err := s.ensureGuard(ctx, self); err != nil {
		res.RequeueAfter = 30 * time.Second
		if !guarded {
			// The table is known to be missing and cannot be put back:
			// the tunnels come down rather than stand open.
			peersKnown = true
			if _, rerr := removeSiteTunnels(nil); rerr != nil {
				d.log.Warn("cannot remove the site link's tunnels", "err", rerr)
			}
		}
		return fail(fmt.Errorf("the site link is not set up: its tunnels cannot be kept inside WireGuard: %w", err))
	} else {
		facts.GuardDropped = dropped
		res.Changed = res.Changed || ch
	}

	// site0.
	mtu := plan.LinkMTU()
	link, err := rtnl.LinkByNameOrNil(SiteIfname)
	if err != nil {
		return fail(err)
	}
	if link != nil && link.Type() != "wireguard" {
		return fail(fmt.Errorf("%s exists but is a %s interface, not WireGuard; remove it", SiteIfname, link.Type()))
	}
	if link == nil {
		if err := netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: SiteIfname, MTU: mtu}}); err != nil {
			return fail(fmt.Errorf("create %s: %w", SiteIfname, err))
		}
		d.log.Info("created the site link interface", "name", SiteIfname, "address", self, "port", facts.Port)
		res.Changed = true
		if link, err = netlink.LinkByName(SiteIfname); err != nil {
			return fail(err)
		}
	}
	if ch, err := rtnl.EnsureMTU(link, mtu); err != nil {
		return fail(fmt.Errorf("set %s MTU: %w", SiteIfname, err))
	} else if ch {
		res.Changed = true
	}
	cidr := self.String() + "/32"
	if ch, err := rtnl.EnsureAddr(link, cidr); err != nil {
		return fail(fmt.Errorf("address %s: %w", SiteIfname, err))
	} else if ch {
		res.Changed = true
	}
	if _, err := rtnl.RemoveOtherAddrs(link, cidr); err != nil {
		return fail(err)
	}
	// The gretaps' packets come back on site0 from addresses only its
	// table routes: no reverse-path check there.
	if ch, err := sysctl.Set("net.ipv4.conf."+SiteIfname+".rp_filter", "0"); err == nil && ch {
		res.Changed = true
	}
	if ch, err := rtnl.EnsureUp(link); err != nil {
		return fail(err)
	} else if ch {
		res.Changed = true
	}

	// WireGuard: key, port, mark, peers.
	dev, err := s.wg.Device(SiteIfname)
	if err != nil {
		return fail(err)
	}
	cfg, unresolved := s.wantDevice(dev, priv, facts.Port, peers)
	for _, u := range unresolved {
		warn("warning", "site-link-endpoint", "%s", u)
	}
	// What status and the warnings say about each peer.
	want := map[string]sitePeerFacts{}
	for _, p := range peers {
		pf := sitePeerFacts{Name: p.Name, Site: p.Site, PublicKey: p.PublicKey, Endpoint: p.Endpoint,
			Ifname: config.SiteIfname(p.PublicKey), Address: config.SiteAddr(p.PublicKey).String()}
		if host, _, err := net.SplitHostPort(p.Endpoint); err == nil {
			if _, perr := netip.ParseAddr(host); perr != nil {
				if r := s.cached(host); r != nil && r.addr.IsValid() {
					pf.Resolved = r.addr.String()
				}
			}
		}
		if ep, err := s.endpoint(p.Endpoint); err != nil {
			pf.EndpointErr = err.Error()
		} else if ep.IsValid() {
			pf.NoRoute = !siteRouteTo(ep.Addr())
		}
		want[pf.Ifname] = pf
		facts.Peers = append(facts.Peers, pf)
	}
	peersKnown = true
	if time.Since(s.since) > siteHandshakeOld {
		last := map[string]time.Time{}
		for _, p := range dev.Peers {
			last[p.PublicKey.String()] = p.LastHandshake
		}
		for _, pf := range facts.Peers {
			if why := sitePeerNote(facts, pf, last[pf.PublicKey], time.Now()); why != "" {
				warn("warning", "site-link-down", "site link peer %s (site %s): %s", sitePeerName(pf), pf.Site, why)
			}
		}
	}
	if cfg != nil {
		if err := s.wg.Configure(SiteIfname, *cfg); err != nil {
			return fail(err)
		}
		for _, p := range cfg.Peers {
			if p.Remove {
				delete(s.set, p.PublicKey)
			} else if p.Endpoint.IsValid() {
				s.set[p.PublicKey] = p.Endpoint
			}
		}
		d.log.Info("configured the site link", "interface", SiteIfname, "peers", len(peers))
		res.Changed = true
	}

	// A gretap to each peer.
	for _, pf := range facts.Peers {
		ch, err := ensureGretap(pf.Ifname, self, config.SiteAddr(pf.PublicKey), config.SiteTunnelMAC(facts.PublicKey, pf.PublicKey), mtu-config.SiteTunnelOverhead)
		if err != nil {
			return fail(err)
		}
		if ch {
			d.log.Info("set up the tunnel to a site link peer", "interface", pf.Ifname, "peer", pf.Name, "site", pf.Site)
			res.Changed = true
		}
		if ch, err := ensureSiteBudget(pf.Ifname, plan.MulticastBudgetKbit()); err != nil {
			warn("warning", "site-link-budget", "cannot limit broadcast and multicast through %s: %v", pf.Ifname, err)
		} else if ch {
			res.Changed = true
		}
	}
	if ch, err := removeSiteTunnels(want); err != nil {
		return fail(err)
	} else if ch {
		res.Changed = true
	}

	// Routing.
	if ch, err := s.ensureRouting(link.Attrs().Index, self, peers); err != nil {
		return fail(err)
	} else if ch {
		res.Changed = true
	}
	set()
	res.RequeueAfter = 10 * time.Second
	return res, nil
}

// wantDevice is the change that makes dev what the plan asks for, or nil
// when it already is. It names the endpoints that cannot be used yet.
func (s *siteLinkReconciler) wantDevice(dev *wg.Device, priv wg.Key, port int, peers []config.SiteMember) (*wg.Config, []string) {
	var unresolved []string
	cfg := &wg.Config{PrivateKey: priv, ListenPort: port, FWMark: siteMark}
	changed := dev.PrivateKey != priv.Clamped() || dev.ListenPort != port || dev.FWMark != siteMark
	have := map[wg.Key]wg.Peer{}
	for _, p := range dev.Peers {
		have[p.PublicKey] = p
	}
	wanted := map[wg.Key]bool{}
	for _, m := range peers {
		k, err := wg.ParseKey(m.PublicKey)
		if err != nil {
			continue // the profile was validated
		}
		wanted[k] = true
		ep, err := s.endpoint(m.Endpoint)
		// A name still being looked up (the first pass after a start) is
		// not a fault to report: the answer comes within the pass or two.
		if err != nil && !errors.Is(err, errSiteLookupPending) {
			unresolved = append(unresolved, fmt.Sprintf("site link peer %s (site %s): endpoint %s: %v; waiting for it to dial in", siteName(m), m.Site, m.Endpoint, err))
		}
		allowed := []netip.Prefix{netip.PrefixFrom(config.SiteAddr(m.PublicKey), 32)}
		pc := wg.PeerConfig{PublicKey: k, Keepalive: siteKeepalive, AllowedIPs: allowed}
		cur, ok := have[k]
		// After a restart nothing is remembered: a peer whose session
		// works is left where WireGuard has it, as before the restart.
		if _, known := s.set[k]; ok && !known && ep.IsValid() && !cur.LastHandshake.IsZero() && time.Since(cur.LastHandshake) < siteHandshakeOld {
			s.set[k] = ep
		}
		give := ep.IsValid() && (!ok || s.set[k] != ep ||
			(cur.Endpoint != ep && time.Since(cur.LastHandshake) > siteHandshakeOld))
		if give {
			pc.Endpoint = ep
		}
		if !ok || give || cur.Keepalive != siteKeepalive || !slices.Equal(cur.AllowedIPs, allowed) {
			cfg.Peers = append(cfg.Peers, pc)
			changed = true
		}
	}
	for k := range have {
		if !wanted[k] {
			cfg.Peers = append(cfg.Peers, wg.PeerConfig{PublicKey: k, Remove: true})
			changed = true
		}
	}
	if !changed {
		return nil, unresolved
	}
	return cfg, unresolved
}

// sitePeerNote says why the tunnel to a peer is not up, in words for the
// status and the alert: what the operator has to look at. Empty when it
// is up (a handshake within WireGuard's session life).
func sitePeerNote(f siteLinkFacts, p sitePeerFacts, last time.Time, now time.Time) string {
	if !last.IsZero() && now.Sub(last) < siteHandshakeOld {
		return ""
	}
	here := "this node"
	if f.Endpoint != "" {
		here += " (" + f.Endpoint + ")"
	}
	switch {
	case p.Endpoint == "" && f.Endpoint == "":
		return "neither this node nor it has an endpoint in the profile, so neither can dial the other: one of the two needs its public address and port there"
	case p.EndpointErr != "" && f.Endpoint == "":
		return fmt.Sprintf("its endpoint %s cannot be used (%s), and this node has none for it to dial", p.Endpoint, p.EndpointErr)
	case p.EndpointErr != "":
		return fmt.Sprintf("its endpoint %s cannot be used (%s): waiting for it to dial %s", p.Endpoint, p.EndpointErr, here)
	case p.NoRoute && f.Endpoint == "":
		return fmt.Sprintf("this node has no uplink that leads to %s: a member needs internet of its own", p.Endpoint)
	case p.NoRoute:
		return fmt.Sprintf("this node has no uplink that leads to %s: waiting for it to dial %s", p.Endpoint, here)
	}
	var b strings.Builder
	if last.IsZero() {
		// A handshake takes a few seconds: nothing to say yet.
		if now.Sub(f.Since) < siteAnswerWait {
			return ""
		}
		b.WriteString("no answer yet")
	} else {
		fmt.Fprintf(&b, "it stopped answering %s ago", now.Sub(last).Round(time.Second))
	}
	b.WriteString(": check that the node there is up, has internet and has this profile, and that UDP reaches ")
	switch {
	case p.Endpoint == "":
		fmt.Fprintf(&b, "%s from there (it has no endpoint, so it dials; this node's router must forward UDP port %d to it)", here, f.Port)
	case f.Endpoint == "":
		fmt.Fprintf(&b, "it at %s (its router must forward that port to it)", p.Endpoint)
	default:
		fmt.Fprintf(&b, "it at %s, or %s from there (one of the two routers must forward the port)", p.Endpoint, here)
	}
	return b.String()
}

// siteRouteTo reports whether WireGuard's packets have a way to addr: a
// route for the site link's mark that is not the stop at the end of its
// rules. With none, this node has no uplink of its own.
func siteRouteTo(addr netip.Addr) bool {
	routes, err := netlink.RouteGetWithOptions(net.IP(addr.AsSlice()), &netlink.RouteGetOptions{Mark: siteMark})
	if err != nil {
		// No route is an error from the kernel (unreachable); anything
		// else is not a finding.
		return !errors.Is(err, unix.ENETUNREACH) && !errors.Is(err, unix.EHOSTUNREACH)
	}
	for _, r := range routes {
		if r.Type == unix.RTN_UNREACHABLE || r.Type == unix.RTN_BLACKHOLE || r.Type == unix.RTN_PROHIBIT {
			return false
		}
	}
	return true
}

func sitePeerName(p sitePeerFacts) string {
	if p.Name != "" {
		return p.Name
	}
	return p.PublicKey[:8] + "…"
}

func siteName(m config.SiteMember) string {
	if m.Name != "" {
		return m.Name
	}
	return m.PublicKey[:8] + "…"
}

// endpoint is a member's endpoint as an address: an address as it is, a
// name from the last lookup (made in the background, and again every
// few minutes). Invalid, with no error, when the member has none.
func (s *siteLinkReconciler) endpoint(ep string) (netip.AddrPort, error) {
	if ep == "" {
		return netip.AddrPort{}, nil
	}
	if ap, err := netip.ParseAddrPort(ep); err == nil {
		// An IPv4 address written as IPv6 is IPv4 to WireGuard, and reads
		// back so: compared in that form.
		return netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port()), nil
	}
	host, port, err := net.SplitHostPort(ep)
	if err != nil {
		return netip.AddrPort{}, err
	}
	var pn uint16
	if _, err := fmt.Sscan(port, &pn); err != nil {
		return netip.AddrPort{}, err
	}
	r := s.cached(host)
	if r == nil || !r.addr.IsValid() {
		if r != nil && r.err != nil {
			return netip.AddrPort{}, r.err
		}
		return netip.AddrPort{}, errSiteLookupPending
	}
	return netip.AddrPortFrom(r.addr, pn), nil
}

// errSiteLookupPending: an endpoint's name has no answer yet, and no
// error either.
var errSiteLookupPending = errors.New("not looked up yet")

// cached returns the last lookup of host, and starts a new one when it is
// old.
func (s *siteLinkReconciler) cached(host string) *siteResolve {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.resolve[host]
	if r == nil {
		r = &siteResolve{}
		s.resolve[host] = r
	}
	every := siteResolveEvery
	if !r.addr.IsValid() {
		every = siteResolveRetry
	}
	if !r.inflight && (r.at.IsZero() || time.Since(r.at) > every) && s.d.ctx != nil {
		r.inflight = true
		go s.lookup(host)
	}
	out := *r
	return &out
}

func (s *siteLinkReconciler) lookup(host string) {
	ctx, cancel := context.WithTimeout(s.d.ctx, 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	var a netip.Addr
	if err == nil {
		// IPv4 first: most uplinks have it.
		for _, x := range addrs {
			if x.Unmap().Is4() {
				a = x.Unmap()
				break
			}
		}
		if !a.IsValid() && len(addrs) > 0 {
			a = addrs[0]
		}
		if !a.IsValid() {
			err = errors.New("no address")
		}
	}
	s.mu.Lock()
	r := s.resolve[host]
	before := r.addr
	r.inflight, r.at = false, time.Now()
	if err != nil {
		r.err = fmt.Errorf("cannot look up %s: %w", host, err)
		// Keep a known address: the uplink's DNS may be down a moment.
	} else {
		r.addr, r.err = a, nil
	}
	changed := r.addr != before
	s.mu.Unlock()
	if changed {
		s.d.Kick("site link endpoint looked up")
	}
}

// ensureGretap makes name a gretap from local to remote with this address
// and MTU. The address is the one every node works out for this tunnel
// (config.SiteTunnelMAC): batman-adv names the neighbour by it.
func ensureGretap(name string, local, remote netip.Addr, mac net.HardwareAddr, mtu int) (bool, error) {
	l, err := rtnl.LinkByNameOrNil(name)
	if err != nil {
		return false, err
	}
	changed := false
	if l != nil {
		gt, ok := l.(*netlink.Gretap)
		if !ok || !gt.Local.Equal(net.IP(local.AsSlice())) || !gt.Remote.Equal(net.IP(remote.AsSlice())) || !bytes.Equal(l.Attrs().HardwareAddr, mac) || gt.PMtuDisc != 0 {
			if err := netlink.LinkDel(l); err != nil {
				return false, fmt.Errorf("replace %s: %w", name, err)
			}
			l, changed = nil, true
		}
	}
	if l == nil {
		// No path MTU discovery: the tunnel's packets may be fragmented
		// on their way into site0. batman-adv splits unicast to fit the
		// tunnel, but not broadcast (flooded multicast included): a
		// full-size broadcast frame is larger than the tunnel, and with
		// "don't fragment" set it was dropped here. The fragments travel
		// inside WireGuard, between the two tunnel addresses only.
		gt := &netlink.Gretap{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: mtu, HardwareAddr: mac},
			Local: net.IP(local.AsSlice()), Remote: net.IP(remote.AsSlice()), Ttl: 64, PMtuDisc: 0}
		if err := netlink.LinkAdd(gt); err != nil {
			return false, fmt.Errorf("create %s: %w", name, err)
		}
		changed = true
		if l, err = netlink.LinkByName(name); err != nil {
			return false, err
		}
	}
	if ch, err := rtnl.EnsureMTU(l, mtu); err != nil {
		return changed, fmt.Errorf("set %s MTU: %w", name, err)
	} else if ch {
		changed = true
	}
	return changed, nil
}

// sitePolicePref is the priority of the budget's ingress filter; the
// egress one has the next. u32 lists the rules of both directions under
// each (its hash tables belong to the qdisc), so the priority tells them
// apart.
const sitePolicePref = 7353

// ensureSiteBudget limits the broadcast and multicast a tunnel carries,
// each way, to kbit: batman-adv's broadcast (0x01) and multicast (0x05)
// packet types, which every node of the far site would send on its radios.
// Routing (OGMs, ELP) and unicast pass untouched. A tc policer on the
// tunnel's clsact hooks: it goes with the tunnel.
func ensureSiteBudget(name string, kbit int) (bool, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return false, err
	}
	rate := uint32(kbit * 1000 / 8)
	burst := max(rate/5, 16000) // 200 ms, and never below a few full frames
	qds, err := netlink.QdiscList(l)
	if err != nil {
		return false, err
	}
	changed := false
	has := false
	for _, q := range qds {
		has = has || q.Type() == "clsact"
	}
	if !has {
		q := &netlink.GenericQdisc{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: l.Attrs().Index, Parent: netlink.HANDLE_CLSACT, Handle: netlink.MakeHandle(0xffff, 0)}, QdiscType: "clsact"}
		if err := netlink.QdiscAdd(q); err != nil {
			return false, fmt.Errorf("clsact: %w", err)
		}
		changed = true
	}
	for i, parent := range []uint32{netlink.HANDLE_MIN_INGRESS, netlink.HANDLE_MIN_EGRESS} {
		pref := uint16(sitePolicePref + i)
		fs, err := netlink.FilterList(l, parent)
		if err != nil {
			return changed, err
		}
		// The dump lists u32's own hash table beside the rule: a rule
		// with the policer at this rate is what counts.
		found, stale := false, false
		for _, f := range fs {
			if f.Attrs().Priority != pref {
				continue
			}
			u, isU32 := f.(*netlink.U32)
			switch {
			case !isU32:
				stale = true
			case len(u.Actions) == 0:
			default:
				p, isPolice := u.Actions[0].(*netlink.PoliceAction)
				if isPolice && p.Rate == rate && !found {
					found = true
				} else {
					stale = true
				}
			}
		}
		if found && !stale {
			continue
		}
		if found || stale {
			del := &netlink.U32{FilterAttrs: netlink.FilterAttrs{LinkIndex: l.Attrs().Index, Parent: parent, Priority: pref, Protocol: batadvEthertype}}
			if err := netlink.FilterDel(del); err != nil && !rtnl.IsNotFound(err) {
				return changed, err
			}
			changed = true
		}
		police := netlink.NewPoliceAction()
		police.Rate, police.Burst, police.ExceedAction, police.NotExceedAction = rate, burst, netlink.TC_POLICE_SHOT, netlink.TC_POLICE_OK
		f := &netlink.U32{
			FilterAttrs: netlink.FilterAttrs{LinkIndex: l.Attrs().Index, Parent: parent, Priority: pref, Protocol: batadvEthertype},
			Sel: &netlink.TcU32Sel{Flags: netlink.TC_U32_TERMINAL, Nkeys: 1,
				Keys: []netlink.TcU32Key{{Mask: 0xfb000000, Val: 0x01000000, Off: 0}}},
			Actions: []netlink.Action{police},
		}
		if err := netlink.FilterAdd(f); err != nil {
			return changed, fmt.Errorf("filter: %w", err)
		}
		changed = true
	}
	return changed, nil
}

// batadvEthertype is ETH_P_BATMAN.
const batadvEthertype = 0x4305

// removeSiteTunnels deletes site-* interfaces not in keep.
func removeSiteTunnels(keep map[string]sitePeerFacts) (bool, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return false, err
	}
	changed := false
	for _, l := range links {
		n := l.Attrs().Name
		if !strings.HasPrefix(n, siteIfPrefix) || l.Type() != "gretap" {
			continue
		}
		if _, ok := keep[n]; ok {
			continue
		}
		if err := netlink.LinkDel(l); err != nil && !rtnl.IsNotFound(err) {
			return changed, fmt.Errorf("remove %s: %w", n, err)
		}
		changed = true
	}
	return changed, nil
}

// ensureRouting sets the tunnel addresses' table and the rules for
// WireGuard's own packets.
func (s *siteLinkReconciler) ensureRouting(siteIdx int, self netip.Addr, peers []config.SiteMember) (bool, error) {
	changed := false
	var want []netlink.Route
	for _, p := range peers {
		a := config.SiteAddr(p.PublicKey)
		want = append(want, netlink.Route{Table: siteTable, LinkIndex: siteIdx, Scope: netlink.SCOPE_LINK,
			Dst: &net.IPNet{IP: net.IP(a.AsSlice()), Mask: net.CIDRMask(32, 32)}, Src: net.IP(self.AsSlice())})
	}
	ch, err := ensureTable(siteTable, want)
	if err != nil {
		return false, err
	}
	changed = changed || ch
	stop := []netlink.Route{{Table: siteTableStop, Type: unix.RTN_UNREACHABLE, Dst: &net.IPNet{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)}}}
	if ch, err = ensureTable(siteTableStop, stop); err != nil {
		return changed, err
	}
	changed = changed || ch

	mask := uint32(siteMarkMask)
	rule := func(prio, table int, mark bool, dst *net.IPNet) netlink.Rule {
		r := *netlink.NewRule()
		r.Priority, r.Table, r.Dst = prio, table, dst
		if mark {
			r.Mark, r.Mask = siteMark, &mask
		}
		return r
	}
	if ch, err = rtnl.EnsureRules(prioSiteAddrs, prioSiteAddrs, []netlink.Rule{rule(prioSiteAddrs, siteTable, false, siteLinkLocal)}); err != nil {
		return changed, err
	}
	changed = changed || ch
	var last []netlink.Rule
	var auto *autoWAN
	s.d.facts.Read(func(f *Facts) { auto = f.AutoWAN })
	if auto != nil {
		last = append(last, rule(prioSiteAutoWAN, tableGWBase+s.d.identity().NodeID, true, nil))
	}
	last = append(last, rule(prioSiteStop, siteTableStop, true, nil))
	if ch, err = rtnl.EnsureRules(prioSiteAutoWAN, prioSiteStop, last); err != nil {
		return changed, err
	}
	return changed || ch, nil
}

// ensureTable makes an IPv4 table hold exactly want (compared by
// destination, interface, type and source).
func ensureTable(table int, want []netlink.Route) (bool, error) {
	have, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return false, err
	}
	wanted := map[string]netlink.Route{}
	for _, r := range want {
		wanted[routeKey(r)] = r
	}
	changed := false
	present := map[string]bool{}
	for _, r := range have {
		k := routeKey(r)
		if _, ok := wanted[k]; ok && !present[k] {
			present[k] = true
			continue
		}
		r := r
		if err := netlink.RouteDel(&r); err != nil && !rtnl.IsNotFound(err) {
			return changed, fmt.Errorf("delete route %s from table %d: %w", k, table, err)
		}
		changed = true
	}
	for k, r := range wanted {
		if present[k] {
			continue
		}
		r := r
		if err := netlink.RouteAdd(&r); err != nil && !rtnl.IsExist(err) {
			return changed, fmt.Errorf("add route %s to table %d: %w", k, table, err)
		}
		changed = true
	}
	return changed, nil
}

// routeKey identifies a route for comparison. A route written without a
// type is a unicast one, which is how the kernel lists it: the two must
// compare equal, or the route is deleted and added again on every pass.
func routeKey(r netlink.Route) string {
	dst := "default"
	if r.Dst != nil && !(r.Dst.IP.IsUnspecified() && isZeroMask(r.Dst.Mask)) {
		dst = r.Dst.String()
	}
	typ := r.Type
	if typ == 0 {
		typ = unix.RTN_UNICAST
	}
	return fmt.Sprintf("%s %d %d %v", dst, r.LinkIndex, typ, r.Src)
}

// ensureGuard puts the guard table in place (nft.SiteGuard) and returns
// what it has dropped. The table is checked on every pass: a flushed
// ruleset must not leave the tunnels open.
// guarded is false when the table is known not to be in place (an error
// that leaves it unknown says true: a table that was there stays).
func (s *siteLinkReconciler) ensureGuard(ctx context.Context, self netip.Addr) (changed bool, dropped uint64, guarded bool, err error) {
	if s.nftPath == "" {
		p, err := proc.LookPath("nft")
		if err != nil {
			return false, 0, false, errors.New("nft is not installed (apt install nftables)")
		}
		s.nftPath = p
	}
	want := nft.SiteGuard{If: SiteIfname, Addr: self.String()}.Render()
	present, rules, counters, err := nft.TableState(ctx, s.nftPath, "inet", nft.SiteTable)
	if err != nil {
		return false, 0, s.guard == want, err
	}
	// The table with its rules: one that was emptied guards nothing.
	whole := present && rules == nft.SiteGuardRules
	if whole && want == s.guard {
		return false, counters[nft.RuleSiteGuard], true, nil
	}
	if err := nft.Apply(ctx, s.nftPath, want); err != nil {
		s.guard = ""
		return false, 0, false, err
	}
	if s.guard != "" && !whole {
		s.d.log.Warn("the site link's guard table had gone or lost its rules (nftables flushed?); put back")
	}
	s.guard = want
	return true, 0, true, nil
}

// remove takes down everything the site link set up. The guard goes
// last, after the tunnels it guards.
func (s *siteLinkReconciler) remove(ctx context.Context) (bool, error) {
	changed := false
	var errs []error
	if ch, err := removeSiteTunnels(nil); err != nil {
		errs = append(errs, err)
	} else if ch {
		changed = true
	}
	// Only a WireGuard interface of that name is the site link's: another
	// kind is someone else's (the member's side refuses to use it, too),
	// and every node comes through here on its first pass.
	if l, err := rtnl.LinkByNameOrNil(SiteIfname); err != nil {
		errs = append(errs, err)
	} else if l != nil && l.Type() == "wireguard" {
		if err := netlink.LinkDel(l); err != nil && !rtnl.IsNotFound(err) {
			errs = append(errs, err)
		} else {
			s.d.log.Info("removed the site link interface", "name", SiteIfname)
			changed = true
		}
	}
	for _, r := range [][2]int{{prioSiteAddrs, prioSiteAddrs}, {prioSiteAutoWAN, prioSiteStop}} {
		if ch, err := rtnl.EnsureRules(r[0], r[1], nil); err != nil {
			errs = append(errs, err)
		} else if ch {
			changed = true
		}
	}
	for _, t := range []int{siteTable, siteTableStop} {
		if ch, err := ensureTable(t, nil); err != nil {
			errs = append(errs, err)
		} else if ch {
			changed = true
		}
	}
	clear(s.set)
	if len(errs) == 0 {
		if s.nftPath == "" {
			s.nftPath, _ = proc.LookPath("nft")
		}
		if s.nftPath != "" {
			if present, _, _, err := nft.TableState(ctx, s.nftPath, "inet", nft.SiteTable); err != nil {
				errs = append(errs, err)
			} else if present {
				if err := nft.DeleteTable(ctx, s.nftPath, "inet", nft.SiteTable); err != nil {
					errs = append(errs, err)
				} else {
					changed = true
				}
			}
		}
		s.guard = ""
	}
	return changed, errors.Join(errs...)
}

func (s *siteLinkReconciler) Teardown(ctx context.Context) error {
	_, err := s.remove(ctx)
	if s.wg != nil {
		s.live.Store(nil)
		_ = s.wg.Close()
		s.wg = nil
	}
	return err
}

// siteIfnames are the tunnels the site link wants as batman-adv hard
// interfaces.
func (f *Facts) siteIfnames() []string {
	var out []string
	for _, p := range f.SiteLink.Peers {
		out = append(out, p.Ifname)
	}
	return out
}

// siteLinkStatus fills st.SiteLink: the reconciler's facts, WireGuard's
// peers as they are now, and batman-adv's view of each tunnel (after
// collectBatman).
func (d *Daemon) siteLinkStatus(st *status.Status, profile *config.Profile) {
	var f siteLinkFacts
	d.facts.Read(func(fa *Facts) {
		f = fa.SiteLink
		f.Peers = append([]sitePeerFacts(nil), fa.SiteLink.Peers...)
	})
	sl := status.SiteLink{PublicKey: f.PublicKey, Member: f.Member, Members: len(profile.SiteLink.Members),
		Site: f.Site, Address: f.Address, Port: f.Port, Endpoint: f.Endpoint, Peers: []status.SitePeer{}, Error: f.Error, GuardDropped: f.GuardDropped}
	live := map[string]wg.Peer{}
	if c := d.sitelinkLive(); c != nil && f.Member {
		if dev, err := c.Device(SiteIfname); err == nil {
			for _, p := range dev.Peers {
				live[p.PublicKey.String()] = p
			}
		}
	}
	active := map[string]bool{}
	for _, h := range st.Batman.HardIfs {
		active[h.Name] = h.Active
	}
	neigh := map[string]int{}
	for _, n := range st.Batman.Neighbors {
		neigh[n.Via]++
	}
	now := time.Now()
	for _, p := range f.Peers {
		sp := status.SitePeer{Name: p.Name, Site: p.Site, PublicKey: p.PublicKey, Endpoint: p.Endpoint, Resolved: p.Resolved,
			Interface: p.Ifname, Address: p.Address, Batman: active[p.Ifname], Neighbors: neigh[p.Ifname]}
		if lp, ok := live[p.PublicKey]; ok {
			if lp.Endpoint.IsValid() {
				sp.Current = lp.Endpoint.String()
			}
			sp.LastHandshake, sp.RxBytes, sp.TxBytes = lp.LastHandshake, lp.RxBytes, lp.TxBytes
			sp.Up = !lp.LastHandshake.IsZero() && now.Sub(lp.LastHandshake) < siteHandshakeOld
		}
		sp.Note = sitePeerNote(f, p, sp.LastHandshake, now)
		// The node that holds the key, when this node hears it; and when
		// it hears it next door without the tunnel, the two are at one
		// site.
		for _, n := range st.Nodes {
			if n.SiteKey != p.PublicKey {
				continue
			}
			sp.Callsign = n.Callsign
			if via := directNeighbor(st, n); via != "" {
				near := fmt.Sprintf("it is a direct neighbour of this node (on %s): the two are at one place, and need no link over the internet. Members of one site take the same site name", via)
				if sp.Note != "" {
					near += ". Besides, " + sp.Note
				}
				sp.Note = near
			}
		}
		sl.Peers = append(sl.Peers, sp)
	}
	// Every member the profile lists, with what is known of it.
	sl.Roster = []status.SiteMemberState{}
	for _, m := range profile.SiteLink.Members {
		ms := status.SiteMemberState{Name: m.Name, Site: m.Site, PublicKey: m.PublicKey, Endpoint: m.Endpoint}
		if m.PublicKey == f.PublicKey && f.PublicKey != "" {
			ms.Self, ms.Heard, ms.Callsign, ms.Reported, ms.Peers = true, true, st.Node.Callsign, f.Member, len(sl.Peers)
			for _, p := range sl.Peers {
				if p.Up {
					ms.Up++
				}
			}
		}
		for _, n := range st.Nodes {
			if n.SiteKey != m.PublicKey || ms.Self {
				continue
			}
			ms.Heard, ms.Callsign = true, n.Callsign
			if n.SiteLink != nil {
				ms.Reported, ms.Up, ms.Peers = true, n.SiteLink.Up, n.SiteLink.Peers
			}
		}
		sl.Roster = append(sl.Roster, ms)
	}
	st.SiteLink = sl
}

// siteLinkGossip is this node's part in the site link for its gossip
// record: nil when it is not a member.
func (d *Daemon) siteLinkGossip() *gossip.SiteState {
	var f siteLinkFacts
	d.facts.Read(func(fa *Facts) {
		f = fa.SiteLink
		f.Peers = append([]sitePeerFacts(nil), fa.SiteLink.Peers...)
	})
	if !f.Member {
		return nil
	}
	s := &gossip.SiteState{Site: f.Site, Peers: len(f.Peers)}
	c := d.sitelinkLive()
	if c == nil {
		return s
	}
	dev, err := c.Device(SiteIfname)
	if err != nil {
		return s
	}
	want := map[string]bool{}
	for _, p := range f.Peers {
		want[p.PublicKey] = true
	}
	for _, p := range dev.Peers {
		if want[p.PublicKey.String()] {
			up, lost := siteTunnelState(p.LastHandshake, time.Now())
			s.Up += up
			s.Lost += lost
		}
	}
	return s
}

// siteTunnelState says of a tunnel, from its last WireGuard handshake,
// whether it is up, or lost: down, and up within the time the nodes
// behind it are remembered (roster.go). One that never had a handshake,
// or has been down for longer, is neither.
func siteTunnelState(handshake, now time.Time) (up, lost int) {
	if handshake.IsZero() {
		return 0, 0
	}
	switch age := now.Sub(handshake); {
	case age < siteHandshakeOld:
		return 1, 0
	case age < rosterKeep:
		return 0, 1
	}
	return 0, 0
}

// directNeighbor is the interface this node hears n on as a batman-adv
// neighbour, tunnels of the site link aside; "" when it does not.
func directNeighbor(st *status.Status, n status.MeshNode) string {
	for _, nb := range st.Batman.Neighbors {
		if strings.HasPrefix(nb.Via, siteIfPrefix) {
			continue
		}
		if nb.MAC == n.MAC || slices.Contains(n.Originators, nb.MAC) {
			return nb.Via
		}
	}
	return ""
}

// sitePublicKey is the node's site link key as the profile lists it; ""
// when it has none yet.
func (d *Daemon) sitePublicKey() string {
	id := d.identity()
	if id == nil {
		return ""
	}
	priv, err := wg.ParseKey(id.SiteKey)
	if err != nil {
		return ""
	}
	pub, err := priv.PublicKey()
	if err != nil {
		return ""
	}
	return pub.String()
}

func (d *Daemon) sitelinkLive() *wg.Client {
	if d.sitelink == nil {
		return nil
	}
	return d.sitelink.live.Load()
}

// SiteLinkNewKey gives this node a new site link key (api.Provider).
func (d *Daemon) SiteLinkNewKey(force bool) (*api.SiteKey, error) {
	d.identWriteMu.Lock()
	defer d.identWriteMu.Unlock()
	cur := d.identity()
	was := ""
	if k, err := wg.ParseKey(cur.SiteKey); err == nil {
		if pub, err := k.PublicKey(); err == nil {
			was = pub.String()
		}
	}
	_, profile := d.conf()
	if m, ok := profile.SiteLink.Member(was); ok && !force {
		return nil, api.Errorf(http.StatusConflict, "this node holds the site link for site %q under its current key: with a new one it leaves the link until the profile lists the new key in its place (force to go on)", m.Site)
	}
	priv, err := wg.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	pub, err := priv.PublicKey()
	if err != nil {
		return nil, err
	}
	next := *cur
	next.SiteKey = priv.String()
	if err := d.setIdentity(&next); err != nil {
		return nil, err
	}
	d.log.Info("new site link key", "public_key", pub.String(), "was", was)
	d.Kick("new site link key")
	return &api.SiteKey{PublicKey: pub.String(), Was: was}, nil
}
