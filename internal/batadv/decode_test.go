package batadv

import (
	"testing"

	"github.com/mdlayher/netlink"
)

func decoder(t *testing.T, f func(ae *netlink.AttributeEncoder)) *netlink.AttributeDecoder {
	t.Helper()
	ae := netlink.NewAttributeEncoder()
	f(ae)
	b, err := ae.Encode()
	if err != nil {
		t.Fatal(err)
	}
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

var peer = []byte{0xa0, 0xb0, 0xc0, 0x00, 0x02, 0x26}

// Values from a real link: driver expected
// throughput 49.895 Mbit/s; batman-adv's internal estimate ~475 (100 kbit/s
// units), exported by the neighbour dump as 47500 (kbit/s). batctl showed
// about 50 Mbit/s; meshctl showed 4750 Mbit/s before this fix.
func TestNeighborThroughputIsKbps(t *testing.T) {
	n := decodeNeighbor(decoder(t, func(ae *netlink.AttributeEncoder) {
		ae.Bytes(BATADV_ATTR_NEIGH_ADDRESS, peer)
		ae.Uint32(BATADV_ATTR_THROUGHPUT, 47500)
	}))
	if n.Throughput.Mbps() != 47.5 {
		t.Errorf("neighbor throughput = %v Mbit/s, want 47.5", n.Throughput.Mbps())
	}
}

func TestOriginatorThroughputIsKbps(t *testing.T) {
	o := decodeOriginator(decoder(t, func(ae *netlink.AttributeEncoder) {
		ae.Bytes(BATADV_ATTR_ORIG_ADDRESS, peer)
		ae.Uint32(BATADV_ATTR_THROUGHPUT, 46500)
		ae.Flag(BATADV_ATTR_FLAG_BEST, true)
	}))
	if o.Throughput.Mbps() != 46.5 || !o.Best {
		t.Errorf("originator = %+v (%v Mbit/s)", o, o.Throughput.Mbps())
	}
}

func TestGatewayThroughputIsHundredKbps(t *testing.T) {
	g := decodeGateway(decoder(t, func(ae *netlink.AttributeEncoder) {
		ae.Bytes(BATADV_ATTR_ORIG_ADDRESS, peer)
		ae.Uint32(BATADV_ATTR_THROUGHPUT, 465) // gateway dump: 100 kbit/s units
		ae.Uint32(BATADV_ATTR_BANDWIDTH_DOWN, 500)
	}))
	if g.Throughput.Mbps() != 46.5 || g.BandwidthDown != 500 {
		t.Errorf("gateway = %+v (%v Mbit/s)", g, g.Throughput.Mbps())
	}
}
