// Command fakemeshd serves meshd's API from made-up data, for working on
// the web UI without a mesh (make ui-dev). It is a development tool and is
// not part of the trellis binary.
//
// The data is a seven-node mesh seen from "relay-1": a dual-band card and
// a HaLow module, a few end-user devices, three gateways (one
// with a dead uplink) and a node two hops away. Figures drift a little on
// every request so the UI has something live to draw. Configuration
// changes are kept in memory and answered with plausible effects.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/hostapd"
	"meridian/trellis/internal/hoststat"
	"meridian/trellis/internal/meshd"
	"meridian/trellis/internal/qr"
	"meridian/trellis/internal/status"
)

type fnode struct {
	callsign  string
	id        int
	mac, orig string
	gw, wanOK bool
	clients   int
	sel       int
}

// radios are the bands of the node's mesh radios, as it would report them:
// most have one dual-band adapter, two only 2.4 GHz, and scout-7 runs a
// build too old to say.
func (n fnode) radios() [][]string {
	switch n.callsign {
	case "bravo-5", "alpha-4":
		return [][]string{{"2g"}}
	case "scout-7":
		return nil
	case "truck-3":
		return [][]string{{"2g"}, {"5g", "6g"}, {"s1g"}}
	}
	return [][]string{{"2g", "5g"}}
}

type flink struct {
	a, b       int // indexes into nodes
	ab, ba     float64
	sigA, sigB int
}

var nodes = []fnode{
	{"relay-1", 12, "6a:dc:ee:10:00:01", "a0:b0:c0:00:89:fa", true, true, 3, 12},
	{"ridge-2", 27, "6a:dc:ee:10:00:02", "a0:b0:c0:20:2d:26", false, false, 1, 12},
	{"truck-3", 41, "6a:dc:ee:10:00:03", "a0:b0:c0:30:5c:f1", true, true, 4, 41},
	{"alpha-4", 58, "6a:dc:ee:10:00:04", "a0:b0:c0:40:dc:bb", false, false, 2, 12},
	{"bravo-5", 73, "6a:dc:ee:10:00:05", "00:c0:ca:50:11:0e", false, false, 1, 41},
	{"ops-6", 96, "6a:dc:ee:10:00:06", "00:c0:ca:60:42:9a", true, false, 6, 41},
	{"scout-7", 130, "6a:dc:ee:10:00:07", "a0:b0:c0:70:7a:ea", false, false, 1, 12},
}

// lastLinks are links on the profile's last link, when it has more than
// one: nodes that meet on two bands, as a dual-band node does with its
// neighbours. Their ends use each node's second interface address.
var lastLinks = []flink{{0, 1, 31, 29, -48, -50}, {0, 4, 18, 16, -66, -69}, {1, 3, 27, 25, -55, -57}}

// linkMAC is a node's interface address on its second mesh link.
func linkMAC(orig string) string { return "a2" + orig[2:] }

var links = []flink{
	{0, 1, 86, 79, -52, -55}, {0, 2, 41, 38, -67, -66}, {0, 3, 120, 115, -44, -45}, {0, 4, 12, 9, -79, -81},
	{1, 6, 22, 19, -74, -75}, {1, 3, 64, 60, -58, -59}, {2, 5, 54, 51, -61, -62}, {2, 4, 33, 30, -68, -70},
	{3, 4, 48, 44, -63, -64}, {5, 4, 7, 5, -84, -86},
}

type fake struct {
	mu       sync.Mutex
	started  time.Time
	node     config.Node
	profile  config.Profile
	setup    bool
	password string
	fails    int
	mcast    uint64
	// A made-up mesh-wide channel survey (StartSurvey).
	surveyAt time.Time
	// A mesh-wide change this node sent (SetMeshProfile).
	meshChange     *status.MeshChange
	meshChangeSent time.Time
	meshChangeTo   *config.Profile // the profile every node switches to
	// siteKey is the demo node's site link key (SiteLinkNewKey changes it).
	siteKey string
	// oneRadio is a node with one dual-band USB adapter in a two-link
	// profile: it meshes on one link, the other has no radio here.
	oneRadio bool
	// portal: the uplink is behind a login page (the WAN's web check).
	portal bool
	// troubled is a node whose hardware has had a bad day: a PCIe link
	// that loses packets, a radio throttling, resets, a trial undone.
	troubled bool
	// upg is a made-up upgrade (upgrade.go).
	upg  *fakeUpgrade
	kept []byte // the package of the last dry run
	// A made-up mesh-wide upgrade (rollout.go).
	rollout     *fakeRollout
	rolloutPkgs map[string]api.RolloutPackage
}

func newFake(setup bool, password string, links int, bare bool) *fake {
	n := config.Node{Callsign: "relay-1", NodeID: 12, Gateway: config.GatewayConfig{DownMbit: 50, UpMbit: 10},
		Radios: config.RadiosConfig{Devices: []config.DeviceRule{{Match: config.DeviceMatch{Port: "usb:1-1.3"}, Label: "spare Alfa"}}}}
	if bare {
		// A node as installed: no per-radio rules, no ports listed. Its
		// configuration has empty lists where the demo node has entries,
		// which the UI must treat the same.
		n.Radios.Devices = nil
	}
	n.ApplyDefaults()
	p := config.Profile{Name: "alpha", MeshID: "trellis-alpha", SAEPassphrase: "k3m9w2-xp7fhq-b5tn8r-dj4cvy",
		Regulatory: config.Regulatory{Mode: config.RegModeCountry, Country: "US"}, Radio: config.RadioPlan{Band: config.Band5G, Channel: 149, Width: 80}}
	// Wi-Fi plus HaLow. With two links, 5 GHz and
	// HaLow; with three, 2.4 GHz as well, on the card's other radio.
	if links >= 2 {
		p.Links = append(p.Links, config.RadioPlan{Band: config.BandS1G, Channel: 26, Width: 2})
	}
	if links >= 3 {
		p.Links = append(p.Links, config.RadioPlan{Band: config.Band2G, Channel: 6, Width: 20})
	}
	p.Voice = config.VoicePlan{Groups: []config.VoiceGroup{{Number: 1, Name: "Command", Priority: true}, {Number: 2, Name: "Alpha"}, {Number: 3, Name: "Bravo"}, {Number: 4, Name: "Medevac"}}}
	if !bare {
		p.SiteLink = config.SiteLinkPlan{Members: []config.SiteMember{
			{Name: "relay-1", Site: "hq", PublicKey: fakeSiteKeys[0], Endpoint: "hq.example.org:51873"},
			{Name: "relay-2", Site: "hq", PublicKey: fakeSiteKeys[1], Endpoint: "198.51.100.23:51873"},
			{Name: "truck-3", Site: "field", PublicKey: fakeSiteKeys[2]},
			{Name: "ridge-9", Site: "ridge", PublicKey: fakeSiteKeys[3], Endpoint: "relay.example.net:51873"},
			{Name: "depot-8", Site: "depot", PublicKey: fakeSiteKeys[4], Endpoint: "depot.example.com:51873"},
		}}
		// The depot, reached only over the site link: its member and a
		// node behind it on its own air.
		nodes = append(nodes, depotNodes...)
	}
	if !bare {
		vol := 70
		n.Voice = config.VoiceNode{Enable: true, Channels: config.VoiceChannels{A: config.VoiceChannel{Group: 1, Ear: "left", Volume: &vol}, B: config.VoiceChannel{Group: 2, Ear: "right"}}, Listen: []int{4}}
	}
	p.ApplyDefaults()
	if setup {
		n.Callsign = "trellis-node"
		n.NodeID = 0
	}
	return &fake{started: time.Now().Add(-3*time.Hour - 17*time.Minute), node: n, profile: p, setup: setup, password: password, mcast: 48211, siteKey: fakeSiteKeys[0]}
}

// oneRadioNode makes the demo node one with a single dual-band adapter, on
// the 2.4 GHz link of a profile with a 5 GHz link too.
func (f *fake) oneRadioNode() {
	f.oneRadio = true
	f.profile.Radio = config.RadioPlan{Band: config.Band2G, Channel: 6, Width: 20}
	f.profile.Links = []config.RadioPlan{{Band: config.Band5G, Channel: 149, Width: 80}}
	f.node.Radios.Devices = nil
}

// fakeSiteKeys are made-up WireGuard public keys; the first is this node's.
var fakeSiteKeys = []string{
	"q2Lw8mJ4c1vN0xTfR7yH3kPzS5aG9dE6uB4iO2nM1lY=",
	"Zt5Wn3Xr8Kp1Qv7Lm2Hd9Fs4Gj6Bc0Ay3Ue5Io8Pk7M=",
	"Rf4Tg7Yh2Uj5Ik8Ol1Pz3Xc6Vb9Nm0Qa4Ws7Ed2Rt5Y=",
	"Mn8Bv5Cx2Zl9Kj6Hg3Fd0Sa7Pq4Wo1Ei8Ru5Ty2Ui9M=",
	"Hx3Jc6Nv9Bq2Wd5Ef8Gr1Ty4Uk7Il0Om3Pa6Sd9Fg2E=",
}

// depotNodes are the depot site's: depot-8 holds the site link there, with
// its tunnel to the demo node up; yard-9 is on the depot's air only. Not
// in a bare demo (no site link).
var depotNodes = []fnode{
	{"depot-8", 150, "6a:dc:ee:10:00:08", "a0:b0:c0:80:3e:41", true, true, 2, 150},
	{"yard-9", 151, "6a:dc:ee:10:00:09", "a0:b0:c0:90:61:c7", false, false, 3, 150},
}

// depotLink is the radio link between the depot's two nodes (indexes into
// nodes once the depot is added).
var depotLink = flink{7, 8, 36, 33, -60, -61}

