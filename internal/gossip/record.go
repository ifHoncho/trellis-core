// Package gossip is Trellis's node-to-node control data (architecture §12.5;
// custom, built into meshd).
//
// Every node publishes one small record: who it is (identity MAC, node ID,
// callsign, mesh address, the MACs of its mesh interfaces so batman-adv
// originators can be mapped to it), its version, and its gateway state. It
// is sent to a mesh multicast group on change, on a slow refresh, and soon
// after a new node is heard, so newcomers converge in seconds.
//
// Records are authenticated with HMAC-SHA256 under a key derived from the
// mesh passphrase (architecture §11): end-user devices share the client
// network with gossip but do not know the mesh secret. Replays are refused
// by a per-sender (boot, sequence) pair. Client ports also filter the
// gossip port in both directions (architecture §7.7), so EUDs neither see
// nor inject it.
package gossip

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// Wire constants.
const (
	Magic     = "TRG1"
	MACLen    = 16 // truncated HMAC-SHA256
	MaxPacket = 1200
	Port      = 7373
	Group     = "239.73.73.73"
)

// Record is one node's published state.
type Record struct {
	V        int      `json:"v"`
	MAC      string   `json:"mac"`            // identity MAC (bat0, br-mesh)
	Orig     []string `json:"orig,omitempty"` // mesh hard-interface MACs (batman originator addresses)
	ID       int      `json:"id"`
	IDSource string   `json:"ids,omitempty"` // config or derived
	Callsign string   `json:"cs"`
	Addr     string   `json:"addr"` // node address, 10.73.N.1
	Version  string   `json:"ver,omitempty"`
	Boot     uint64   `json:"boot"` // random per meshd start
	Seq      uint64   `json:"seq"`  // increases with every record sent in one boot
	GW       *GW      `json:"gw,omitempty"`
	// Radios lists the radios that can carry a mesh link, one entry each:
	// the radio's bands joined with "+" ("2g+5g", "s1g"). It tells the rest
	// of the mesh which links this node could follow a profile change to; a
	// node that sends none (an older version) is simply not known.
	Radios []string `json:"mr,omitempty"`
}

// MaxRadios bounds the radios one record lists.
const MaxRadios = 8

// RadioEntry is a radio's bands as one Radios entry.
func RadioEntry(bands []string) string { return strings.Join(bands, "+") }

// RadioBands is the inverse of RadioEntry.
func RadioBands(entry string) []string { return strings.Split(entry, "+") }

// GW is the gateway part of a record: present while the node has a WAN.
type GW struct {
	Healthy  bool   `json:"ok"`
	DownKbps uint32 `json:"down,omitempty"`
	UpKbps   uint32 `json:"up,omitempty"`
	RTTms    int    `json:"rtt,omitempty"`
	LossPct  int    `json:"loss,omitempty"`
}

// Key derives the gossip key from the mesh passphrase and mesh ID.
func Key(passphrase, meshID string) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(passphrase), []byte(meshID), "trellis gossip v1", 32)
}

// Seal encodes and authenticates a record.
func Seal(key []byte, r *Record) ([]byte, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	pkt := make([]byte, 0, len(Magic)+len(body)+MACLen)
	pkt = append(pkt, Magic...)
	pkt = append(pkt, body...)
	m := hmac.New(sha256.New, key)
	m.Write(pkt)
	pkt = append(pkt, m.Sum(nil)[:MACLen]...)
	if len(pkt) > MaxPacket {
		return nil, fmt.Errorf("gossip record is %d bytes, over the %d-byte cap", len(pkt), MaxPacket)
	}
	return pkt, nil
}

// Errors from Open.
var (
	ErrNotGossip = errors.New("not a gossip packet")
	ErrAuth      = errors.New("gossip packet failed authentication")
	ErrInvalid   = errors.New("invalid gossip record")
)

// Open verifies and decodes a packet.
func Open(key []byte, pkt []byte) (*Record, error) {
	if len(pkt) < len(Magic)+2+MACLen || !bytes.HasPrefix(pkt, []byte(Magic)) {
		return nil, ErrNotGossip
	}
	signed, tag := pkt[:len(pkt)-MACLen], pkt[len(pkt)-MACLen:]
	m := hmac.New(sha256.New, key)
	m.Write(signed)
	if !hmac.Equal(tag, m.Sum(nil)[:MACLen]) {
		return nil, ErrAuth
	}
	var r Record
	if err := json.Unmarshal(signed[len(Magic):], &r); err != nil {
		return nil, fmt.Errorf("gossip record: %w", err)
	}
	if r.MAC == "" || r.Boot == 0 {
		return nil, fmt.Errorf("%w: no identity or boot id", ErrInvalid)
	}
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return &r, nil
}

// reCallsign is config's callsign rule (a DNS label).
var reCallsign = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validate checks the fields other nodes' records feed into this node's
// configuration. Authentication only proves a record comes from a mesh
// member; a buggy or out-of-date member must still not be able to write
// extra lines into dnsmasq's hosts or servers files (callsign, address),
// shift firewall marks (the ID is ORed into 0x7300), or put control
// characters on an operator's terminal (meshctl prints version and ID
// source).
func (r *Record) validate() error {
	if m, err := net.ParseMAC(r.MAC); err != nil || len(m) != 6 {
		return fmt.Errorf("bad mac %q", r.MAC)
	}
	for _, o := range r.Orig {
		if m, err := net.ParseMAC(o); err != nil || len(m) != 6 {
			return fmt.Errorf("bad originator mac %q", o)
		}
	}
	if r.ID < 0 || r.ID > 254 {
		return fmt.Errorf("node id %d out of range", r.ID)
	}
	if r.Callsign != "" && !reCallsign.MatchString(r.Callsign) {
		return fmt.Errorf("callsign %q is not a DNS label", r.Callsign)
	}
	if r.Addr != "" {
		if ip := net.ParseIP(r.Addr); ip == nil || ip.To4() == nil {
			return fmt.Errorf("address %q is not IPv4", r.Addr)
		}
	}
	if len(r.Radios) > MaxRadios {
		return fmt.Errorf("%d radios listed, over the limit of %d", len(r.Radios), MaxRadios)
	}
	for _, e := range r.Radios {
		if e == "" || !printable(e, 24) {
			return fmt.Errorf("radio bands %q not short printable text", e)
		}
	}
	if !printable(r.Version, 64) || !printable(r.IDSource, 16) {
		return fmt.Errorf("version or id source not short printable text")
	}
	return nil
}

// printable reports whether s is at most max bytes of printable ASCII
// without spaces.
func printable(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// Same reports whether two records carry the same state. The sequence
// number and the WAN's live figures (RTT, loss) are ignored: they ride
// along on the periodic refresh instead of triggering a send on every
// probe.
func Same(a, b *Record) bool {
	if a == nil || b == nil {
		return a == b
	}
	x, y := *a, *b
	x.Seq, y.Seq = 0, 0
	if x.GW != nil {
		g := *x.GW
		g.RTTms, g.LossPct = 0, 0
		x.GW = &g
	}
	if y.GW != nil {
		g := *y.GW
		g.RTTms, g.LossPct = 0, 0
		y.GW = &g
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return bytes.Equal(ja, jb)
}
