package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Site link: parts of one mesh in different places, joined over the
// internet (architecture §13, Site link). Members are listed here by their
// WireGuard public key; a node whose own key is listed tunnels to every
// member of another site, and the tunnels are batman-adv interfaces like
// radio links.

// Site link defaults.
const (
	SiteLinkDefaultPort = 51873
	SiteLinkDefaultMbit = 10
	SiteLinkDefaultMTU  = 1420
	// SiteLinkDefaultMulticastKbit is the default budget per tunnel and
	// direction for what one site broadcasts to another.
	SiteLinkDefaultMulticastKbit = 1000
	// SiteLinkMaxMembers bounds the list: each member of another site is
	// a tunnel and an interface on every member.
	SiteLinkMaxMembers = 64
	// SiteTunnelOverhead is what a gretap frame adds inside WireGuard:
	// IPv4 (20), GRE (4) and the Ethernet header (14).
	SiteTunnelOverhead = 38
)

// SiteLinkPlan is the profile's site link section.
type SiteLinkPlan struct {
	// Port is the UDP port every member listens on; default 51873.
	Port int `yaml:"port,omitempty" json:"port,omitempty"`
	// Mbit is the throughput batman-adv is told a tunnel has (a tunnel
	// reports none, and batman-adv would take 1 Mbit/s); default 10.
	Mbit int `yaml:"mbit,omitempty" json:"mbit,omitempty"`
	// MTU is the WireGuard interface's; default 1420. The tunnels inside
	// it carry 38 bytes less.
	MTU int `yaml:"mtu,omitempty" json:"mtu,omitempty"`
	// MulticastKbit bounds the broadcast and multicast a tunnel carries,
	// each way (kbit/s; default 1000): a busy site cannot fill another's
	// radios with its video or chatter. Unicast is not limited.
	MulticastKbit int          `yaml:"multicast_kbit,omitempty" json:"multicast_kbit,omitempty"`
	Members       []SiteMember `yaml:"members,omitempty" json:"members,omitempty"`
}

// SiteMember is a node that holds the site link.
type SiteMember struct {
	// Name is a label, usually the node's callsign.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// Site names where the node is: members of one site do not tunnel to
	// each other (they meet over the air).
	Site string `yaml:"site" json:"site"`
	// PublicKey is the node's WireGuard public key (meshctl site).
	PublicKey string `yaml:"public_key" json:"public_key"`
	// Endpoint is where the other sites reach it, host:port. Without one
	// it only dials out (behind NAT): then the members it tunnels to need
	// one.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
}

// ListenPort is the port, default filled.
func (s SiteLinkPlan) ListenPort() int {
	if s.Port == 0 {
		return SiteLinkDefaultPort
	}
	return s.Port
}

// ThroughputMbit is the tunnels' throughput for batman-adv, default filled.
func (s SiteLinkPlan) ThroughputMbit() int {
	if s.Mbit == 0 {
		return SiteLinkDefaultMbit
	}
	return s.Mbit
}

// LinkMTU is the WireGuard interface's MTU, default filled.
func (s SiteLinkPlan) LinkMTU() int {
	if s.MTU == 0 {
		return SiteLinkDefaultMTU
	}
	return s.MTU
}

// MulticastBudgetKbit is the broadcast and multicast budget, default filled.
func (s SiteLinkPlan) MulticastBudgetKbit() int {
	if s.MulticastKbit == 0 {
		return SiteLinkDefaultMulticastKbit
	}
	return s.MulticastKbit
}

// Member returns the member with this public key.
func (s SiteLinkPlan) Member(publicKey string) (SiteMember, bool) {
	for _, m := range s.Members {
		if m.PublicKey == publicKey {
			return m, true
		}
	}
	return SiteMember{}, false
}

// Peers are the members a member with this key tunnels to: those of every
// other site. None when the key is not a member.
func (s SiteLinkPlan) Peers(publicKey string) []SiteMember {
	me, ok := s.Member(publicKey)
	if !ok {
		return nil
	}
	var out []SiteMember
	for _, m := range s.Members {
		if m.Site != me.Site {
			out = append(out, m)
		}
	}
	return out
}

func siteKeyHash(publicKey string) [32]byte {
	b, _ := base64.StdEncoding.DecodeString(publicKey)
	return sha256.Sum256(b)
}

// SiteAddr is a member's tunnel address: 169.254.a.b, from a hash of its
// public key, outside the link-local range's reserved first and last /24.
func SiteAddr(publicKey string) netip.Addr {
	h := siteKeyHash(publicKey)
	return netip.AddrFrom4([4]byte{169, 254, 1 + h[0]%254, h[1]})
}

// SiteIfname is the name of the tunnel to a member: site-<6 hex>.
func SiteIfname(publicKey string) string {
	h := siteKeyHash(publicKey)
	return "site-" + hex.EncodeToString(h[2:5])
}