// siteKeyOf is the site link key a demo node tells the mesh: truck-3's is
// the one the demo profile lists, the others' are made up from their
// names. scout-7, on an older build, tells none.
func siteKeyOf(callsign string) string {
	switch callsign {
	case "relay-1":
		return fakeSiteKeys[0]
	case "truck-3":
		return fakeSiteKeys[2]
	case "depot-8":
		return fakeSiteKeys[4]
	case "scout-7":
		return ""
	}
	sum := sha256.Sum256([]byte("site key of " + callsign))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// siteLink is the demo node's part in the site link: a member at hq, one
// peer up behind NAT, one not answering. A member added in the UI gets
// no answer, with meshd's words for why.
func (f *fake) siteLink(now time.Time) status.SiteLink {
	sl := status.SiteLink{PublicKey: f.siteKey, Members: len(f.profile.SiteLink.Members), Peers: []status.SitePeer{}}
	if _, ok := f.profile.SiteLink.Member(f.siteKey); !ok {
		return sl
	}
	me, _ := f.profile.SiteLink.Member(f.siteKey)
	sl.Member, sl.Site, sl.Address, sl.Port, sl.Endpoint = true, me.Site, config.SiteAddr(f.siteKey).String(), f.profile.SiteLink.ListenPort(), me.Endpoint
	here := "this node"
	if me.Endpoint != "" {
		here += " (" + me.Endpoint + ")"
	}
	for _, m := range f.profile.SiteLink.Peers(f.siteKey) {
		p := status.SitePeer{Name: m.Name, Site: m.Site, PublicKey: m.PublicKey, Endpoint: m.Endpoint, Interface: config.SiteIfname(m.PublicKey), Address: config.SiteAddr(m.PublicKey).String()}
		check := ". Check remote node power, internet and profile, and UDP to "
		switch {
		case m.PublicKey == fakeSiteKeys[2]:
			p.Callsign = "truck-3"
			p.Current, p.LastHandshake, p.Up = "203.0.113.77:41822", now.Add(-time.Duration(20+rand.IntN(90))*time.Second), true
			p.RxBytes, p.TxBytes, p.Batman, p.Neighbors = 48_211_904+uint64(now.Sub(f.started).Seconds())*900, 31_870_115, true, 1
		case m.PublicKey == fakeSiteKeys[4]:
			p.Callsign, p.Resolved = "depot-8", "203.0.113.150"
			p.Current, p.LastHandshake, p.Up = "203.0.113.150:51873", now.Add(-time.Duration(5+rand.IntN(100))*time.Second), true
			p.RxBytes, p.TxBytes, p.Batman, p.Neighbors = 9_310_552+uint64(now.Sub(f.started).Seconds())*300, 7_204_118, true, 1
		case m.PublicKey == fakeSiteKeys[3]:
			p.Resolved, p.Current, p.LastHandshake = "192.0.2.61", "192.0.2.61:51873", now.Add(-47*time.Minute)
			p.TxBytes = 412_330
			p.Note = "no handshake for 47 min" + check + m.Endpoint + " or " + here + "; forward the port on one router"
		case m.Endpoint == "" && me.Endpoint == "":
			p.Note = "no endpoint on either node, neither can connect. Set a public address and port for one of them in the profile"
		case m.Endpoint == "":
			p.Note = "no handshake yet" + check + here + fmt.Sprintf("; remote has no endpoint, forward UDP port %d to this node", sl.Port)
		default:
			p.Current, p.TxBytes = m.Endpoint, 14_800
			p.Note = "no handshake yet" + check + m.Endpoint + "; forward the port on the remote router"
		}
		// A node of the demo mesh listed at another site: next door.
		for _, n := range nodes[1:] {
			if k := siteKeyOf(n.callsign); k != "" && k == m.PublicKey && n.callsign != "truck-3" {
				p.Callsign = n.callsign
				p.Note = "it is a direct neighbour of this node (on mesh0): the two are at one place, and need no link over the internet. Members of one site take the same site name. Besides, " + p.Note
			}
		}
		sl.Peers = append(sl.Peers, p)
	}
	return sl
}

// siteRoster is every member of the demo profile as the demo node knows
// it: itself, the nodes of its mesh, and members it does not hear.
func (f *fake) siteRoster(sl status.SiteLink) []status.SiteMemberState {
	out := []status.SiteMemberState{}
	for _, m := range f.profile.SiteLink.Members {
		ms := status.SiteMemberState{Name: m.Name, Site: m.Site, PublicKey: m.PublicKey, Endpoint: m.Endpoint}
		if m.PublicKey == f.siteKey {
			ms.Self, ms.Heard, ms.Callsign, ms.Reported, ms.Peers = true, true, f.node.Callsign, true, len(sl.Peers)
			for _, p := range sl.Peers {
				if p.Up {
					ms.Up++
				}
			}
		}
		for _, n := range nodes[1:] {
			if k := siteKeyOf(n.callsign); k != "" && k == m.PublicKey {
				ms.Heard, ms.Callsign = true, n.callsign
				if s := f.nodeSite(n.callsign); s != nil {
					ms.Reported, ms.Up, ms.Peers = true, s.Up, s.Peers
				}
			}
		}
		out = append(out, ms)
	}
	return out
}

// nodeSite is what a demo node says of its own part in the site link:
// truck-3 has its tunnel to hq up, a node of this mesh listed in the UI
// has none.
func (f *fake) nodeSite(callsign string) *status.NodeSiteLink {
	m, ok := f.profile.SiteLink.Member(siteKeyOf(callsign))
	if !ok {
		return nil
	}
	s := &status.NodeSiteLink{Site: m.Site, Peers: len(f.profile.SiteLink.Peers(m.PublicKey))}
	if callsign == "truck-3" && s.Peers > 0 {
		s.Up = 1
	}
	return s
}

// drift moves a figure by up to pct percent, differently every call.
func drift(v, pct float64) float64 {
	return math.Round(v*(1+(rand.Float64()*2-1)*pct/100)*10) / 10
}

func (f *fake) Status(ctx context.Context) (*status.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.mcast += uint64(rand.IntN(9))
	// A mesh-wide change in force from its moment on.
	if f.meshChangeTo != nil && f.meshChange != nil && now.After(f.meshChange.ApplyAt) {
		f.profile, f.meshChangeTo = *f.meshChangeTo, nil
	}
	self := nodes[0]
	st := &status.Status{
		Version: "demo", Hostname: "relay-1", Time: now, StartedAt: f.started,
		Node: status.Node{Callsign: f.node.Callsign, NodeID: self.id, NodeIDSource: "config", MAC: self.mac, Address: "10.73.12.1/16", ConfigDigest: f.node.Digest()},
		Profile: status.Profile{Name: f.profile.Name, MeshID: f.profile.MeshID, Band: f.profile.Radio.Band, Channel: f.profile.Radio.Channel,
			FreqMHz: config.LinkFrequencyMHz(f.profile.Radio, f.profile.Regulatory.Country), WidthMHz: f.profile.Radio.Width, Prefix: f.profile.Prefix, PinnedBatman: "2026.3", MinBatman: "2024.0", OrigIntervalMS: 1000,
			PositionsShared: f.profile.Positions.Shared(), Revision: f.profile.Revision, Digest: f.profile.Digest()},
		// As on a node where trellis crda answers the kernel: the mode in
		// force follows the profile.
		Regulatory: status.Regulatory{RequestedMode: f.profile.Regulatory.Mode, Country: f.profile.Regulatory.Country, ActiveMode: f.profile.Regulatory.Mode,
			KernelAlpha2: f.profile.Regulatory.Country, Source: "trellis"},
		Setup:    status.Setup{Needed: f.setup, NodeConfig: !f.setup, Profile: !f.setup},
		Warnings: []status.Warning{},
	}
	if st.Regulatory.RequestedMode == config.RegModeUnrestricted {
		st.Regulatory.Note = "Trellis's rules, without regulatory limits: each radio's driver and firmware set its power"
	}
	st.Radios = []status.Radio{
		{Fingerprint: "pci:0000:01:00.0:5g", FingerprintSource: "port", Phy: "phy1", Chipset: "MediaTek MT7915", Driver: "mt7915e", Bus: "pci",
			Port: "pci:0000:01:00.0", DeviceID: "14c3:7915", PermAddr: "a0:b0:c0:00:89:fb", Bands: []string{"5g", "6g"}, Streams: 2, MeshPoint: true, AP: true, MeshAndAP: true,
			Power: map[string]status.PowerFact{"5g": {DBm: 27, Source: "measured"}, "6g": {DBm: 23, Source: "database"}}, Role: "mesh", Recommended: "mesh",
			Interfaces: []string{"mesh0 (mesh point)"}, Quirks: []string{"Dual-band concurrent card: appears as two radios, one per band"}, LED: true},
		{Fingerprint: "pci:0000:01:00.0:2g", FingerprintSource: "port", Phy: "phy0", Chipset: "MediaTek MT7915", Driver: "mt7915e", Bus: "pci",
			Port: "pci:0000:01:00.0", DeviceID: "14c3:7915", PermAddr: "a0:b0:c0:00:89:fa", Bands: []string{"2g"}, Streams: 2, MeshPoint: true, AP: true, MeshAndAP: true,
			Power: map[string]status.PowerFact{"2g": {DBm: 25, Source: "measured"}}, Role: "ap", Recommended: "ap", Interfaces: []string{"ap0 (AP)"}, LED: true},
		{Fingerprint: "mac:00:c0:ca:98:12:4c", FingerprintSource: "permanent-mac", Phy: "phy2", Label: "spare Alfa", Chipset: "MediaTek MT7612U", Driver: "mt76x2u", Bus: "usb",
			Port: "usb:1-1.3", DeviceID: "0e8d:7612", USBSpeedMbps: 5000, PermAddr: "00:c0:ca:98:12:4c", Bands: []string{"2g", "5g"}, Streams: 2, MeshPoint: true, AP: true, MeshAndAP: true,
			Power: map[string]status.PowerFact{"2g": {DBm: 20, Source: "driver"}, "5g": {DBm: 20, Source: "driver"}}, Role: "unused", Recommended: "unused",
			Interfaces: []string{"wlx00c0ca98124c (station)"}},
	}
	st.Plan = status.Plan{MeshRadio: st.Radios[0].Fingerprint, APRadio: st.Radios[1].Fingerprint, APBand: "2g", Notices: []status.Warning{
		{Level: "info", Code: "extra-radio", Message: "1 radio(s) unused.", Source: "radios"}}}

	st.MeshLink = status.MeshLink{Index: 1, Interface: "mesh0", Transport: "wifi", Supplicant: status.Supplicant{Running: true, PID: 812, State: "COMPLETED", Joined: true},
		Band: f.profile.Radio.Band, Channel: f.profile.Radio.Channel, WidthMHz: f.profile.Radio.Width,
		FreqMHz: config.LinkFrequencyMHz(f.profile.Radio, f.profile.Regulatory.Country), MTU: 1560, TXPowerDBm: 27, Peers: []status.Peer{}}
	country := f.profile.Regulatory.Country
	for i, l := range f.profile.Links {
		ml := status.MeshLink{Index: i + 2, Interface: fmt.Sprintf("mesh%d", i+1), Transport: "wifi",
			Supplicant: status.Supplicant{Running: true, PID: 840 + i, State: "COMPLETED", Joined: true},
			Band:       l.Band, Channel: l.Channel, WidthMHz: l.Width, FreqMHz: config.LinkFrequencyMHz(l, country), MTU: 1560, Peers: []status.Peer{}}
		role := fmt.Sprintf("mesh%d", i+2)
		switch l.Band {
		case config.BandS1G:
			ml.TXPowerDBm = 21
			st.Radios = append(st.Radios, status.Radio{Fingerprint: "mac:a0:b0:c0:00:89:3d", FingerprintSource: "permanent-mac", Phy: "phy3",
				Chipset: "Morse Micro MM8108 (Wi-Fi HaLow)", Driver: "morse_usb", Bus: "usb", Port: "usb:1-1.2", PermAddr: "a0:b0:c0:00:89:3d",
				Bands: []string{"s1g"}, Streams: 1, MeshPoint: true, Role: role, Recommended: role, Interfaces: []string{ml.Interface + " (mesh point)"},
				Quirks: []string{"Needs Morse Micro's driver, firmware and the module's board file"}})
		case config.Band2G:
			// The card's 2.4 GHz radio carries this link, and the client
			// Wi-Fi with it: no radio is left over.
			ml.TXPowerDBm = 25
			st.Radios[1].Role, st.Radios[1].Recommended = role+"+ap", role+"+ap"
			st.Radios[1].Interfaces = []string{ml.Interface + " (mesh point)", "ap0 (AP)"}
			st.Plan.Notices = append(st.Plan.Notices, status.Warning{Level: "warning", Code: "ap-shares-mesh-radio",
				Message: fmt.Sprintf("No free radio for client AP: sharing the radio and channel of the %s link.", l.Name()), Source: "radios"})
		}
		st.MoreLinks = append(st.MoreLinks, ml)
	}
	if f.oneRadio {
		// One MT7610U on the first link; the 5 GHz link has no radio here.
		st.Radios = []status.Radio{{Fingerprint: "mac:a0:b0:c0:00:01:f1", FingerprintSource: "permanent-mac", Phy: "phy0", Chipset: "MediaTek MT7610U", Driver: "mt76x0u",
			Bus: "usb", Port: "usb:1-1", DeviceID: "0e8d:7610", PermAddr: "a0:b0:c0:00:01:f1", Bands: []string{"2g", "5g"}, Streams: 1, MeshPoint: true, AP: true, MeshAndAP: true,
			Power: map[string]status.PowerFact{"2g": {DBm: 14, Source: "driver"}, "5g": {DBm: 16, Source: "driver"}}, Role: "mesh", Recommended: "mesh",
			Interfaces: []string{"mesh0 (mesh point)"}}}
		for i := range st.MoreLinks {
			st.MoreLinks[i].Interface, st.MoreLinks[i].Transport, st.MoreLinks[i].FreqMHz = "", "none", 0
		}
		st.Plan.MeshRadio, st.Plan.APRadio = st.Radios[0].Fingerprint, st.Radios[0].Fingerprint
		st.Radios[0].Role, st.Radios[0].Recommended = "mesh+ap", "mesh+ap"
		st.Radios[0].Interfaces = append(st.Radios[0].Interfaces, "ap0 (AP)")
		st.Plan.Notices = []status.Warning{{Level: "info", Code: "mesh2-no-radio", Source: "radios",
			Message: "No radio for the 5 GHz · ch 149 link: meshing on the 2.4 GHz · ch 6 link only."}}
	}
	st.Batman = status.Batman{Interface: "bat0", Present: true, ModuleVersion: "2026.3", ModulePath: "/lib/modules/6.18.60+rpt-rpi-v8/updates/dkms/batman-adv.ko",
		Algo: "BATMAN_V", MAC: self.mac, MTU: 1500, MTUOK: true, HardIfs: []status.HardIf{{Name: "mesh0", MAC: self.orig, Active: true, MTU: 1560}},
		Neighbors: []status.Neighbor{}, Originators: []status.Originator{}, Gateways: []status.Gateway{},
		Settings: map[string]any{"aggregated_ogms": true, "bonding": false, "bridge_loop_avoidance": true, "distributed_arp_table": false, "fragmentation": true,
			"gw_mode": "server", "gw_sel_class": 20, "hop_penalty": 30, "multicast_fanout": 16, "multicast_forceflood": false, "orig_interval_ms": 1000},
		Multicast: status.Multicast{Optimisations: true, OwnFlags: 57, Bridged: true, QuerierIPv4: true, QuerierIPv6: true, PeersWithFlags: 6, Path: "efficient",
			Counters: status.McastCounters{TX: f.mcast, TXBytes: f.mcast * 311, TXLocal: f.mcast / 3, RX: f.mcast * 4, RXLocal: f.mcast * 4, Fwd: f.mcast / 2, FwdBytes: f.mcast * 150}},
	}
	direct := map[int]float64{}
	for _, l := range links {
		if l.a != 0 {
			continue
		}
		peer := nodes[l.b]
		tput := drift(l.ab, 6)
		direct[l.b] = tput
		st.MeshLink.Peers = append(st.MeshLink.Peers, status.Peer{MAC: peer.orig, SignalDBm: l.sigA + rand.IntN(3) - 1, TXMbps: drift(l.ab*3.1, 4), RXMbps: drift(l.ba*3.1, 4),
			InactiveMS: uint32(rand.IntN(40)), ConnectedSeconds: uint32(now.Sub(f.started).Seconds()) - 40, PlinkState: "ESTAB", Established: true})
		st.Batman.Neighbors = append(st.Batman.Neighbors, status.Neighbor{MAC: peer.orig, Via: "mesh0", LastSeenMS: uint32(rand.IntN(900)), ThroughputMbps: tput})
		st.Batman.Originators = append(st.Batman.Originators, status.Originator{MAC: peer.orig, NextHop: peer.orig, Via: "mesh0", LastSeenMS: uint32(rand.IntN(900)), ThroughputMbps: tput, Best: true})
	}
	// Nodes beyond one hop: ops-6 through truck-3, scout-7 through ridge-2.
	for _, far := range []struct {
		node, via int
		tput      float64
	}{{5, 2, 24}, {6, 1, 17}} {
		st.Batman.Originators = append(st.Batman.Originators, status.Originator{MAC: nodes[far.node].orig, NextHop: nodes[far.via].orig, Via: "mesh0",
			LastSeenMS: uint32(rand.IntN(900)), ThroughputMbps: drift(far.tput, 6), Best: true})
	}
	st.Batman.Originators = append(st.Batman.Originators, status.Originator{MAC: nodes[4].orig, NextHop: nodes[3].orig, Via: "mesh0", LastSeenMS: 420, ThroughputMbps: drift(31, 6)})
	// The depot, over the site link: the tunnel at the profile's figure,
	// the paths through it at what the tunnels' hop penalty leaves of it.
	if len(nodes) > 7 {
		tun, depot, yard := config.SiteIfname(fakeSiteKeys[4]), nodes[7].orig, nodes[8].orig
		mbit := float64(f.profile.SiteLink.ThroughputMbit())
		st.Batman.Neighbors = append(st.Batman.Neighbors, status.Neighbor{MAC: depot, Via: tun, LastSeenMS: uint32(rand.IntN(900)), ThroughputMbps: mbit})
		st.Batman.Originators = append(st.Batman.Originators,
			status.Originator{MAC: depot, NextHop: depot, Via: tun, LastSeenMS: uint32(rand.IntN(900)), ThroughputMbps: 3.1, Best: true},
			status.Originator{MAC: yard, NextHop: depot, Via: tun, LastSeenMS: uint32(rand.IntN(900)), ThroughputMbps: 3.1, Best: true})
	}
	// Neighbours on the last link too: batman-adv lists a node once per
	// link it is heard on.
	if k := len(st.MoreLinks); k > 0 && !f.oneRadio {
		ml := &st.MoreLinks[k-1]
		for _, l := range lastLinks {
			if l.a != 0 {
				continue
			}
			peer := linkMAC(nodes[l.b].orig)
			ml.Peers = append(ml.Peers, status.Peer{MAC: peer, SignalDBm: l.sigA + rand.IntN(3) - 1, TXMbps: drift(l.ab*3.1, 4), RXMbps: drift(l.ba*3.1, 4),
				InactiveMS: uint32(rand.IntN(40)), ConnectedSeconds: uint32(now.Sub(f.started).Seconds()) - 60, PlinkState: "ESTAB", Established: true})
			st.Batman.Neighbors = append(st.Batman.Neighbors, status.Neighbor{MAC: peer, Via: ml.Interface, LastSeenMS: uint32(rand.IntN(900)), ThroughputMbps: drift(l.ab, 6)})
		}
	}

	st.Bridge = status.Bridge{Name: "br-mesh", Present: true, Ports: []string{"ap0", "bat0", "eth1"}, Address: "10.73.12.1/16", MTU: 1500}
	apJoin := f.apJoin()
	// A node with GNSS: its clock follows the receiver and it serves the
	// mesh. The position is made up.
	lat, lon, alt := 52.0907+rand.Float64()*0.0002, 5.1214+rand.Float64()*0.0002, 4.0
	st.GNSS = status.GNSS{Receiver: true, Device: "/dev/ttyAMA0", Driver: "u-blox", Fix: "3d", Lat: &lat, Lon: &lon, AltM: &alt,
		HAccM: 3.2, VAccM: 5.1, SatsUsed: 9, SatsSeen: 14, FixAgeMS: int64(rand.IntN(900))}
	// On the ATAK map, and keeping one node that left at its last known
	// position.
	st.Positions = status.Positions{Shared: true, Sent: 42, NMEADevices: 2, NMEASent: 7200, LastSentAgoMS: int64(rand.IntN(60000)), Announcing: true,
		Note: "on the ATAK map, re-sent every 1m0s while still and at once on a move",
		LastKnown: []status.LastKnown{{Callsign: "scout-7", MAC: "a0:b0:c0:00:00:07", Heard: time.Now().Add(-17 * time.Minute),
			Position: status.Position{Lat: 52.0961, Lon: 5.1302, AccM: 6, Fix: "3d", Time: time.Now().Add(-17 * time.Minute)}}}}
	if !f.profile.Positions.Shared() {
		st.Positions = status.Positions{Off: "profile", LastSentAgoMS: -1, Note: "turned off for the whole mesh (positions.share in the profile)"}
	} else if !f.node.Positions.Shared() {
		st.Positions.Shared, st.Positions.Off, st.Positions.Note = false, "node", "turned off for this node (positions.share in node.yaml)"
	}
	st.Clock = status.Clock{Service: "chrony", Synced: true, Source: "GNSS receiver", SourceKind: "gnss", Stratum: 1,
		OffsetMS: math.Round((rand.Float64()-0.5)*40) / 10, Serving: true}
	// Channel load per link, here and at the other nodes.
	st.MeshLink.ChannelBusyPct, st.MeshLink.NoiseDBm = 4+rand.IntN(3), -92
	for i := range st.MoreLinks {
		st.MoreLinks[i].ChannelBusyPct = -1
		if st.MoreLinks[i].Band == "2g" {
			st.MoreLinks[i].ChannelBusyPct = 30 + rand.IntN(6)
		}
	}
	st.Rollout = f.rolloutStatus(now)
	if mc := f.meshChange; mc != nil {
		// The other nodes answer one by one; every node switches at the
		// moment.
		c := *mc
		since := now.Sub(f.meshChangeSent)
		for i, n := range []string{"alpha-4", "bravo-5", "ops-6", "ridge-2", "truck-3"} {
			state := "sent"
			if since > time.Duration(i+1)*1500*time.Millisecond {
				state = "has it"
			}
			c.Nodes = append(c.Nodes, status.MeshChangeNode{Callsign: n, State: state})
		}
		if now.After(c.ApplyAt) {
			c.State = "applied"
		}
		st.MeshChange = &c
	}
	apRadio := st.Radios[len(st.Radios)-1].Fingerprint
	if len(st.Radios) > 1 {
		apRadio = st.Radios[1].Fingerprint
	}
	st.AP = status.AP{Enabled: f.node.APEnabled(), Interface: "ap0", Radio: apRadio, SSID: apJoin.SSID, Band: "2g", Channel: 1, FreqMHz: 2412,
		Hostapd: status.Helper{Running: true, PID: 840, State: "ENABLED"}, TXPowerDBm: 25, Clients: []status.Client{
			{MAC: "02:09:3b:ba:06:e1", SignalDBm: -38 + rand.IntN(5), TXMbps: 144.4, RXMbps: drift(86, 20), InactiveMS: uint32(rand.IntN(3000)), ConnectedSeconds: 5120, IP: "10.73.12.105", Hostname: "Galaxy-S24"},
			{MAC: "7e:21:5c:0d:88:3a", SignalDBm: -61 + rand.IntN(5), TXMbps: 72.2, RXMbps: drift(52, 20), InactiveMS: uint32(rand.IntN(9000)), ConnectedSeconds: 940, IP: "10.73.12.118", Hostname: "Pixel-8a"},
		}}
	if !st.AP.Enabled {
		st.AP = status.AP{Clients: []status.Client{}}
	}
	st.ClientNet = status.ClientNet{
		Anycast: status.Anycast{Interface: "anycast0", Present: true, Address: "10.73.0.1", MAC: "02:74:72:65:6c:01"},
		Filters: status.Filters{Applied: true, Rules: 29, Drops: map[string]uint64{"anycast-arp": 212, "anycast-mac": 0, "chatter": 1840 + f.mcast/40, "dhcp-broadcast": 31,
			"dhcpv6": 12, "gossip-to-from-clients": 0, "ipv4-broadcast": 96, "ipv6-ra": 4, "link-local-multicast": 0, "rogue-dhcp-server": 0, "rogue-ipv6-ra": 0, "rogue-multicast-querier": 3}},
		DHCP: status.DHCP{Dnsmasq: status.Helper{Running: true, PID: 841, State: "running"}, PoolStart: "10.73.12.100", PoolEnd: "10.73.12.199", Domain: "mesh"},
		Leases: []status.Lease{
			{MAC: "02:09:3b:ba:06:e1", IP: "10.73.12.105", Hostname: "Galaxy-S24", Expires: now.Add(21 * time.Hour)},
			{MAC: "7e:21:5c:0d:88:3a", IP: "10.73.12.118", Hostname: "Pixel-8a", Expires: now.Add(23 * time.Hour)},
			{MAC: "4c:e1:73:42:25:28", IP: "10.73.12.147", Hostname: "toughbook", Expires: now.Add(19 * time.Hour)},
		}}
	gwMode := f.node.Gateway.Mode
	st.Gateway = status.GatewayState{Mode: gwMode, Role: "gateway", WAN: "eth0", WANHealthy: true, WANKnown: true, WANSince: f.started.Add(4 * time.Second),
		RTTms: drift(18.4, 25), LossPct: 0, ProbeTargets: f.node.Gateway.ProbeTargets, Announced: fmt.Sprintf("%d/%d Mbit/s", f.node.Gateway.DownMbit, f.node.Gateway.UpMbit),
		Selected: &status.SelectedGateway{Self: true, Callsign: f.node.Callsign, NodeID: self.id, Address: "10.73.12.1", Since: f.started.Add(5 * time.Second)},
		DNS:      []string{"192.168.8.1"}, Switches: 2,
		Candidates: []status.GatewayCandidate{
			{Originator: nodes[2].orig, Callsign: "truck-3", NodeID: 41, Address: "10.73.41.1", ThroughputMbps: direct[2], LastSeenMS: 310, Usable: true},
			{Originator: nodes[5].orig, Callsign: "ops-6", NodeID: 96, Address: "10.73.96.1", ThroughputMbps: 24, LastSeenMS: 640, Usable: false, Why: "its uplink is down (gossip)"},
		}}
	if f.node.Gateway.WebChecking() {
		st.Gateway.WebCheck, st.Gateway.WebCheckAt = "ok", now.Add(-20*time.Second)
	}
	if f.portal {
		st.Gateway.Role, st.Gateway.Announced = "client (uplink behind a login page)", ""
		st.Gateway.WebCheck, st.Gateway.WebCheckDetail = "portal", "connectivitycheck.gstatic.com: 302 to http://wifi.example-hotel.com/"
		st.Gateway.Candidates[0].Usable, st.Gateway.Candidates[0].Why = false, "its uplink is down (gossip)"
		st.Warnings = append(st.Warnings, status.Warning{Level: "warning", Source: "gateway", Code: "wan-portal",
			Message: "Uplink eth0 behind captive portal (connectivitycheck.gstatic.com: 302 to http://wifi.example-hotel.com/): not shared until sign-in. Local clients reach the portal while no other gateway is available. Override: gateway.web_check: off."})
	}
	if gwMode == config.GatewayOff {
		st.Gateway.Role, st.Gateway.Announced = "client", ""
		st.Gateway.Selected = &status.SelectedGateway{Callsign: "truck-3", NodeID: 41, Address: "10.73.41.1", ThroughputMbps: direct[2], Since: now.Add(-2 * time.Minute)}
		st.Gateway.DNS = []string{"10.73.41.1"}
	}
	// The firewall as the made-up node's configuration says.
	st.Gateway.Firewall = config.FirewallOpen
	if fw := f.node.Firewall; fw.Closed() {
		st.Gateway.Firewall = config.FirewallClosed
		if fw.SSHOpen() {
			st.Gateway.FirewallOpen = append(st.Gateway.FirewallOpen, "tcp/22")
		}
		if f.node.Web.WANAccess {
			st.Gateway.FirewallOpen = append(st.Gateway.FirewallOpen, "tcp/80", "tcp/443")
		}
		st.Gateway.FirewallOpen = append(st.Gateway.FirewallOpen, fw.Allow...)
		st.Gateway.FirewallDrop = 1342
	}

	st.Batman.Gateways = []status.Gateway{
		{MAC: nodes[2].orig, Router: nodes[2].orig, DownMbps: 20, UpMbps: 5, ThroughputMbps: direct[2]},
		{MAC: nodes[5].orig, Router: nodes[2].orig, DownMbps: 100, UpMbps: 20, ThroughputMbps: 24},
	}
	st.Ports = []status.Port{
		{Name: "eth0", Role: "in-use", Why: "it has the address 192.168.8.114/24", Carrier: true, Since: f.started},
		{Name: "eth1", Role: "client", Why: "taken over from NetworkManager: a device without a DHCP server is plugged in", Carrier: true, Since: now.Add(-41 * time.Minute)},
	}
	for _, n := range nodes[1:] {
		st.Nodes = append(st.Nodes, status.MeshNode{Callsign: n.callsign, NodeID: n.id, Address: fmt.Sprintf("10.73.%d.1", n.id), MAC: n.mac, Originators: []string{n.orig, linkMAC(n.orig)},
			Version: "demo", Gateway: n.gw, WANHealthy: n.wanOK, HeardAgoMS: int64(rand.IntN(30000)), MeshRadios: n.radios(), SiteKey: siteKeyOf(n.callsign)})
	}
	st.Nodes[5].Version = "4a48da6+1104.072526"
	f.hardware(st, now)
	// Each node's channel load per link, as gossip carries it; scout-7, on
	// an older build, sends none, and HaLow reports none.
	for i := range st.Nodes[:5] {
		load := []int{3 + i%4}
		for _, l := range f.profile.Links {
			v := -1
			if l.Band == "2g" {
				v = 12 + 5*i
			}
			load = append(load, v)
		}
		st.Nodes[i].ChannelBusy = load
	}
	st.Gossip = status.Gossip{Sent: 391 + f.mcast/90, Received: 2310 + f.mcast/15, BytesSent: 101660, BytesReceived: 598290, LastRecordBytes: 260}
	passes := uint64(now.Sub(f.started) / (15 * time.Second))
	st.Reconcile = status.Reconcile{Passes: passes, LastPass: now.Add(-time.Duration(rand.IntN(14000)) * time.Millisecond), LastDuration: "118ms", Converged: true}
	for _, name := range []string{"host", "radios", "meshlink", "meshlink2", "batman", "ports", "bridge", "filters", "anycast", "gateway", "gossip", "ap", "dhcp"} {
		st.Reconcile.Reconcilers = append(st.Reconcile.Reconcilers, status.ReconcilerInfo{Name: name, LastChanged: f.started.Add(6 * time.Second), Changes: 1})
	}
	st.SiteLink = f.siteLink(now)
	st.SiteLink.Roster = f.siteRoster(st.SiteLink)
	// As meshd: a tunnel that is not up is an alert, in the peer's words.
	for _, p := range st.SiteLink.Peers {
		if !p.Up && p.Note != "" && (now.Sub(f.started) > 3*time.Minute) {
			name := p.Name
			if name == "" {
				name = p.Callsign
			}
			st.Warnings = append(st.Warnings, status.Warning{Level: "warning", Source: "sitelink", Code: "site-link-down",
				Message: fmt.Sprintf("Site link to %s (site %s) down: %s.", name, p.Site, p.Note)})
		}
	}
	for i := range st.Nodes {
		st.Nodes[i].SiteLink = f.nodeSite(st.Nodes[i].Callsign)
	}
	st.Process = status.Process{PID: 790, OpenFiles: 31, Goroutines: 34}
	st.Warnings = append(st.Warnings,
		status.Warning{Level: "warning", Code: "weak-link", Message: "Weak link to bravo-5 (5 GHz · ch 149): -79 dBm. Expect low throughput.", Source: "meshlink"},
		status.Warning{Level: "info", Code: "version-mismatch", Message: "scout-7 runs a different build (4a48da6+1104.072526).", Source: "gossip"})
	if f.setup {
		st.Warnings = append(st.Warnings, status.Warning{Level: "info", Code: "setup-needed", Message: "No mesh profile: node standalone. Provision via web UI or meshctl profile install."})
		st.Nodes, st.MeshLink.Peers, st.Batman.Neighbors, st.Batman.Originators = nil, []status.Peer{}, []status.Neighbor{}, []status.Originator{}
		st.Gateway.Candidates, st.Batman.Gateways = []status.GatewayCandidate{}, []status.Gateway{}
	}
	return st, nil
}

func (f *fake) apJoin() *status.APJoin {
	ssid, pass := f.node.Callsign, "h7kq2mxw9dfp"
	if f.node.AP.SSID != "" {
		ssid = f.node.AP.SSID
	}
	if f.node.AP.Passphrase != "" {
		pass = f.node.AP.Passphrase
	}
	if f.setup {
		ssid, pass = "trellis-setup-0001", "trellis-setup"
	}
	j := &status.APJoin{Enabled: f.node.APEnabled(), SSID: ssid, Passphrase: pass, Band: "2g", Channel: 1}
	j.WiFiQR = hostapd.WiFiQR(ssid, pass)
	if c, err := qr.Encode(j.WiFiQR, qr.M); err == nil {
		j.QR = &status.QR{Size: c.Size, Rows: c.Rows()}
	}
	if !j.Enabled {
		j.Note = "the client AP is disabled in node.yaml"
	}
	return j
}

func (f *fake) Config() api.ConfigResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := api.ConfigResult{Node: f.node.Redacted(), Profile: f.profile.Redacted(), Setup: status.Setup{Needed: f.setup, NodeConfig: !f.setup, Profile: !f.setup},
		NodeDigest: f.node.Digest(), ProfileDigest: f.profile.Digest()}
	if f.meshChange != nil && f.meshChange.State == "pending" && f.meshChangeTo != nil {
		r := f.meshChangeTo.Redacted()
		out.PendingProfile = &r
	}
	return out
}

