package meshctl

import (
	"bytes"
	"strings"
	"testing"

	"meridian/trellis/internal/status"
)

func routesFixture() (status.Batman, []status.Peer) {
	n1, n2, pi, wired := "a0:b0:c0:00:01:f1", "a0:b0:c0:00:02:26", "a0:b0:c0:00:81:ea", "02:00:00:00:00:09"
	peers := []status.Peer{
		{MAC: n2, PlinkState: "ESTAB", Established: true, SignalDBm: -46, TXMbps: 72.2, RXMbps: 72.2, InactiveMS: 40, ConnectedSeconds: 888},
		{MAC: pi, PlinkState: "ESTAB", Established: true, SignalDBm: -55, TXMbps: 72.2, RXMbps: 72.2, InactiveMS: 210, ConnectedSeconds: 868},
		{MAC: n1, PlinkState: "ESTAB", Established: true, SignalDBm: -67, TXMbps: 72.2, RXMbps: 39, InactiveMS: 247, ConnectedSeconds: 406},
	}
	b := status.Batman{
		HardIfs: []status.HardIf{{Name: "mesh0", Active: true}},
		Neighbors: []status.Neighbor{
			{MAC: n1, Via: "mesh0", ThroughputMbps: 2.1},
			{MAC: pi, Via: "mesh0", ThroughputMbps: 23.9},
			{MAC: n2, Via: "mesh0", ThroughputMbps: 23.9},
			{MAC: wired, Via: "eth1", ThroughputMbps: 1000, LastSeenMS: 12},
		},
		Originators: []status.Originator{
			{MAC: n1, NextHop: n2, Via: "mesh0", ThroughputMbps: 11.9, Best: true},
			{MAC: n1, NextHop: n1, Via: "mesh0", ThroughputMbps: 2.1},
			{MAC: pi, NextHop: pi, Via: "mesh0", ThroughputMbps: 23.9, Best: true},
			{MAC: n2, NextHop: n2, Via: "mesh0", ThroughputMbps: 23.9, Best: true},
		},
	}
	return b, peers
}

func TestPrintRoutes(t *testing.T) {
	b, peers := routesFixture()
	var buf bytes.Buffer
	names := map[string]string{"a0:b0:c0:00:02:26": "node-2"}
	printLinks(&buf, peers, b.Neighbors, names)
	printRoutes(&buf, b, peers, names, nil)
	out := buf.String()
	t.Log("\n" + out)
	for _, want := range []string{
		"a0:b0:c0:00:01:f1  ?       relayed via node-2 (beats the direct link)",
		"3 other nodes: 2 direct, 1 relayed",
		"batman only (eth1)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	// Direct routes are listed before relayed ones.
	if strings.Index(out, "a0:b0:c0:00:81:ea  ?       direct") > strings.Index(out, "relayed via") {
		t.Error("direct routes should come first")
	}
}

// Two mesh links: node-2 is known by its first link's address (the
// originator) and reached over its second link. That is a direct route, and
// it was printed as "relayed via node-2".
func TestPrintRoutesSecondLink(t *testing.T) {
	n2a, n2b, r5 := "a0:b0:c0:00:02:26", "02:11:22:33:44:55", "a0:b0:c0:00:81:ea"
	b := status.Batman{
		HardIfs: []status.HardIf{{Name: "mesh0", Active: true}, {Name: "mesh1", Active: true}},
		Originators: []status.Originator{
			{MAC: n2a, NextHop: n2b, Via: "mesh1", ThroughputMbps: 80, Best: true},
			{MAC: r5, NextHop: n2b, Via: "mesh1", ThroughputMbps: 40, Best: true},
		},
	}
	peers := []status.Peer{{MAC: n2b, Established: true}, {MAC: r5, Established: true}}
	st := &status.Status{Nodes: []status.MeshNode{
		{Callsign: "node-2", MAC: "66:26:2e:a5:2a:c9", Originators: []string{n2a, n2b}},
		{Callsign: "alpha-unit", MAC: "6a:dc:ee:dc:bd:e8", Originators: []string{r5}},
	}}
	var buf bytes.Buffer
	printRoutes(&buf, b, peers, nodeNames(st), nodeOfAddr(st))
	out := buf.String()
	t.Log("\n" + out)
	for _, want := range []string{
		n2a + "  node-2      direct on mesh1",
		r5 + "  alpha-unit  relayed via node-2 (beats the direct link) on mesh1",
		"2 other nodes: 1 direct, 1 relayed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

// Each mesh link lists its own neighbours. A link this node has no radio
// for lists none (a 2.4 GHz-only node in a mesh with a 5 GHz second link),
// and a node on the second link alone shows them there.
func TestMeshLinkNeighbours(t *testing.T) {
	b, peers := routesFixture()
	st := &status.Status{Batman: b}
	st.MeshLink = status.MeshLink{Index: 1, Interface: "mesh0", Transport: "wifi+wired", Band: "2g", Channel: 6, Peers: peers, WiredPorts: []string{"eth1"}}
	st.MoreLinks = []status.MeshLink{{Index: 2, Transport: "none", Band: "5g", Channel: 149, Peers: []status.Peer{}}}
	var buf bytes.Buffer
	printMeshLink(&buf, "Mesh link 2", st.MoreLinks[0], st, false)
	if out := buf.String(); !strings.Contains(out, "no radio here for this link: 5g ch 149") || strings.Contains(out, "batman only") || strings.Contains(out, "DIRECT LINK") {
		t.Errorf("a link without a radio lists another link's neighbours:\n%s", out)
	}
	buf.Reset()
	printMeshLink(&buf, "Mesh link", st.MeshLink, st, true)
	if out := buf.String(); !strings.Contains(out, "batman only (eth1)") || strings.Count(out, "ESTAB") != 3 {
		t.Errorf("first link: its radio peers and the wired neighbour:\n%s", out)
	}

	// The second link only: one dual-band radio pinned to it.
	st.MeshLink = status.MeshLink{Index: 1, Transport: "none", Band: "2g", Channel: 6, Peers: []status.Peer{}}
	st.MoreLinks = []status.MeshLink{{Index: 2, Interface: "mesh1", Transport: "wifi", Band: "5g", Channel: 149, WidthMHz: 80, FreqMHz: 5745, Peers: peers}}
	for i := range st.Batman.Neighbors {
		st.Batman.Neighbors[i].Via = "mesh1"
	}
	buf.Reset()
	printMeshLink(&buf, "Mesh link", st.MeshLink, st, true)
	if out := buf.String(); !strings.Contains(out, "no radio here for this link: 2g ch 6") || strings.Contains(out, "DIRECT LINK") {
		t.Errorf("first link without a radio:\n%s", out)
	}
	buf.Reset()
	printMeshLink(&buf, "Mesh link 2", st.MoreLinks[0], st, false)
	if out := buf.String(); strings.Count(out, "ESTAB") != 3 || !strings.Contains(out, "5g ch 149") {
		t.Errorf("second link with the node's only radio:\n%s", out)
	}
}
