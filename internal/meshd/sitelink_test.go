package meshd

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/wg"
)

func siteTestKey(t *testing.T) (wg.Key, string) {
	t.Helper()
	k, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := k.PublicKey()
	return k, p.String()
}

// The device is changed only where it differs, and an endpoint is given
// again only when it changed or the peer went quiet.
func TestSiteWantDevice(t *testing.T) {
	priv, _ := siteTestKey(t)
	_, pubA := siteTestKey(t)
	_, pubB := siteTestKey(t)
	_, pubOld := siteTestKey(t)
	s := newSiteLinkReconciler(&Daemon{})
	peers := []config.SiteMember{
		{Name: "a", Site: "hq", PublicKey: pubA, Endpoint: "192.0.2.1:51873"},
		{Name: "b", Site: "hq", PublicKey: pubB}, // behind NAT: dials in
	}
	keyOf := func(s string) wg.Key { k, _ := wg.ParseKey(s); return k }
	old := keyOf(pubOld)
	dev := &wg.Device{Peers: []wg.Peer{{PublicKey: old}}}

	cfg, unresolved := s.wantDevice(dev, priv, 51873, peers)
	if cfg == nil || len(unresolved) != 0 || cfg.ListenPort != 51873 || cfg.FWMark != siteMark || cfg.PrivateKey != priv {
		t.Fatalf("first: %+v %v", cfg, unresolved)
	}
	got := map[wg.Key]wg.PeerConfig{}
	for _, p := range cfg.Peers {
		got[p.PublicKey] = p
	}
	a, b := got[keyOf(pubA)], got[keyOf(pubB)]
	if !got[old].Remove || a.Endpoint.String() != "192.0.2.1:51873" || b.Endpoint.IsValid() ||
		a.Keepalive != siteKeepalive || len(b.AllowedIPs) != 1 || b.AllowedIPs[0].Addr() != config.SiteAddr(pubB) {
		t.Fatalf("peers %+v", cfg.Peers)
	}
	s.set[keyOf(pubA)] = a.Endpoint

	// The kernel now holds it (the key clamped), and a NATed peer's
	// address learned: nothing to do.
	now := time.Now()
	allowed := func(pub string) []netip.Prefix { return []netip.Prefix{netip.PrefixFrom(config.SiteAddr(pub), 32)} }
	dev = &wg.Device{PrivateKey: priv.Clamped(), ListenPort: 51873, FWMark: siteMark, Peers: []wg.Peer{
		{PublicKey: keyOf(pubA), Endpoint: netip.MustParseAddrPort("198.51.100.7:4000"), Keepalive: siteKeepalive, AllowedIPs: allowed(pubA), LastHandshake: now},
		{PublicKey: keyOf(pubB), Endpoint: netip.MustParseAddrPort("203.0.113.9:1234"), Keepalive: siteKeepalive, AllowedIPs: allowed(pubB), LastHandshake: now},
	}}
	if cfg, _ := s.wantDevice(dev, priv, 51873, peers); cfg != nil {
		t.Fatalf("unchanged device configured again: %+v", cfg)
	}
	// Peer a quiet past a session's life, away from its endpoint: given it again.
	dev.Peers[0].LastHandshake = now.Add(-4 * time.Minute)
	cfg, _ = s.wantDevice(dev, priv, 51873, peers)
	if cfg == nil || len(cfg.Peers) != 1 || cfg.Peers[0].Endpoint.String() != "192.0.2.1:51873" {
		t.Fatalf("quiet peer: %+v", cfg)
	}
	// After a restart nothing is remembered: a peer whose session works
	// away from its profile endpoint is left there.
	s2 := newSiteLinkReconciler(&Daemon{})
	dev.Peers[0].LastHandshake = now
	if cfg, _ := s2.wantDevice(dev, priv, 51873, peers); cfg != nil {
		t.Fatalf("after a restart, a working peer was given its endpoint again: %+v", cfg)
	}
	// One whose session is dead is given it.
	s3 := newSiteLinkReconciler(&Daemon{})
	dev.Peers[0].LastHandshake = now.Add(-4 * time.Minute)
	if cfg, _ := s3.wantDevice(dev, priv, 51873, peers); cfg == nil || len(cfg.Peers) != 1 || cfg.Peers[0].Endpoint.String() != "192.0.2.1:51873" {
		t.Fatalf("after a restart, a dead peer: %+v", cfg)
	}
	// An IPv4 address written as IPv6 is the same endpoint: not given
	// again on every pass.
	peers[0].Endpoint = "[::ffff:192.0.2.1]:51873"
	dev.Peers[0].Endpoint = netip.MustParseAddrPort("192.0.2.1:51873")
	if cfg, _ := s.wantDevice(dev, priv, 51873, peers); cfg != nil {
		t.Fatalf("an IPv4-mapped endpoint configured again: %+v", cfg)
	}
	peers[0].Endpoint = "192.0.2.1:51873"
	// A name not looked up yet (the first pass after a start) is no fault
	// to report; one the resolver refused is.
	peers[0].Endpoint = "hq.example.org:51873"
	dev.Peers[0].LastHandshake = now
	s.set[keyOf(pubA)] = netip.MustParseAddrPort("192.0.2.1:51873")
	if _, unresolved := s.wantDevice(dev, priv, 51873, peers); len(unresolved) != 0 {
		t.Fatalf("a lookup still on its way reported: %v", unresolved)
	}
	s.resolve["hq.example.org"] = &siteResolve{err: errors.New("no such host"), at: now}
	if _, unresolved := s.wantDevice(dev, priv, 51873, peers); len(unresolved) != 1 || !strings.Contains(unresolved[0], "hq.example.org") || !strings.Contains(unresolved[0], "no such host") {
		t.Fatalf("unresolved %v", unresolved)
	}
}