// stale is meshd's refusal of a change made from an old copy.
func stale(base, now, what string) error {
	if base == "" || base == now {
		return nil
	}
	return api.Conflict("%s changed on the node since this change was started (from another browser, another node or meshctl): nothing was changed. Read it again, and make the change again", what)
}

func (f *fake) Kick(string)   {}
func (f *fake) Reload() error { return nil }

func (f *fake) APJoin() (*status.APJoin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apJoin(), nil
}

func eff(level, code, msg string) status.Warning {
	return status.Warning{Level: level, Code: code, Message: msg}
}

func (f *fake) SetNode(ctx context.Context, raw []byte, base string, dryRun bool) (*api.ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := stale(base, f.node.Digest(), "This node's configuration"); err != nil {
		return nil, err
	}
	n, err := config.ParseNodeJSON(raw)
	if err != nil {
		return nil, err
	}
	if n.AP.Passphrase == config.Redacted {
		n.AP.Passphrase = f.node.AP.Passphrase
	}
	was, _ := json.Marshal(f.node)
	now, _ := json.Marshal(n)
	res := &api.ApplyResult{Changed: string(was) != string(now), Effects: []status.Warning{}}
	cur := f.node
	if cur.Radios.Roles.Mesh != n.Radios.Roles.Mesh {
		if n.Radios.Roles.Mesh.Mode == config.RoleDisabled {
			res.Effects = append(res.Effects, eff("error", "mesh-link-down", "The mesh link goes down: no radio carries it any more. If it is this node's only link, the node leaves the mesh, and you lose this page if you reach it through another node."))
		} else {
			res.Effects = append(res.Effects, eff("warning", "mesh-link-moves", "The mesh link moves to another radio and drops for a few seconds."))
		}
	}
	if cur.APEnabled() != n.APEnabled() || cur.AP.SSID != n.AP.SSID || cur.AP.Passphrase != n.AP.Passphrase || cur.Callsign != n.Callsign {
		res.Effects = append(res.Effects, eff("warning", "ap-credentials", "The client Wi-Fi's name or passphrase changes. Devices must join again with the new ones, and so must you if you are on it."))
	}
	if !cur.Firewall.Closed() && n.Firewall.Closed() {
		res.Effects = append(res.Effects, eff("warning", "firewall-closes", "Firewall on: from every interface but lo, br-mesh and anycast0 (the uplink, VPNs, containers) the node accepts tcp/22, plus replies, ICMP and DHCP, and drops new connections into the mesh except your DNAT port forwards. Services reached that way before stop answering; the mesh, this node's Wi-Fi and device ports, and the internet it shares are not affected."))
	}

	if cur.Web.WANAccess != n.Web.WANAccess {
		res.Effects = append(res.Effects, eff("warning", "web-wan-access", "This page also becomes reachable from the node's uplink network, not only from the mesh, where the admin password protects it."))
	}
	if cur.Gateway.Mode != n.Gateway.Mode && n.Gateway.Mode == config.GatewayOff {
		res.Effects = append(res.Effects, eff("warning", "gateway-off", "This node stops sharing its internet uplink with the mesh. Its own devices use another node's gateway, if there is one."))
	}
	if cur.Callsign != n.Callsign {
		res.Effects = append(res.Effects, eff("info", "renamed", fmt.Sprintf("The node is renamed from %s to %s; on the mesh it becomes %s.mesh.", cur.Callsign, n.Callsign, n.Callsign)))
	}
	if !dryRun && res.Changed {
		f.node, res.Applied = *n, true
	}
	if cur.Callsign != n.Callsign || cur.AP != n.AP {
		keep := f.node
		f.node = *n
		res.AP = f.apJoin()
		if dryRun || !res.Changed {
			f.node = keep
		}
	}
	return res, nil
}

