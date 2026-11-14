package meshd

import (
	"testing"

	"meridian/trellis/internal/config"
)

// A peer that moved its radio to another link is live there and a leftover
// where it was; only the leftover goes, and only once it is clearly silent.
func TestMovedPeers(t *testing.T) {
	node2, alpha5 := "a0:b0:c0:00:02:26", "a0:b0:c0:00:81:ea"
	for name, c := range map[string]struct {
		peers   []linkPeer
		stale   []linkPeer
		pending bool
	}{
		"one link each": {peers: []linkPeer{{0, alpha5, 40, true}, {1, node2, 20, true}}},
		// node-2 moved to 5 GHz (mesh1), still listed on mesh0.
		"moved": {peers: []linkPeer{{0, node2, 151684, true}, {1, node2, 80, true}, {0, alpha5, 40, true}},
			stale: []linkPeer{{0, node2, 151684, true}}},
		"moved the other way": {peers: []linkPeer{{0, node2, 12, true}, {1, node2, 9000, true}},
			stale: []linkPeer{{1, node2, 9000, true}}},
		"just moved: wait": {peers: []linkPeer{{0, node2, 2000, true}, {1, node2, 30, true}}, pending: true},
		// Two adapters with one address (cheap clones) on two links: both
		// live, both stay.
		"same address on two live radios": {peers: []linkPeer{{0, node2, 30, true}, {1, node2, 60, true}}},
		// Gone from both: its entries time out by themselves.
		"silent everywhere": {peers: []linkPeer{{0, node2, 9000, true}, {1, node2, 8000, true}}},
		"three links": {peers: []linkPeer{{0, node2, 7000, true}, {1, node2, 100, true}, {2, node2, 5000, true}},
			stale: []linkPeer{{0, node2, 7000, true}, {2, node2, 5000, true}}},
	} {
		stale, pending := movedPeers(c.peers)
		if len(stale) != len(c.stale) || pending != c.pending {
			t.Errorf("%s: stale %v pending %v, want %v %v", name, stale, pending, c.stale, c.pending)
			continue
		}
		for _, want := range c.stale {
			found := false
			for _, got := range stale {
				found = found || got == want
			}
			if !found {
				t.Errorf("%s: stale %v, want %v among them", name, stale, want)
			}
		}
	}
}

// A peer candidate heard on another link's channel (while the radio
// scanned, or before the peer changed band) never gets established on this
// link and stays silent here: it goes after a minute. Established peers,
// and candidates still being heard, stay.
func TestStrayPeers(t *testing.T) {
	node2, alpha5, picm := "a0:b0:c0:00:02:26", "a0:b0:c0:00:81:ea", "a0:b0:c0:00:89:fa"
	peers := []linkPeer{
		{1, picm, 40, true},                  // the real peer on 5 GHz
		{1, node2, 185000, false},            // heard on 2.4 GHz during the scan
		{1, alpha5, 20000, false},            // the same, not a minute yet
		{0, picm, 70000, true},               // established: never a stray, whatever its silence
		{1, "a0:b0:c0:00:52:de", 300, false}, // on the channel, peering now
	}
	got := strayPeers(peers)
	if len(got) != 1 || got[0].mac != node2 {
		t.Errorf("strays %v, want only %s", got, node2)
	}
}

// The plan is made afresh when the configuration asks for another one, and
// only then: a callsign or a channel is not a reason to move roles.
func TestPlanKey(t *testing.T) {
	node := func(mut func(*config.Node)) *config.Node {
		n := &config.Node{Callsign: "a"}
		n.ApplyDefaults()
		if mut != nil {
			mut(n)
		}
		return n
	}
	prof := func(bands ...string) *config.Profile {
		p := &config.Profile{Radio: config.RadioPlan{Band: bands[0], Channel: 6}}
		for _, b := range bands[1:] {
			p.Links = append(p.Links, config.RadioPlan{Band: b, Channel: 149})
		}
		return p
	}
	base := planKey(node(nil), prof("2g", "5g"))
	same := map[string]string{
		"callsign": planKey(node(func(n *config.Node) { n.Callsign = "b" }), prof("2g", "5g")),
		"gateway":  planKey(node(func(n *config.Node) { n.Gateway.Mode = config.GatewayOff }), prof("2g", "5g")),
		"a label": planKey(node(func(n *config.Node) {
			n.Radios.Devices = []config.DeviceRule{{Match: config.DeviceMatch{Fingerprint: "mac:a0:b0:c0:00:02:26"}, Label: "roof"}}
		}), prof("2g", "5g")),
		"channel, width": planKey(node(nil), &config.Profile{Radio: config.RadioPlan{Band: "2g", Channel: 11, Width: 40}, Links: []config.RadioPlan{{Band: "5g", Channel: 36, Width: 160}}}),
	}
	for name, k := range same {
		if k != base {
			t.Errorf("%s changes the plan key:\n%s\n%s", name, base, k)
		}
	}
	off := false
	differ := map[string]string{
		"a pin": planKey(node(func(n *config.Node) {
			n.Radios.Roles.More = []config.RoleSelection{{Mode: "radio", Radio: "mac:a0:b0:c0:00:02:26"}}
		}), prof("2g", "5g")),
		"a role off":      planKey(node(func(n *config.Node) { n.Radios.Roles.AP.Mode = config.RoleDisabled }), prof("2g", "5g")),
		"the AP off":      planKey(node(func(n *config.Node) { n.AP.Enabled = &off }), prof("2g", "5g")),
		"a radio ignored": planKey(node(func(n *config.Node) { n.Radios.Devices = []config.DeviceRule{{Ignore: true}} }), prof("2g", "5g")),
		"links reordered": planKey(node(nil), prof("5g", "2g")),
		"a link added":    planKey(node(nil), prof("2g", "5g", "s1g")),
	}
	for name, k := range differ {
		if k == base {
			t.Errorf("%s leaves the plan key unchanged", name)
		}
	}
}
