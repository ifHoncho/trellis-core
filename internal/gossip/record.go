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
	"math"
	"net"
	"regexp"
	"strconv"
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
	// Links are the mesh profile's links as this node runs them, in the
	// profile's order: "band/channel/width" ("5g/149/80"). Nodes compare
	// them with their own: two nodes whose profiles differ on a link meet
	// badly or not at all on it. Absent from older versions.
	Links []string `json:"lk,omitempty"`
	// Time says whether the node serves time to the mesh:
	// TimeGNSS when its clock follows its own GNSS receiver, TimeServer
	// when it serves time from another source; empty when it does not
	// serve (no chrony, or an older version).
	Time string `json:"tm,omitempty"`
	// ProfileRev is the revision of the mesh profile the node runs
	// (config.Profile.Revision): a node behind asks one ahead for it.
	ProfileRev uint64 `json:"pr,omitempty"`
	// ProfileChange is the ID of the mesh-wide change that made that
	// revision (config.ChangeInfo), when it has one: a node that does not
	// know it asks for the profile, and two parts of a mesh that changed it
	// apart find out when they meet.
	ProfileChange string `json:"pc,omitempty"`
	// MeshChange: the node follows mesh-wide profile changes (offers).
	MeshChange bool `json:"mc,omitempty"`
	// Regulatory is the profile's regulatory setting: "unrestricted", or
	// "country:US". A mesh-wide change that alters it on another node says
	// so before it is sent.
	Regulatory string `json:"rg,omitempty"`
	// Load is how busy each mesh link's channel is where this node is, in
	// percent, in the profile's order; -1 for a link it has no radio on or
	// whose driver does not measure it.
	Load []int `json:"ld,omitempty"`
	// Pos is the node's GNSS position while it has a fix and shares it.
	// It rides on the refresh, not on every move: the node's own ATAK
	// reports carry the live position; this is what the other nodes keep
	// as its last known one.
	Pos *Position `json:"ps,omitempty"`
	// NoPos: the node does not share its position (positions.share off).
	// The others forget its last known one at once.
	NoPos bool `json:"np,omitempty"`
	// Arch is the node's Debian architecture (amd64, arm64, armhf): which
	// package of a mesh-wide upgrade it takes.
	Arch string `json:"ar,omitempty"`
	// Rollouts: the node takes mesh-wide upgrades (a package install that
	// trusts a release key).
	Rollouts bool `json:"ru,omitempty"`
	// Rollout is the node's progress in the mesh-wide upgrade under way
	// (internal/rollout), or nil.
	Rollout *RolloutState `json:"ro,omitempty"`
}

// RolloutState is a node's progress in a mesh-wide upgrade.
type RolloutState struct {
	ID     string `json:"i"`
	Phase  string `json:"p"`
	Reason string `json:"r,omitempty"` // why it skipped, failed or halted; short
}

// MaxRolloutReason bounds a rollout reason in a record.
const MaxRolloutReason = 120

// Position is a node's GNSS fix, compact.
type Position struct {
	Lat float64 `json:"la"`
	Lon float64 `json:"lo"`
	// Alt is the height above the ellipsoid in metres; HasAlt says it is
	// known.
	Alt    float64 `json:"al,omitempty"`
	HasAlt bool    `json:"ha,omitempty"`
	// Acc is the horizontal error in metres (95%), 0 when unknown.
	Acc int `json:"ac,omitempty"`
	// Fix is 2 (2D) or 3 (3D); Time the fix's time, Unix seconds.
	Fix  int   `json:"f"`
	Time int64 `json:"t"`
}

// NewPosition rounds a fix for gossip: about a metre, which is finer than
// any receiver's error.
func NewPosition(lat, lon, alt float64, hasAlt bool, acc float64, fix int, t int64) *Position {
	r := func(v float64, d float64) float64 { return math.Round(v*d) / d }
	p := &Position{Lat: r(lat, 1e5), Lon: r(lon, 1e5), Acc: int(math.Round(acc)), Fix: fix, Time: t}
	if hasAlt {
		p.Alt, p.HasAlt = r(alt, 10), true
	}
	return p
}

func (p *Position) validate() error {
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	switch {
	case !finite(p.Lat) || !finite(p.Lon) || !finite(p.Alt) || p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180:
		return fmt.Errorf("position %v,%v out of range", p.Lat, p.Lon)
	case p.Alt < -1000 || p.Alt > 100000:
		return fmt.Errorf("height %v out of range", p.Alt)
	case p.Acc < 0 || p.Acc > 1000000:
		return fmt.Errorf("position error %d out of range", p.Acc)
	case p.Fix != 2 && p.Fix != 3:
		return fmt.Errorf("fix %d is not 2 or 3", p.Fix)
	case p.Time < 0:
		return fmt.Errorf("fix time %d out of range", p.Time)
	}
	return nil
}