func (f *fake) profileFrom(in api.ProfileInput) (*config.Profile, error) {
	switch {
	case in.Code != "":
		p, err := config.DecodeProfileCode(in.Code)
		if err != nil {
			return nil, api.Invalid("%v", err)
		}
		return p, nil
	case in.YAML != "":
		return config.ParseProfile([]byte(in.YAML))
	case in.Profile != nil:
		p := *in.Profile
		if p.SAEPassphrase == config.Redacted {
			p.SAEPassphrase = f.profile.SAEPassphrase
		}
		p.ApplyDefaults()
		return &p, p.Validate()
	case in.New != nil:
		if strings.TrimSpace(in.New.Name) == "" {
			return nil, config.Errors{{Field: "name", Msg: "required"}}
		}
		p := config.Profile{Name: in.New.Name, MeshID: "trellis-" + in.New.Name, SAEPassphrase: "n4vq8z-t2hk6m-w9cxp3-f7bdgj",
			Regulatory: config.Regulatory{Mode: in.New.Mode, Country: in.New.Country}, Radio: config.RadioPlan{Band: in.New.Band, Channel: in.New.Channel, Width: in.New.Width},
			Links: append([]config.RadioPlan(nil), in.New.Links...)}
		if p.Regulatory.Mode == "" {
			p.Regulatory.Mode = config.RegModeCountry
		}
		p.ApplyDefaults()
		def := map[string]int{"2g": 6, "5g": 149, "6g": 37, "s1g": 26}
		if p.Radio.Channel == 0 {
			p.Radio.Channel = def[p.Radio.Band]
		}
		for i := range p.Links {
			if p.Links[i].Channel == 0 {
				p.Links[i].Channel = def[p.Links[i].Band]
			}
		}
		return &p, p.Validate()
	}
	return nil, api.Invalid("give the profile in exactly one form: code, yaml, profile or new")
}

