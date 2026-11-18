package meshd

import (
	"strings"
	"testing"

	"meridian/trellis/internal/nodestate"
	"meridian/trellis/internal/status"
)

// Two gateways with one node ID would share a routing table and a
// connection mark: only the one that keeps the ID is used.
func TestDropSharedIDs(t *testing.T) {
	gws := func() ([]*gwChoice, []status.GatewayCandidate) {
		us := []*gwChoice{
			{MAC: "02:00:00:00:00:09", ID: 7, Callsign: "high-mac", idSource: nodestate.IDDerived},
			{MAC: "02:00:00:00:00:01", ID: 7, Callsign: "low-mac", idSource: nodestate.IDDerived},
			{MAC: "02:00:00:00:00:05", ID: 8, Callsign: "alone", idSource: nodestate.IDDerived},
			{MAC: "02:00:00:00:00:0a", ID: 3, Callsign: "mine", idSource: nodestate.IDDerived},
		}
		cs := make([]status.GatewayCandidate, len(us))
		for i, u := range us {
			u.cand = i
			cs[i] = status.GatewayCandidate{Callsign: u.Callsign, Usable: true}
		}
		return us, cs
	}
	me := &nodestate.Identity{MAC: "02:00:00:00:00:02", NodeID: 3}

	us, cs := gws()
	got := dropSharedIDs(us, cs, me)
	var names []string
	for _, g := range got {
		names = append(names, g.Callsign)
	}
	if strings.Join(names, ",") != "low-mac,alone" {
		t.Errorf("kept %v, want the lower MAC of ID 7 and the gateway alone on ID 8", names)
	}
	if cs[0].Usable || !strings.Contains(cs[0].Why, "shares node ID 7 with low-mac") {
		t.Errorf("the dropped gateway's candidate: %+v", cs[0])
	}
	if cs[3].Usable || !strings.Contains(cs[3].Why, "this node's ID 3") {
		t.Errorf("a gateway with this node's ID: %+v", cs[3])
	}
	if !cs[1].Usable || !cs[2].Usable {
		t.Errorf("kept gateways marked unusable: %+v", cs)
	}

	// A pinned ID beats a lower MAC.
	us, cs = gws()
	us[0].idSource = nodestate.IDConfig
	if got := dropSharedIDs(us, cs, nil); len(got) != 3 || got[0].Callsign != "high-mac" {
		t.Errorf("pinned ID: kept %+v", got)
	}

	// No conflict: the list is untouched.
	us, cs = gws()
	us = us[2:3]
	if got := dropSharedIDs(us, cs, me); len(got) != 1 || !cs[2].Usable {
		t.Errorf("no conflict: %+v", got)
	}
}
