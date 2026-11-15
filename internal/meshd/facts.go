package meshd

import (
	"net"
	"sync"
	"time"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/radio"
	"meridian/trellis/internal/status"
)

// Facts is the state reconcilers learn and share, and that the status
// endpoint reads. Slow-changing facts live here; tables that change every
// second (peers, originators) are read live when status is requested.
type Facts struct {
	mu sync.RWMutex

	// Firewall is what the firewall reconciler found in force.
	Firewall firewallFacts

	Radios      []*radio.Radio
	Plan        *radio.Plan
	Roles       map[string]radio.Role // active roles by fingerprint
	GraceActive bool

	MeshIfname  string // "" when no mesh interface exists
	MeshIfindex int
	MeshWiphy   uint32
	MeshMTU     int
	APIfname    string // "" when no AP interface exists
	APIfindex   int
	APWiphy     uint32
	APShared    bool // the AP runs on a mesh radio
	// APLink is the mesh link whose radio the AP shares (0 is the first),
	// meaningful while APShared.
	APLink int

	// MoreLinks are the mesh links after the first (mesh1, mesh2, …; the
	// profile's links): MoreLinks[0] is link 1. Use moreLink.
	MoreLinks []meshLinkFacts

	Supplicant  status.Supplicant
	MeshFreqMHz int
	// MeshWidthMHz is the width mesh0 runs at (0 until it has started).
	MeshWidthMHz int
	TXPowerDBm   float64
	SAEFailures  []status.SAEEvent
	Transport    string

	Regulatory status.Regulatory

	BatPresent    bool
	BatIfindex    int
	BatModule     string
	BatModulePath string
	BatInferred   bool
	BatMismatch   bool
	BatAlgo       string
	BatMTU        int
	BatMAC        string

	BridgePresent bool
	BridgeIfindex int
	BridgeAddress string
	BridgeMTU     int

	Hostname    string
	GossipGW    *gossip.GW          // this node's gateway state, as gossip publishes it
	Gateway     status.GatewayState // static part of the gateway status
	DNSUpstream []string            // resolvers dnsmasq forwards non-.mesh names to

	Ports       []status.Port // wired ports and their roles (ports reconciler)
	AutoClients []string      // auto ports that act as client ports
	AutoWAN     *autoWAN      // an auto port holding a WAN lease, or nil

	AP      status.AP // static part; clients are read live
	Anycast status.Anycast
	Filters status.Filters // static part; drop counters are read live
	DHCP    status.DHCP
}

// Update runs fn with the write lock held.
func (f *Facts) Update(fn func(*Facts)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// Read runs fn with the read lock held.
func (f *Facts) Read(fn func(*Facts)) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	fn(f)
}

// AddSAEFailure records a rejected peer on a link's interface, keeping the
// last 20 of all links.
func (f *Facts) AddSAEFailure(ifname, mac, kind, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.SAEFailures = append(f.SAEFailures, status.SAEEvent{Time: time.Now(), Interface: ifname, MAC: mac, Kind: kind, Message: msg})
	if len(f.SAEFailures) > 20 {
		f.SAEFailures = f.SAEFailures[len(f.SAEFailures)-20:]
	}
}

// saeFailures are the recorded failures on interface ifname; first adds
// those recorded without one.
func (f *Facts) saeFailures(ifname string, first bool) []status.SAEEvent {
	out := []status.SAEEvent{}
	for _, e := range f.SAEFailures {
		if (ifname != "" && e.Interface == ifname) || (first && e.Interface == "") {
			out = append(out, e)
		}
	}
	return out
}

func (f *Facts) bridgeAddress() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.BridgeAddress
}

// autoWAN is an auto port that became a WAN (architecture §7.1): meshd holds
// its DHCP lease and routes mesh clients out through it from a table of its
// own, never from the main table.
type autoWAN struct {
	Port   string
	Addr   *net.IPNet // leased address and mask
	Router net.IP
	DNS    []net.IP
}

// clientPorts are the ports bridged into br-mesh as client ports: those
// configured as client, and auto ports that became one.
func (d *Daemon) clientPorts() []string {
	out := append([]string{}, d.cfg.ClientPorts()...)
	d.facts.Read(func(f *Facts) { out = append(out, f.AutoClients...) })
	return out
}

// meshLinkFacts describes one mesh link beyond the first (mesh0's facts
// are the Mesh* fields above, kept as they were).
type meshLinkFacts struct {
	Ifname     string // "" when the interface does not exist
	Ifindex    int
	Wiphy      uint32
	MTU        int
	Supplicant status.Supplicant
	FreqMHz    int
	WidthMHz   int // what the interface runs at, 0 until it has started
	TXPowerDBm float64
}

// linkFacts returns a copy of the facts of mesh link i (1 or more), zero
// when there are none yet. Safe under the read lock: it changes nothing.
func (f *Facts) linkFacts(i int) meshLinkFacts {
	if i < 1 || i > len(f.MoreLinks) {
		return meshLinkFacts{}
	}
	return f.MoreLinks[i-1]
}

// moreLink returns the facts of mesh link i (1 or more), growing the list
// as needed. The caller holds the write lock (facts.Update): it may append.
func (f *Facts) moreLink(i int) *meshLinkFacts {
	for len(f.MoreLinks) < i {
		f.MoreLinks = append(f.MoreLinks, meshLinkFacts{})
	}
	return &f.MoreLinks[i-1]
}