// RegulatoryEntry is a profile's regulatory setting as a Regulatory value.
func RegulatoryEntry(mode, country string) string {
	if mode == "unrestricted" {
		return mode
	}
	return mode + ":" + country
}

// Time values.
const (
	TimeGNSS   = "gnss"
	TimeServer = "ntp"
)

// MaxRadios bounds the radios one record lists.
const MaxRadios = 8

// MaxLinkEntries bounds the links one record lists.
const MaxLinkEntries = 10

// LinkEntry is one Links entry.
func LinkEntry(band string, channel, widthMHz int) string {
	return fmt.Sprintf("%s/%d/%d", band, channel, widthMHz)
}

// ParseLinkEntry is the inverse of LinkEntry.
func ParseLinkEntry(e string) (band string, channel, widthMHz int, ok bool) {
	parts := strings.Split(e, "/")
	if len(parts) != 3 {
		return "", 0, 0, false
	}
	c, err1 := strconv.Atoi(parts[1])
	w, err2 := strconv.Atoi(parts[2])
	// Channel and width are positive and modest; the band is a short
	// lower-case name ("5g", "s1g").
	if err1 != nil || err2 != nil || c < 1 || c > 10000 || w < 1 || w > 10000 || !reBand.MatchString(parts[0]) {
		return "", 0, 0, false
	}
	return parts[0], c, w, true
}

var reBand = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

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
	r.MAC = canonMAC(r.MAC)
	for i := range r.Orig {
		r.Orig[i] = canonMAC(r.Orig[i])
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
	// 1–254, as everywhere else: ID 0 would be the mesh's anycast address
	// (10.73.0.1), routing table 7400 and mark 0x7300.
	if r.ID < 1 || r.ID > 254 {
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
	if !printable(r.Regulatory, 32) {
		return fmt.Errorf("regulatory %q not short printable text", r.Regulatory)
	}
	if !printable(r.Arch, 16) {
		return fmt.Errorf("architecture %q not short printable text", r.Arch)
	}
	if ro := r.Rollout; ro != nil {
		if ro.ID == "" || !printable(ro.ID, 32) || ro.Phase == "" || !printable(ro.Phase, 16) || !sentence(ro.Reason, MaxRolloutReason) {
			return fmt.Errorf("bad rollout state")
		}
	}
	if r.Time != "" && r.Time != TimeGNSS && r.Time != TimeServer {
		return fmt.Errorf("time %q is not %s or %s", r.Time, TimeGNSS, TimeServer)
	}
	if len(r.Load) > MaxLinkEntries {
		return fmt.Errorf("%d loads listed, over the limit of %d", len(r.Load), MaxLinkEntries)
	}
	for _, l := range r.Load {
		if l < -1 || l > 100 {
			return fmt.Errorf("channel load %d out of range", l)
		}
	}
	if r.Pos != nil {
		if err := r.Pos.validate(); err != nil {
			return err
		}
	}
	if len(r.Links) > MaxLinkEntries {
		return fmt.Errorf("%d links listed, over the limit of %d", len(r.Links), MaxLinkEntries)
	}
	for _, e := range r.Links {
		if _, _, _, ok := ParseLinkEntry(e); !ok || !printable(e, 24) {
			return fmt.Errorf("link %q is not band/channel/width", e)
		}
	}
	if r.ProfileChange != "" && !validChangeID(r.ProfileChange) {
		return fmt.Errorf("profile change %q is not 16 hex digits", r.ProfileChange)
	}
	if !printable(r.Version, 64) || !printable(r.IDSource, 16) {
		return fmt.Errorf("version or id source not short printable text")
	}
	return nil
}

// printable reports whether s is at most max bytes of printable ASCII
// without spaces.
// sentence is like printable, with spaces allowed.
func sentence(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

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
	// Channel load changes with every sample, and a position with every
	// move: they ride on the refresh. Gaining or losing a fix is a change.
	x.Load, y.Load = nil, nil
	if x.Pos != nil {
		x.Pos = &Position{}
	}
	if y.Pos != nil {
		y.Pos = &Position{}
	}
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

// validChangeID is config.ValidChangeID, kept here so that gossip does not
// import config.
func validChangeID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}