// StartSurvey begins a made-up mesh-wide survey: the nodes answer one by
// one over a few seconds.
func (f *fake) StartSurvey(ctx context.Context) (*status.Survey, error) {
	f.mu.Lock()
	if f.surveyAt.IsZero() || time.Since(f.surveyAt) > 20*time.Second {
		f.surveyAt = time.Now()
	}
	f.mu.Unlock()
	return f.Survey(), nil
}

// Survey is the made-up survey: 2.4 GHz channel 6 busy near truck-3 with
// networks around it, channel 1 clear; 5 GHz channel 149's block clear.
func (f *fake) Survey() *status.Survey {
	f.mu.Lock()
	at, p, self := f.surveyAt, f.profile, f.node.Callsign
	f.mu.Unlock()
	if at.IsZero() {
		return nil
	}
	since := time.Since(at)
	sv := &status.Survey{Started: at, Done: since > 6*time.Second, Nodes: []status.SurveyNode{}, Suggestions: []status.ChannelSuggestion{}}
	busy24 := map[string]map[int]int{"truck-3": {5: 30, 6: 34, 7: 31, 11: 9}, "ridge-2": {6: 18}}
	names := []string{self, "ridge-2", "truck-3", "alpha-4", "bravo-5", "ops-6", "scout-7"}
	for i, name := range names {
		n := status.SurveyNode{Callsign: name, State: "asked"}
		switch {
		case name == "scout-7" && sv.Done:
			n.State = "no answer"
		case name != "scout-7" && since > time.Duration(i)*800*time.Millisecond:
			n.State = "answered"
			for k, l := range p.AllLinks() {
				sl := status.SurveyLink{Link: k + 1, Band: l.Band, Channels: []status.SurveyChannel{}}
				switch l.Band {
				case "2g":
					for c := 1; c <= 11; c++ {
						b := 4 + (c+i)%5
						if v, ok := busy24[name][c]; ok {
							b = v
						}
						nets := map[int]int{6: 2, 7: 1}[c]
						if name == "alpha-4" || name == "bravo-5" {
							b = -1 // drivers that measure no busy time
						}
						sl.Channels = append(sl.Channels, status.SurveyChannel{Ch: c, Busy: b, Nets: nets, Strongest: map[bool]int{true: -58}[nets > 0]})
					}
				case "5g":
					for _, c := range []int{36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112, 149, 153, 157, 161, 165} {
						b := 2 + (c+i)%4
						if c >= 36 && c <= 48 {
							b += 9
						}
						sl.Channels = append(sl.Channels, status.SurveyChannel{Ch: c, Busy: b, Noise: -92, Nets: map[int]int{44: 1}[c]})
					}
				default:
					sl.Error = "HaLow radios report no channel survey"
				}
				n.Links = append(n.Links, sl)
			}
		}
		sv.Nodes = append(sv.Nodes, n)
	}
	for k, l := range p.AllLinks() {
		sug := status.ChannelSuggestion{Link: k + 1, Name: l.Name(), Width: l.Width}
		switch l.Band {
		case "2g":
			cur := status.ChannelScore{Ch: l.Channel, Score: 44, WorstBusy: 34, WorstNode: "truck-3", Nets: 2, Strongest: -58}
			best := status.ChannelScore{Ch: 1, Score: 8, WorstBusy: 8, WorstNode: "ops-6"}
			sug.Current, sug.Best = &cur, &best
			sug.Ranked = []status.ChannelScore{best, {Ch: 11, Score: 9, WorstBusy: 9, WorstNode: "truck-3"}, {Ch: 2, Score: 13, WorstBusy: 8, Nets: 1}, cur}
			sug.Note = fmt.Sprintf("channel 1 is clearer: at worst 8%% busy, no other network, against 34%% busy, 2 other networks on channel %d", l.Channel)
		case "5g":
			cur := status.ChannelScore{Ch: l.Channel, Score: 5, WorstBusy: 5, WorstNode: "ridge-2"}
			sug.Current = &cur
			sug.Ranked = []status.ChannelScore{cur, {Ch: 52, Score: 5, WorstBusy: 5, DFS: true}, {Ch: 100, Score: 5, WorstBusy: 5, DFS: true}, {Ch: 36, Score: 19, WorstBusy: 14, Nets: 1}}
			sug.Note = "this link's channel is the clearest one surveyed"
		default:
			sug.Note = "HaLow radios report no channel survey"
		}
		sv.Suggestions = append(sv.Suggestions, sug)
	}
	return sv
}