// What the status and the alert say about a tunnel that is not up.
func TestSitePeerNote(t *testing.T) {
	now := time.Now()
	me := siteLinkFacts{Port: 51873}
	withEP := siteLinkFacts{Port: 51873, Endpoint: "hq.example.org:51873"}
	for _, tc := range []struct {
		name string
		f    siteLinkFacts
		p    sitePeerFacts
		last time.Time
		want []string
	}{
		{"up", me, sitePeerFacts{Endpoint: "192.0.2.1:1"}, now.Add(-time.Minute), nil},
		{"no endpoint at either end", me, sitePeerFacts{}, time.Time{}, []string{"no endpoint on either node"}},
		{"name does not resolve", me, sitePeerFacts{Endpoint: "x.example:1", EndpointErr: "cannot look up x.example"}, time.Time{}, []string{"x.example:1 unusable (cannot look up x.example)", "this node has no endpoint"}},
		{"name does not resolve, it can dial", withEP, sitePeerFacts{Endpoint: "x.example:1", EndpointErr: "boom"}, time.Time{}, []string{"waiting for remote to connect to this node (hq.example.org:51873)"}},
		{"no uplink", me, sitePeerFacts{Endpoint: "192.0.2.1:1", NoRoute: true}, time.Time{}, []string{"no uplink route to 192.0.2.1:1", "its own internet"}},
		{"never, it dials", withEP, sitePeerFacts{}, time.Time{}, []string{"no handshake yet", "remote has no endpoint", "UDP port 51873"}},
		{"never, this node dials", me, sitePeerFacts{Endpoint: "192.0.2.1:1"}, time.Time{}, []string{"no handshake yet", "UDP to 192.0.2.1:1"}},
		{"just set up", siteLinkFacts{Port: 51873, Since: now.Add(-5 * time.Second)}, sitePeerFacts{Endpoint: "192.0.2.1:1"}, time.Time{}, nil},
		{"just set up, nothing to dial", siteLinkFacts{Port: 51873, Since: now.Add(-5 * time.Second)}, sitePeerFacts{}, time.Time{}, []string{"no endpoint on either node"}},
		{"stopped", withEP, sitePeerFacts{Endpoint: "192.0.2.1:1"}, now.Add(-10 * time.Minute), []string{"no handshake for 10 min", "192.0.2.1:1 or this node (hq.example.org:51873)"}},
	} {
		got := sitePeerNote(tc.f, tc.p, tc.last, now)
		if len(tc.want) == 0 && got != "" {
			t.Errorf("%s: %q, want nothing", tc.name, got)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", tc.name, got, w)
			}
		}
	}
}

