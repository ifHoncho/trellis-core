package wg

import (
	"encoding/hex"
	"net/netip"
	"testing"
	"time"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
)

func TestKeys(t *testing.T) {
	// RFC 7748 §6.1, Alice.
	var priv Key
	b, _ := hex.DecodeString("77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	copy(priv[:], b)
	pub, err := priv.PublicKey()
	if err != nil || hex.EncodeToString(pub[:]) != "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a" {
		t.Fatalf("public key %x, %v", pub, err)
	}
	back, err := ParseKey(pub.String())
	if err != nil || back != pub {
		t.Fatalf("round trip: %v %v", back, err)
	}
	for _, bad := range []string{"", "not base64!", "AAAA"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
	k1, err1 := GeneratePrivateKey()
	k2, err2 := GeneratePrivateKey()
	if err1 != nil || err2 != nil || k1 == k2 || k1.IsZero() || k1.Clamped() != k1 {
		t.Fatal("generated keys")
	}
	// Clamping keeps the public key.
	if c, _ := priv.Clamped().PublicKey(); c != pub || priv.Clamped() == priv {
		t.Error("clamped key")
	}
}

func TestSockaddr(t *testing.T) {
	for _, s := range []string{"192.0.2.1:51873", "[2001:db8::7]:443"} {
		ap := netip.MustParseAddrPort(s)
		if got := parseSockaddr(sockaddr(ap)); got != ap {
			t.Errorf("%s: got %s", s, got)
		}
	}
	if parseSockaddr(nil).IsValid() || parseSockaddr([]byte{1, 2, 3}).IsValid() {
		t.Error("short sockaddr parsed")
	}
}

// A dump that splits one peer's allowed IPs over two messages is merged.
func TestDecodeDeviceMerges(t *testing.T) {
	var pk Key
	pk[0] = 7
	msg := func(first bool, ips ...string) genetlink.Message {
		ae := netlink.NewAttributeEncoder()
		ae.Uint32(WGDEVICE_A_IFINDEX, 5)
		ae.String(WGDEVICE_A_IFNAME, "site0")
		if first {
			ae.Uint16(WGDEVICE_A_LISTEN_PORT, 51873)
		}
		ae.Nested(WGDEVICE_A_PEERS, func(pe *netlink.AttributeEncoder) error {
			pe.Nested(0, func(ae *netlink.AttributeEncoder) error {
				p := PeerConfig{PublicKey: pk, Keepalive: 25 * time.Second}
				for _, s := range ips {
					p.AllowedIPs = append(p.AllowedIPs, netip.MustParsePrefix(s))
				}
				if first {
					p.Endpoint = netip.MustParseAddrPort("192.0.2.9:1")
				}
				encodePeer(ae, p)
				return nil
			})
			return nil
		})
		b, err := ae.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return genetlink.Message{Data: b}
	}
	d, err := decodeDevice([]genetlink.Message{msg(true, "169.254.1.2/32"), msg(false, "10.0.0.0/8", "2001:db8::/32")})
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "site0" || d.ListenPort != 51873 || len(d.Peers) != 1 {
		t.Fatalf("device %+v", d)
	}
	p := d.Peers[0]
	if p.PublicKey != pk || p.Endpoint.String() != "192.0.2.9:1" || p.Keepalive != 25*time.Second || len(p.AllowedIPs) != 3 ||
		p.AllowedIPs[0].String() != "169.254.1.2/32" || p.AllowedIPs[2].String() != "2001:db8::/32" {
		t.Fatalf("peer %+v", p)
	}
}