// SetMeshProfile answers as a node that sends a change to every node: a
// switch in a minute, the other nodes answering one by one.
func (f *fake) SetMeshProfile(ctx context.Context, in api.MeshProfileInput, dryRun bool) (*api.ApplyResult, error) {
	res, err := f.SetProfile(ctx, in.ProfileInput, true)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// As meshd: a change on its way is replaced only on purpose; the
	// profile in force, sent with replace, cancels it.
	pending := f.meshChange
	if pending == nil || pending.State != "pending" || f.meshChangeTo == nil {
		pending = nil
	}
	if pending != nil && !in.Replace && (in.Base != "" || !dryRun) {
		return nil, api.Conflict("a mesh-wide change from %s is on its way (every node switches at %s): sending this now would replace it and drop what it changes. Wait for the switch, or replace it on purpose (Replace it in the web UI; meshctl profile push --replace)", pending.From, pending.ApplyAt.Format("15:04:05"))
	}
	cancels := pending != nil && in.Replace && !res.Changed
	delay := time.Minute
	if in.DelaySeconds != 0 {
		delay = time.Duration(in.DelaySeconds) * time.Second
	}
	at := time.Now().Add(delay).Truncate(time.Second)
	var effects []status.Warning
	linksChange := false
	for _, e := range res.Effects {
		switch e.Code {
		case "link-restarts", "links-drop":
			linksChange = true
		case "this-node-only", "leaves-mesh":
		default:
			effects = append(effects, e)
		}
	}
	// As meshd: only links whose settings change restart.
	if linksChange {
		effects = append(effects, status.Warning{Level: "warning", Code: "mesh-change-restart",
			Message: "At switchover all nodes restart the 5 GHz · ch 149 link: down a few seconds, longer on DFS channels (CAC). Traffic uses other links meanwhile."})
	}
	res.Effects = append(effects, status.Warning{Level: "info", Code: "mesh-change",
		Message: fmt.Sprintf("Switchover at %s (in %s): this node and ridge-2, truck-3, alpha-4, bravo-5 and ops-6.", at.Format("15:04:05"), delay)},
		status.Warning{Level: "warning", Code: "mesh-change-old", Message: "scout-7: version cannot follow mesh changes. Install the profile manually, or the old one stays."})
	res.ApplyAt, res.Nodes = &at, []string{"alpha-4", "bravo-5", "ops-6", "ridge-2", "truck-3"}
	if pending != nil {
		what := ""
		if pending.Summary != "" {
			what = " (" + pending.Summary + ")"
		}
		if cancels {
			res.Effects = append(res.Effects, status.Warning{Level: "warning", Code: "mesh-change-cancelled", Message: fmt.Sprintf("Cancels the change from %s due at %s%s. All nodes keep the current profile (new revision).", pending.From, pending.ApplyAt.Format("15:04:05"), what)})
		} else {
			res.Effects = append(res.Effects, status.Warning{Level: "warning", Code: "mesh-change-replaced", Message: fmt.Sprintf("Supersedes the change from %s due at %s%s; its settings not in this profile are dropped.", pending.From, pending.ApplyAt.Format("15:04:05"), what)})
		}
	}
	if dryRun || (!res.Changed && !cancels) {
		return res, nil
	}
	p, err := f.profileFrom(in.ProfileInput)
	if err != nil {
		return nil, err
	}
	p.Revision = f.profile.Revision + 1
	if p.Revision < 4 {
		p.Revision = 4
	}
	if pending != nil {
		p.Revision = pending.Revision + 1
	}
	summary := "5 GHz · ch 149 at 80 MHz"
	if n := len(p.SiteLink.Members); n > 0 {
		summary += fmt.Sprintf(", site link with %d member%s", n, map[bool]string{true: "", false: "s"}[n == 1])
	}
	f.meshChange = &status.MeshChange{Revision: p.Revision, ApplyAt: at, From: f.node.Callsign, Sender: true, State: "pending", Summary: summary}
	f.meshChangeSent, f.meshChangeTo = time.Now(), p
	res.Applied, res.Revision = true, p.Revision
	return res, nil
}

func (f *fake) SetProfile(ctx context.Context, in api.ProfileInput, dryRun bool) (*api.ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := stale(in.Base, f.profile.Digest(), "The mesh profile"); err != nil {
		return nil, err
	}
	p, err := f.profileFrom(in)
	if err != nil {
		return nil, err
	}
	was, _ := json.Marshal(f.profile)
	now, _ := json.Marshal(p)
	// The node's own check, over the made-up mesh: this node's direct
	// neighbours are all on its first link, it has one radio per band (so
	// a second link on a band already used finds none), and the other
	// nodes report their radios.
	view := meshd.ProfileView{Peers: make([][]string, 1+len(f.profile.Links)), Callsign: f.node.Callsign, SiteKey: f.siteKey}
	for _, n := range nodes[1:] {
		view.Nodes = append(view.Nodes, status.MeshNode{Callsign: n.callsign, MeshRadios: n.radios(), SiteKey: siteKeyOf(n.callsign)})
	}
	for _, l := range links {
		if l.a == 0 {
			view.Peers[0] = append(view.Peers[0], nodes[l.b].callsign)
		}
	}
	view.Carries = func(i int) bool {
		l, ok := p.Link(i)
		for j := 0; ok && j < i; j++ {
			if prev, _ := p.Link(j); prev.Band == l.Band {
				return false
			}
		}
		return ok
	}
	res := &api.ApplyResult{Changed: string(was) != string(now), Effects: meshd.ProfileEffects(&f.profile, p, false, view)}
	if !dryRun && res.Changed {
		f.profile, res.Applied = *p, true
	}
	return res, nil
}