// SiteTunnelMAC is the address of the tunnel a member (from) holds to
// another (to): from a hash of the two keys, locally administered. Every
// node that has the profile can work it out, so a member's tunnels need
// not be listed in its gossip record, however many it holds.
func SiteTunnelMAC(from, to string) net.HardwareAddr {
	a, _ := base64.StdEncoding.DecodeString(from)
	b, _ := base64.StdEncoding.DecodeString(to)
	h := sha256.Sum256(append(append([]byte("trellis site tunnel\x00"), a...), b...))
	mac := net.HardwareAddr(h[:6])
	mac[0] = mac[0]&0xfc | 0x02
	return mac
}

// TunnelMACs are the addresses of the tunnels a member with this key
// holds; none when the key is not a member.
func (s SiteLinkPlan) TunnelMACs(publicKey string) []string {
	var out []string
	for _, p := range s.Peers(publicKey) {
		out = append(out, SiteTunnelMAC(publicKey, p.PublicKey).String())
	}
	return out
}

var reSiteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

func (s *SiteLinkPlan) validate(es *Errors) {
	if s.Port != 0 && (s.Port < 1 || s.Port > 65535) {
		es.add("site_link.port", "must be 1–65535 (got %d)", s.Port)
	}
	if s.Mbit != 0 && (s.Mbit < 1 || s.Mbit > 100000) {
		es.add("site_link.mbit", "must be 1–100000 (got %d)", s.Mbit)
	}
	if min := 1280 + SiteTunnelOverhead; s.MTU != 0 && (s.MTU < min || s.MTU > 9000) {
		es.add("site_link.mtu", "must be %d–9000, so that a tunnel carries 1280 bytes (got %d)", min, s.MTU)
	}
	if s.MulticastKbit != 0 && (s.MulticastKbit < 16 || s.MulticastKbit > 1000000) {
		es.add("site_link.multicast_kbit", "must be 16–1000000 (got %d)", s.MulticastKbit)
	}
	if len(s.Members) > SiteLinkMaxMembers {
		es.add("site_link.members", "at most %d members (got %d)", SiteLinkMaxMembers, len(s.Members))
	}
	keys, addrs, names := map[string]int{}, map[netip.Addr]int{}, map[string]int{}
	// Site names are compared as written: "HQ" and "hq" would be two
	// sites, whose members link to each other.
	spelled := map[string]string{}
	for i, m := range s.Members {
		f := fmt.Sprintf("site_link.members[%d]", i)
		if !reSiteName.MatchString(m.Site) {
			es.add(f+".site", "must be 1–32 letters, digits, '.', '_' or '-' (got %q)", m.Site)
		} else if first, ok := spelled[strings.ToLower(m.Site)]; ok && first != m.Site {
			es.add(f+".site", "%q and %q differ only in case, and would be two sites: write one site's name one way", m.Site, first)
		} else {
			spelled[strings.ToLower(m.Site)] = m.Site
		}
		if len(m.Name) > 32 {
			es.add(f+".name", "at most 32 characters (got %d)", len(m.Name))
		} else if err := printableASCII(m.Name); err != nil {
			es.add(f+".name", "%v", err)
		}
		if m.Endpoint != "" {
			if err := checkEndpoint(m.Endpoint); err != nil {
				es.add(f+".endpoint", "%v", err)
			}
		}
		if b, err := base64.StdEncoding.Strict().DecodeString(m.PublicKey); err != nil || len(b) != 32 {
			es.add(f+".public_key", "must be a WireGuard public key, 44 characters of base64 (meshctl site prints the node's)")
			continue
		}
		if j, ok := keys[m.PublicKey]; ok {
			es.add(f+".public_key", "the same key as members[%d]", j)
			continue
		}
		keys[m.PublicKey] = i
		if j, ok := addrs[SiteAddr(m.PublicKey)]; ok {
			es.add(f+".public_key", "its tunnel address %s is also members[%d]'s: give one of the two nodes a new key (meshctl site --new-key)", SiteAddr(m.PublicKey), j)
		}
		addrs[SiteAddr(m.PublicKey)] = i
		if j, ok := names[SiteIfname(m.PublicKey)]; ok {
			es.add(f+".public_key", "its tunnel's name %s is also members[%d]'s: give one of the two nodes a new key (meshctl site --new-key)", SiteIfname(m.PublicKey), j)
		}
		names[SiteIfname(m.PublicKey)] = i
	}
}

// checkEndpoint checks host:port, the host a name or an address.
func checkEndpoint(ep string) error {
	host, port, err := net.SplitHostPort(ep)
	if err != nil {
		return fmt.Errorf("must be host:port, [IPv6]:port for an IPv6 address (got %q)", ep)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port must be 1–65535 (got %q)", port)
	}
	if host == "" {
		return fmt.Errorf("no host in %q", ep)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return fmt.Errorf("an address with a zone (%%%s) only means something on one node: give an address every site can reach", a.Zone())
		}
		return nil
	}
	if len(host) > 253 || !reHostname.MatchString(host) {
		return fmt.Errorf("host %q is neither an address nor a DNS name", host)
	}
	return nil
}

var reHostname = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.?$`)
