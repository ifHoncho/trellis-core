package meshd

import (
	"net"
	"strings"
	"testing"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/nodestate"
)

func TestResolveIdentity(t *testing.T) {
	me := &nodestate.Identity{MAC: "0a:00:00:00:00:05", NodeID: 2, NodeIDSource: nodestate.IDDerived}
	lower := "02:00:00:00:00:01"
	higher := "0e:00:00:00:00:01"

	t.Run("no conflict", func(t *testing.T) {
		dec, err := resolveIdentity(me, nil, 1, []gossip.Record{{MAC: lower, ID: 3}}, nil)
		if err != nil || dec.Next != nil {
			t.Fatalf("got %+v, %v", dec, err)
		}
	})
	t.Run("lower MAC keeps it", func(t *testing.T) {
		peers := []gossip.Record{{MAC: lower, ID: 2, IDSource: nodestate.IDDerived, Callsign: "a"}, {MAC: higher, ID: 9}}
		dec, err := resolveIdentity(me, nil, 1, peers, nil)
		if err != nil || dec.Next == nil {
			t.Fatalf("got %+v, %v", dec, err)
		}
		if n := dec.Next; n.NodeID == 2 || n.NodeID == 9 || n.NodeIDSource != nodestate.IDClaimed || n.MAC != me.MAC {
			t.Fatalf("next = %+v", n)
		}
		if me.NodeID != 2 {
			t.Fatal("resolveIdentity modified the current identity")
		}
		if !strings.Contains(dec.Reason, "lower MAC") {
			t.Errorf("reason %q", dec.Reason)
		}
	})
	t.Run("higher MAC does not take it", func(t *testing.T) {
		dec, _ := resolveIdentity(me, nil, 1, []gossip.Record{{MAC: higher, ID: 2}}, nil)
		if dec.Next != nil {
			t.Fatalf("moved off an ID it keeps: %+v", dec.Next)
		}
	})
	t.Run("a pin beats a lower MAC", func(t *testing.T) {
		pinned := *me
		pinned.NodeIDSource = nodestate.IDConfig
		dec, _ := resolveIdentity(&pinned, nil, 1, []gossip.Record{{MAC: lower, ID: 2, IDSource: nodestate.IDDerived}}, nil)
		if dec.Next != nil {
			t.Fatal("a pinned node gave its ID to a derived one")
		}
		dec, _ = resolveIdentity(me, nil, 1, []gossip.Record{{MAC: higher, ID: 2, IDSource: nodestate.IDConfig}}, nil)
		if dec.Next == nil || !strings.Contains(dec.Reason, "pinned") {
			t.Fatalf("a derived node kept an ID pinned elsewhere: %+v", dec)
		}
	})
	t.Run("clone: higher hard-interface MAC takes a new identity", func(t *testing.T) {
		clone := &gossip.Record{MAC: me.MAC, ID: 2, Orig: []string{"9c:00:00:00:00:01"}, Boot: 5}
		dec, err := resolveIdentity(me, []string{"9c:00:00:00:00:02"}, 6, nil, clone)
		if err != nil || dec.Next == nil {
			t.Fatalf("got %+v, %v", dec, err)
		}
		mac, perr := net.ParseMAC(dec.Next.MAC)
		if perr != nil || dec.Next.MAC == me.MAC || mac[0]&0x02 == 0 || mac[0]&0x01 != 0 {
			t.Fatalf("new MAC %q", dec.Next.MAC)
		}
		if dec.Next.NodeID != nodestate.DeriveNodeID(mac) || dec.Next.NodeIDSource != nodestate.IDDerived {
			t.Fatalf("new ID %+v", dec.Next)
		}
		dec, _ = resolveIdentity(me, []string{"9c:00:00:00:00:00"}, 6, nil, clone)
		if dec.Next != nil {
			t.Fatal("the clone with the lower hard-interface MAC changed")
		}
	})
}

func TestIsNodeAddress(t *testing.T) {
	_, prefix, _ := net.ParseCIDR("10.73.0.0/16")
	cases := map[string]bool{
		"10.73.2.1/16":   true,
		"10.73.254.1/16": true,
		"10.73.0.1/16":   false, // the anycast address's network, not a node
		"10.73.255.1/16": false,
		"10.73.2.5/16":   false,
		"10.73.2.1/24":   false,
		"10.74.2.1/16":   false,
	}
	for s, want := range cases {
		ip, n, _ := net.ParseCIDR(s)
		n.IP = ip
		if got := isNodeAddress(prefix, n); got != want {
			t.Errorf("isNodeAddress(%s) = %v, want %v", s, got, want)
		}
	}
}

// A node ID change worked out from an identity read a moment earlier
// keeps the site link key the API made meanwhile.
func TestLoopIdentityChangeKeepsNewKey(t *testing.T) {
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	d.ident = &nodestate.Identity{MAC: "02:00:00:00:00:01", NodeID: 1, NodeIDSource: nodestate.IDDerived, SiteKey: "old-key"}
	// The loop reads the identity, and decides on a new node ID.
	next := *d.identity()
	next.NodeID = 9
	// Meanwhile the API gives the node a new key.
	k, err := d.SiteLinkNewKey(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.setIdentityFromLoop(&next); err != nil {
		t.Fatal(err)
	}
	if got := d.sitePublicKey(); got != k.PublicKey || d.identity().NodeID != 9 {
		t.Errorf("after both: key %q (the API answered %q), node ID %d", got, k.PublicKey, d.identity().NodeID)
	}
}

// A new site link key is not a new address: the AP and the client ports,
// which restart when the node is re-addressed, are left alone.
func TestNewSiteKeyIsNotReaddressing(t *testing.T) {
	d, _ := testDaemon(t, map[string]string{"node.yaml": "schema: 0\ncallsign: node-1\n", "profile.yaml": testProfile})
	d.ident = &nodestate.Identity{MAC: "02:00:00:00:00:01", NodeID: 1, NodeIDSource: nodestate.IDDerived, SiteKey: "old-key"}
	gen := d.identityGen()
	if _, err := d.SiteLinkNewKey(false); err != nil {
		t.Fatal(err)
	}
	if got := d.identityGen(); got != gen {
		t.Errorf("a new site link key counted as a re-addressing (generation %d -> %d)", gen, got)
	}
	next := *d.identity()
	next.NodeID = 9
	if err := d.setIdentityFromLoop(&next); err != nil {
		t.Fatal(err)
	}
	if got := d.identityGen(); got == gen {
		t.Error("a new node ID did not count as a re-addressing")
	}
}