// joinsMesh is meshd's own sentence for the mesh a node in setup joins.
func joinsMesh(p *config.Profile) string {
	for _, e := range meshd.ProfileEffects(nil, p, true, meshd.ProfileView{}) {
		if e.Code == "joins-mesh" {
			return e.Message
		}
	}
	return ""
}

func (f *fake) Setup(ctx context.Context, in api.SetupInput, dryRun bool) (*api.ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.profileFrom(in.ProfileInput)
	if err != nil {
		return nil, err
	}
	n := f.node
	n.Callsign, n.NodeID = strings.TrimSpace(in.Callsign), in.NodeID
	if err := n.Validate(); err != nil {
		return nil, err
	}
	res := &api.ApplyResult{Changed: true, Effects: []status.Warning{
		eff("warning", "ap-credentials", "The setup Wi-Fi goes away. The node's own client Wi-Fi replaces it under a new name and passphrase: note them before applying if that is how you are connected."),
		eff("info", "joins-mesh", joinsMesh(p)),
	}}
	if p.Regulatory.Mode == config.RegModeUnrestricted {
		res.Effects = append([]status.Warning{eff("warning", "regulatory", "Regulatory mode is unrestricted: radios use every channel and the full power the hardware allows. That is lawful only with frequency authorization; without it, use country mode.")}, res.Effects...)
	}
	keepNode, keepSetup := f.node, f.setup
	f.node, f.setup = n, false
	res.AP = f.apJoin()
	if dryRun {
		f.node, f.setup = keepNode, keepSetup
		return res, nil
	}
	f.profile, res.Applied = *p, true
	return res, nil
}

func (f *fake) ProfileExport() (*api.ProfileExport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setup {
		return nil, api.Errorf(409, "this node has no mesh profile yet: there is nothing to share")
	}
	y, _ := f.profile.YAML("Trellis mesh profile. Share it with every node in the mesh.\nIt holds the mesh secret: keep it private.")
	code, _ := f.profile.Code()
	out := &api.ProfileExport{Profile: f.profile, YAML: string(y), Code: code}
	if c, err := qr.Encode(code, qr.M); err == nil {
		out.QR = &status.QR{Size: c.Size, Rows: c.Rows()}
	}
	return out, nil
}

func (f *fake) Topology(ctx context.Context) (*status.Topology, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &status.Topology{Time: time.Now(), IntervalMS: 5000, Nodes: []status.TopoNode{}, Links: []status.TopoLink{}}
	if f.setup {
		t.Nodes = append(t.Nodes, status.TopoNode{ID: nodes[0].mac, Callsign: f.node.Callsign, Self: true, Reported: true})
		return t, nil
	}
	for i, n := range nodes {
		tn := status.TopoNode{ID: n.mac, Callsign: n.callsign, NodeID: n.id, Address: fmt.Sprintf("10.73.%d.1", n.id), Self: i == 0, Version: "demo",
			Gateway: n.gw, WANHealthy: n.wanOK, Reported: i != 6, ReportAgeMS: int64(rand.IntN(4000)), Clients: n.clients, SelectedGateway: n.sel}
		if i == 0 {
			tn.Callsign = f.node.Callsign
			if f.node.Gateway.Mode == config.GatewayOff {
				tn.Gateway, tn.SelectedGateway = false, 41
			}
		}
		if i == 6 { // an older build: it does not answer link queries
			tn.Clients, tn.SelectedGateway, tn.Version = 0, 0, "4a48da6+1104.072526"
		}
		t.Nodes = append(t.Nodes, tn)
	}
	for _, l := range links {
		tl := status.TopoLink{A: nodes[l.a].mac, B: nodes[l.b].mac, ABMbps: drift(l.ab, 6), BAMbps: drift(l.ba, 6),
			ASignalDBm: l.sigA + rand.IntN(3) - 1, BSignalDBm: l.sigB + rand.IntN(3) - 1, Medium: "wifi", AMAC: nodes[l.a].orig, BMAC: nodes[l.b].orig, MeshLink: 1}
		if l.b == 6 {
			tl.BAMbps, tl.BSignalDBm = 0, 0
		}
		t.Links = append(t.Links, tl)
	}
	if len(nodes) > 7 {
		mbit := float64(f.profile.SiteLink.ThroughputMbit())
		t.Links = append(t.Links,
			status.TopoLink{A: nodes[0].mac, B: nodes[7].mac, ABMbps: mbit, BAMbps: mbit, Medium: "site", AMAC: nodes[0].orig, BMAC: nodes[7].orig},
			status.TopoLink{A: nodes[7].mac, B: nodes[8].mac, ABMbps: drift(depotLink.ab, 6), BAMbps: drift(depotLink.ba, 6),
				ASignalDBm: depotLink.sigA, BSignalDBm: depotLink.sigB, Medium: "wifi", AMAC: nodes[7].orig, BMAC: nodes[8].orig, MeshLink: 1})
	}
	if k := len(f.profile.Links); k > 0 && !f.oneRadio {
		for _, l := range lastLinks {
			t.Links = append(t.Links, status.TopoLink{A: nodes[l.a].mac, B: nodes[l.b].mac, ABMbps: drift(l.ab, 6), BAMbps: drift(l.ba, 6),
				ASignalDBm: l.sigA, BSignalDBm: l.sigB, Medium: "wifi", AMAC: linkMAC(nodes[l.a].orig), BMAC: linkMAC(nodes[l.b].orig), MeshLink: k + 1})
		}
	}
	return t, nil
}

// SiteLinkNewKey gives the demo node a made-up new key, refused for a
// member without force as meshd does.
func (f *fake) SiteLinkNewKey(force bool) (*api.SiteKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.profile.SiteLink.Member(f.siteKey); ok && !force {
		return nil, api.Errorf(409, "this node holds the site link for site %q under its current key: with a new one it leaves the link until the profile lists the new key in its place (force to go on)", m.Site)
	}
	was := f.siteKey
	sum := sha256.Sum256([]byte(was + time.Now().String()))
	f.siteKey = base64.StdEncoding.EncodeToString(sum[:])
	return &api.SiteKey{PublicKey: f.siteKey, Was: was}, nil
}

func (f *fake) IdentifyRadio(fp string, seconds int) error {
	if strings.HasPrefix(fp, "pci:") {
		return nil
	}
	if strings.HasPrefix(fp, "mac:") {
		return api.Errorf(409, "MediaTek MT7612U (phy2, usb:1-1.3) has no LED the driver exposes; it cannot be identified this way")
	}
	return api.Errorf(404, "no radio with fingerprint %q is present", fp)
}

func (f *fake) Doctor(ctx context.Context) (string, error) {
	return "Trellis doctor (demo data)\n\n  [ ok ] kernel 6.18.60+rpt-rpi-v8, arm64\n  [ ok ] batman-adv 2026.3 (DKMS), BATMAN_V available\n  [ ok ] bridge IGMP snooping\n" +
		"  [ ok ] wpa_supplicant 2.11 with mesh and SAE\n  [ ok ] hostapd, dnsmasq, nft\n  [warn] regulatory.db: fallback database in use\n  [ ok ] radios: 3 found, 2 in use\n" +
		"  [ ok ] node.yaml and profile.yaml valid\n  [ ok ] meshd running, converged\n\n0 failed, 1 warning\n", nil
}

func (f *fake) WebInfo() api.WebInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return api.WebInfo{Callsign: f.node.Callsign, Version: "demo", AdminSet: f.password != "", Setup: f.setup, WANAccess: f.node.Web.WANAccess, Prefix: f.profile.Prefix}
}

func (f *fake) WebLogin(from, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.password == "" {
		return api.Forbidden("no admin password is set on this node")
	}
	if password != f.password {
		f.fails++
		if f.fails > 4 {
			f.fails = 0
			return api.TooMany(8*time.Second, "too many wrong passwords; wait 8 s and try again")
		}
		return api.ErrBadPassword
	}
	f.fails = 0
	return nil
}

func (f *fake) WebSetPassword(from, current, new string, privileged bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.password != "" && current != f.password:
		return api.Errorf(401, "the current password is wrong")
	case f.password == "" && !f.setup:
		return api.Forbidden("no admin password is set on this node, and it is already part of a mesh. Set one on the node itself: sudo meshctl web password")
	case len(new) < 8:
		return api.Invalid("the password must be 8 to 128 characters (got %d)", len(new))
	}
	f.password = new
	return nil
}

