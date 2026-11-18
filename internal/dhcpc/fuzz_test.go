package dhcpc

import (
	"net"
	"testing"
	"time"
)

// DHCP messages come from whatever is on the other end of a cable: Parse,
// the frame decoder and the lease built from an answer must never panic,
// and a lease must stay within the bounds leaseFrom promises.
func FuzzParse(f *testing.F) {
	c := &Client{MAC: clientMAC, Hostname: "node-3", VendorClass: "trellis"}
	ack := c.base(Ack, 1)
	ack.YIAddr = net.IPv4(192, 168, 1, 50).To4()
	ack.Options[optSubnetMask] = []byte{255, 255, 255, 0}
	ack.Options[optRouter] = []byte{192, 168, 1, 1}
	ack.Options[optDNS] = []byte{1, 1, 1, 1, 8, 8, 8, 8}
	ack.Options[optLeaseTime] = leaseSeconds(time.Hour)
	f.Add(ack.Marshal())
	f.Add(make([]byte, 240))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Parse(b)
		if err != nil {
			return
		}
		_ = p.MsgType()
		_ = p.ServerID()
		l, err := leaseFrom(p, clientMAC, time.Unix(1e9, 0))
		if err != nil {
			return
		}
		if ones, bits := l.Addr.Mask.Size(); bits != 32 || ones < minPrefixLen {
			t.Fatalf("lease with mask %v", l.Addr.Mask)
		}
		if l.Duration < minLease || l.T1 <= 0 || l.T1 >= l.Duration || l.T2 <= l.T1 || l.T2 >= l.Duration {
			t.Fatalf("lease times %v %v %v", l.Duration, l.T1, l.T2)
		}
	})
}

func FuzzUDPPayload(f *testing.F) {
	f.Add(udp4(net.IPv4(10, 0, 0, 1), net.IPv4bcast, 67, 68, []byte("payload")))
	f.Add([]byte{0x45})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = udpPayload(b)
	})
}