func TestSiteLinkEffects(t *testing.T) {
	_, a := siteTestKey(t)
	_, b := siteTestKey(t)
	_, c := siteTestKey(t)
	cur := config.SiteLinkPlan{Members: []config.SiteMember{{Name: "hq-1", Site: "hq", PublicKey: a}, {Name: "f-1", Site: "field", PublicKey: b}}}
	next := config.SiteLinkPlan{Port: 4000, Members: []config.SiteMember{{Name: "hq-1", Site: "hq", PublicKey: a, Endpoint: "x.example:4000"}, {Name: "f-2", Site: "field", PublicKey: c}}}
	var got []string
	for _, e := range siteLinkEffects(cur, next, ProfileView{}) {
		got = append(got, e.Level+" "+e.Message)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{"members added: f-2 (site field)", "f-1 (site field) removed from the site link: tunnels close", "endpoint change: hq-1 (site hq)", "port or MTU change"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if es := siteLinkEffects(cur, cur, ProfileView{}); len(es) != 0 {
		t.Errorf("no change, effects %v", es)
	}
	one := config.SiteLinkPlan{Members: []config.SiteMember{{Site: "hq", PublicKey: a}}}
	// Members at one site, and nothing about the link changed: nothing
	// said (it used to ride along with every other profile change).
	if es := siteLinkEffects(one, one, ProfileView{}); len(es) != 0 {
		t.Errorf("one site, no change: %v", es)
	}
	// A port with no members, before or after: no tunnel restarts.
	if es := siteLinkEffects(config.SiteLinkPlan{}, config.SiteLinkPlan{Port: 4000}, ProfileView{}); len(es) != 0 {
		t.Errorf("settings without members: %v", es)
	}
	if es := siteLinkEffects(config.SiteLinkPlan{}, one, ProfileView{}); len(es) != 2 || es[1].Code != "site-link-one-site" {
		t.Errorf("one site: %v", es)
	}
	// Two sites, no endpoint at either: said. Both members in this mesh
	// already, with no link to explain it: said too.
	two := config.SiteLinkPlan{Members: []config.SiteMember{{Site: "hq", PublicKey: a}, {Site: "field", PublicKey: b}}}
	view := ProfileView{Callsign: "node-1", SiteKey: a, Nodes: []status.MeshNode{{Callsign: "node-2", SiteKey: b}}}
	codes := func(es []status.Warning) string {
		var out []string
		for _, e := range es {
			out = append(out, e.Code)
		}
		return strings.Join(out, " ")
	}
	es := siteLinkEffects(config.SiteLinkPlan{}, two, view)
	if got := codes(es); got != "site-link-members site-link-no-endpoint site-link-same-mesh" {
		t.Errorf("two nodes of one mesh: %s", got)
	}
	if m := es[2].Message; !strings.Contains(m, "node-1 (site hq) and node-2 (site field) already connected") {
		t.Errorf("same mesh: %s", m)
	}
	// With an endpoint, and a node this mesh does not hold: neither.
	two.Members[1] = config.SiteMember{Site: "field", PublicKey: c, Endpoint: "f.example:51873"}
	if got := codes(siteLinkEffects(config.SiteLinkPlan{}, two, view)); got != "site-link-members" {
		t.Errorf("a far member with an endpoint: %s", got)
	}
	// A link in force explains nodes of two sites in one mesh.
	linked := config.SiteLinkPlan{Members: []config.SiteMember{{Site: "hq", PublicKey: a, Endpoint: "h.example:1"}, {Site: "field", PublicKey: b}}}
	more := config.SiteLinkPlan{Members: append(slices.Clone(linked.Members), config.SiteMember{Site: "field", PublicKey: c})}
	if got := codes(siteLinkEffects(linked, more, view)); got != "site-link-members" {
		t.Errorf("a member added to a link in force: %s", got)
	}
}

// A route written without a type is the unicast route the kernel lists:
// compared otherwise, it was deleted and added again on every pass.
func TestRouteKey(t *testing.T) {
	dst := &net.IPNet{IP: net.IPv4(169, 254, 20, 1).To4(), Mask: net.CIDRMask(32, 32)}
	src := net.IPv4(169, 254, 10, 1).To4()
	want := netlink.Route{Table: siteTable, LinkIndex: 7, Scope: netlink.SCOPE_LINK, Dst: dst, Src: src}
	kernel := netlink.Route{Table: siteTable, LinkIndex: 7, Scope: netlink.SCOPE_LINK, Dst: dst, Src: src, Type: unix.RTN_UNICAST, Protocol: unix.RTPROT_BOOT}
	if routeKey(want) != routeKey(kernel) {
		t.Errorf("%q != %q", routeKey(want), routeKey(kernel))
	}
	other := kernel
	other.LinkIndex = 8
	stop := netlink.Route{Table: siteTableStop, Type: unix.RTN_UNREACHABLE, Dst: &net.IPNet{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)}}
	stopKernel := netlink.Route{Table: siteTableStop, Type: unix.RTN_UNREACHABLE}
	if routeKey(other) == routeKey(kernel) || routeKey(stop) == routeKey(kernel) {
		t.Error("different routes compare equal")
	}
	if routeKey(stop) != routeKey(stopKernel) {
		t.Errorf("a default route with and without its destination: %q != %q", routeKey(stop), routeKey(stopKernel))
	}
}

func TestSiteTunnelState(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name      string
		handshake time.Time
		up, lost  int
	}{
		{"never came up", time.Time{}, 0, 0},
		{"up", now.Add(-time.Minute), 1, 0},
		{"down for five minutes", now.Add(-5 * time.Minute), 0, 1},
		{"down for 23 hours", now.Add(-23 * time.Hour), 0, 1},
		{"down for two days: the nodes behind it are forgotten", now.Add(-48 * time.Hour), 0, 0},
	} {
		if up, lost := siteTunnelState(c.handshake, now); up != c.up || lost != c.lost {
			t.Errorf("%s: up %d lost %d, want %d %d", c.name, up, lost, c.up, c.lost)
		}
	}
}