func main() {
	socket := flag.String("socket", "", "unix socket to serve on (required)")
	setup := flag.Bool("setup", false, "pretend the node waits to be set up (no profile, no admin password)")
	password := flag.String("password", "trellis-demo", "the admin password (ignored with --setup)")
	bare := flag.Bool("bare", false, "a node with no per-radio rules, as freshly installed")
	oneRadio := flag.Bool("one-radio", false, "a node with one dual-band adapter in a 2.4 + 5 GHz profile")
	portal := flag.Bool("portal", false, "the uplink is behind a login page, and no other gateway is usable")
	unrestricted := flag.Bool("unrestricted", false, "a mesh profile in unrestricted regulatory mode")
	links := flag.Int("links", 1, "mesh links: 2 adds a HaLow radio and link, 3 a 2.4 GHz link as well")
	consoleSock := flag.String("console", "", "also serve a made-up voice console socket here (webd --voice-console)")
	troubled := flag.Bool("troubled", false, "a node with hardware trouble: a PCIe link that loses packets, a throttling radio, resets, a trial that was undone")
	flag.Parse()
	if *socket == "" {
		fmt.Fprintln(os.Stderr, "usage: fakemeshd --socket PATH [--setup] [--links N] [--bare] [--password PW]")
		os.Exit(2)
	}
	if *setup {
		*password = ""
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	f := newFake(*setup, *password, *links, *bare)
	if *unrestricted && !*setup {
		f.profile.Regulatory.Mode = config.RegModeUnrestricted
	}
	if *oneRadio {
		f.oneRadioNode()
	}
	f.portal = *portal
	f.troubled = *troubled
	if *consoleSock != "" {
		if err := f.serveConsole(*consoleSock); err != nil {
			fmt.Fprintln(os.Stderr, "fakemeshd:", err)
			os.Exit(1)
		}
	}
	srv := &api.Server{Provider: f, Socket: *socket, Log: slog.Default(), Mode: 0o600}
	if err := srv.Serve(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "fakemeshd:", err)
		os.Exit(1)
	}
}

// hardware fills in what the radios' hardware says about itself: the PCIe
// link and temperature of the dual-band card, what each link's width costs
// on its radio, the other nodes' degraded radios, and, on a troubled node,
// resets and trials.
func (f *fake) hardware(st *status.Status, now time.Time) {
	link := &status.PCIeLink{Device: "0000:01:00.0", Port: "0000:00:00.0", SpeedGTs: 2.5, Width: 1, MaxSpeedGTs: 5, MaxWidth: 1, Gen: 1, MaxGen: 2,
		Counted: true, Health: "ok", HealthDetail: "no errors", SampledAt: now.Add(-6 * time.Second)}
	temp, duty := 58.0, 100
	if f.troubled {
		link.Health, link.ReplayPct, link.PeakReplayPct = "marginal", 14, 71
		link.HealthDetail = "replay rollovers 14% of 10 ms windows (~15/s, peak 71%)"
		link.Errors = []string{"BadTLP", "BadDLLP", "Rollover", "Timeout"}
		temp, duty = 121, 50
		// A node on another passphrase: it never connects, so its
		// failures stay listed.
		st.MeshLink.SAEFailures = []status.SAEEvent{
			{Time: now.Add(-40 * time.Second), Interface: "mesh0", MAC: "a0:b0:c0:00:77:01", Kind: "blocked", Message: "SAE blocked: repeated failures. Retry in 21 s"},
			{Time: now.Add(-95 * time.Second), Interface: "mesh0", MAC: "a0:b0:c0:00:77:01", Kind: "failure", Message: "SAE authentication failed"},
		}
		st.Warnings = append(st.Warnings,
			status.Warning{Level: "warning", Code: "pcie-link-marginal", Source: "hardware", Message: "PCIe link to MediaTek MT7915 (0000:01:00.0) marginal: " + link.HealthDetail + ". Risk of card hang and node reset. Reseat card and adapter, or change adapter or slot."},
			status.Warning{Level: "warning", Code: "radio-throttling", Source: "hardware", Message: "MediaTek MT7915 (5g/6g) thermal throttling: 121 °C, Tx duty 50%. Improve card cooling."},
			status.Warning{Level: "warning", Code: "node-reset", Source: "hardware", Message: "2 unplanned resets in 24 h, last at " + now.Add(-47*time.Minute).Format("15:04 2 Jan") + ": card hang in mt76, mt7915e (SError): suspect PCIe link or power."},
			status.Warning{Level: "warning", Code: "trial-reverted", Source: "trials", Message: "Trial mt7915e-power 2 reverted: reboot before confirmation. Locked out; re-arm: sudo meshctl trial rearm mt7915e-power."},
			status.Warning{Level: "warning", Code: "channel-busy", Source: "survey", Message: "Channel busy: 5 GHz · ch 149, 74 % at bravo-5 (mesh traffic included). Run channel survey"})
		st.Resets = []status.Reset{
			{Time: now.Add(-47 * time.Minute), Verdict: "card-stopped", UptimeS: 163.9, Detail: "SError Interrupt on CPU2, code 0x00000000bf000002 -- SError",
				Summary: "card hang in mt76, mt7915e (SError): suspect PCIe link or power"},
			{Time: now.Add(-5 * time.Hour), Verdict: "power-loss", Detail: "the boot before started " + now.Add(-9*time.Hour).Format("2006-01-02 15:04"),
				Summary: "no crash record: power loss or external reset (watchdog, reset line)"},
			{Time: now.Add(-50 * time.Hour), Verdict: "sysrq", UptimeS: 5123.4,
				Summary: "SysRq reset from serial console: disconnect the console or set kernel.sysrq=0"},
			{Time: now.Add(-6 * 24 * time.Hour), TimeUnknown: true, Verdict: "power-loss",
				Summary: "no crash record: power loss or external reset (watchdog, reset line)"},
		}
		st.Trials = []status.Trial{
			{Name: "halow-bcf", Kind: "bcf", Version: "27 dBm", Module: "morse", State: "pending", ConfirmAfterS: 214, Started: now.Add(-86 * time.Second),
				Files: []string{"/lib/firmware/morse/bcf_fgh100mhaamd.bin"}},
			{Name: "mt7915e-power", Kind: "module", Version: "2", Module: "mt7915e", State: "reverted", Latched: true, Started: now.Add(-49 * time.Minute), Ended: now.Add(-46 * time.Minute),
				Reason: "reboot before confirmation", Files: []string{"/lib/modules/6.18.60/updates/dkms/mt7915e.ko"}},
		}
	}
	byWidth := map[string]map[int]float64{"2g": {20: 20, 40: 17}, "5g": {20: 20, 40: 17, 80: 13.5, 160: 13.5}, "6g": {20: 20, 40: 17, 80: 13.5, 160: 13.5}}
	for i := range st.Radios {
		r := &st.Radios[i]
		if r.Driver != "mt7915e" {
			continue
		}
		r.PCIe, r.PowerByWidth = link, byWidth
		r.Thermal = &status.RadioThermal{TempC: temp, ThrottleAtC: 120, DutyPct: duty, Throttling: duty < 100}
		// What the width of the link it carries costs on it.
		for _, l := range append([]*status.MeshLink{&st.MeshLink}, linkPtrs(st.MoreLinks)...) {
			role := "mesh"
			if l.Index > 1 {
				role = fmt.Sprintf("mesh%d", l.Index)
			}
			pw, ok := byWidth[l.Band][l.WidthMHz]
			if (r.Role != role && r.Role != role+"+ap") || l.WidthMHz <= 20 || !ok {
				continue
			}
			noise := math.Round(100*math.Log10(float64(l.WidthMHz)/20)) / 10
			l.WidthCost = &status.WidthCost{WidthMHz: l.WidthMHz, BaseMHz: 20, NoiseDB: noise, PowerKnown: true, PowerDBm: pw, BasePowerDBm: 20,
				PowerDB: 20 - pw, TotalDB: math.Round(10*(noise+20-pw)) / 10}
		}
	}
	st.Host = f.host(now)
	for _, w := range hoststat.Check(st.Host, now) {
		st.Warnings = append(st.Warnings, status.Warning{Level: w.Level, Code: w.Code, Message: w.Message, Source: "hardware"})
	}
	for i := range st.Nodes {
		switch i {
		case 1:
			st.Nodes[i].Degraded = []status.NodeDegraded{{Bands: []string{"5g"}, Reason: "thermal"}}
		case 2:
			st.Nodes[i].Degraded = []status.NodeDegraded{{Bands: []string{"2g"}, Reason: "pcie"}, {Bands: []string{"5g"}, Reason: "pcie"}}
		}
	}
}

// host is a CM4, its figures wandering a little; on a
// troubled node, hot, short of power and with its card nearly full.
func (f *fake) host(now time.Time) status.Host {
	up := now.Sub(f.started).Seconds() + 3*86400 + 4*3600
	wobble := math.Sin(now.Sub(f.started).Seconds() / 20)
	low := false
	h := status.Host{Model: "Raspberry Pi Compute Module 4 Rev 1.1", UptimeS: up, CPUs: 4,
		CPUPct: math.Round(10*(6+4*wobble+3*rand.Float64())) / 10, Load: [3]float64{0.31 + 0.1*wobble, 0.27, 0.22},
		CPUMHz: 1500, CPUMaxMHz: 1500,
		MemTotal: 1_933_062_144, MemAvailable: 1_190_000_000 - uint64(20e6*wobble), SwapTotal: 1_932_521_472, SwapFree: 1_932_521_472,
		Temps:        []status.HostTemp{{Name: "cpu-thermal", TempC: math.Round(10*(48+1.5*wobble)) / 10, HotC: 80, CritC: 110}},
		Undervoltage: &low,
		Filesystems: []status.Filesystem{
			{Mount: "/", Device: "/dev/mmcblk0p2", Type: "ext4", Size: 30_168_961_024, Used: 4_654_776_320, Avail: 24_243_888_128},
			{Mount: "/boot/firmware", Device: "/dev/mmcblk0p1", Type: "vfat", Size: 528_592_896, Used: 69_558_272, Avail: 459_034_624},
		}}
	if f.troubled {
		yes, at := true, now.Add(-2*time.Second)
		h.Temps[0].TempC = math.Round(10*(82+wobble)) / 10
		h.CPUMHz, h.Undervoltage, h.UndervoltageAt = 1000, &yes, &at
		h.Filesystems[0].Used, h.Filesystems[0].Avail = 27_900_000_000, 2_268_961_024-1_500_000_000
	}
	return h
}

func linkPtrs(ls []status.MeshLink) []*status.MeshLink {
	out := make([]*status.MeshLink, len(ls))
	for i := range ls {
		out[i] = &ls[i]
	}
	return out
}
