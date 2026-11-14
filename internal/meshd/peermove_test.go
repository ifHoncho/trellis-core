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
		"one link each": {peers: []linkPeer{{0, alpha5, 40}, {1, node2, 20}}},
		// node-2 moved to 5 GHz (mesh1), still listed on mesh0.
		"moved": {peers: []linkPeer{{0, node2, 151684}, {1, node2, 80}, {0, alpha5, 40}},
			stale: []linkPeer{{0, node2, 151684}}},
		"moved the other way": {peers: []linkPeer{{0, node2, 12}, {1, node2, 9000}},
			stale: []linkPeer{{1, node2, 9000}}},
		"just moved: wait": {peers: []linkPeer{{0, node2, 2000}, {1, node2, 30}}, pending: true},
		// Two adapters with one address (cheap clones) on two links: both
		// live, both stay.
		"same address on two live radios": {peers: []linkPeer{{0, node2, 30}, {1, node2, 60}}},
		// Gone from both: its entries time out by themselves.
		"silent everywhere": {peers: []linkPeer{{0, node2, 9000}, {1, node2, 8000}}},
		"three links": {peers: []linkPeer{{0, node2, 7000}, {1, node2, 100}, {2, node2, 5000}},
			stale: []linkPeer{{0, node2, 7000}, {2, node2, 5000}}},
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
