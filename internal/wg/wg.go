// Package wg configures and reads WireGuard interfaces through the kernel's
// generic netlink family, and makes and parses WireGuard keys.
package wg

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"

	"meridian/trellis/internal/gnl"
)

// FamilyName is the generic netlink family of the wireguard module.
const FamilyName = "wireguard"

const familyVersion = 1

// ErrNotLoaded means the wireguard module is not loaded.
var ErrNotLoaded = errors.New("the wireguard kernel module is not loaded")

// KeyLen is the length of a WireGuard key.
const KeyLen = 32

// Key is a WireGuard (Curve25519) key.
type Key [KeyLen]byte

// String is the base64 form wg(8) uses.
func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// IsZero says whether the key is all zeros (none set).
func (k Key) IsZero() bool { return k == Key{} }

// ParseKey reads a key in base64, only in the one form String writes, so
// that keys compared as text compare as keys.
func ParseKey(s string) (Key, error) {
	var k Key
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return k, fmt.Errorf("not base64: %w", err)
	}
	if len(b) != KeyLen {
		return k, fmt.Errorf("%d bytes, a WireGuard key has %d", len(b), KeyLen)
	}
	copy(k[:], b)
	return k, nil
}

// GeneratePrivateKey returns a new private key, clamped as wg genkey's are.
func GeneratePrivateKey() (Key, error) {
	var k Key
	p, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return k, err
	}
	copy(k[:], p.Bytes())
	return k.Clamped(), nil
}

// Clamped is the private key as the kernel keeps it (RFC 7748 §5): the
// same public key, and what reading a device gives back.
func (k Key) Clamped() Key {
	k[0] &= 248
	k[31] = k[31]&127 | 64
	return k
}

// PublicKey derives the public key of a private key.
func (k Key) PublicKey() (Key, error) {
	var pub Key
	p, err := ecdh.X25519().NewPrivateKey(k[:])
	if err != nil {
		return pub, err
	}
	copy(pub[:], p.PublicKey().Bytes())
	return pub, nil
}

// Device is a WireGuard interface's configuration and state.
type Device struct {
	Ifindex    int
	Name       string
	PrivateKey Key
	PublicKey  Key
	ListenPort int
	FWMark     uint32
	Peers      []Peer
}

// Peer is one peer of a device.
type Peer struct {
	PublicKey Key
	// Endpoint is where its packets go; invalid for none.
	Endpoint netip.AddrPort
	// Keepalive is the persistent keepalive interval; 0 is off.
	Keepalive time.Duration
	// AllowedIPs are what it may send from, and what is sent to it.
	AllowedIPs []netip.Prefix
	// Read only.
	LastHandshake time.Time
	RxBytes       uint64
	TxBytes       uint64
}

// Client is a connection to the wireguard family. It survives failed
// requests: see package gnl.
type Client struct {
	c *gnl.Conn
}

// Dial opens a connection to the wireguard family.
func Dial() (*Client, error) {
	c, err := gnl.Dial(FamilyName)
	if err != nil {
		if errors.Is(err, gnl.ErrFamilyNotFound) {
			return nil, ErrNotLoaded
		}
		return nil, err
	}
	return &Client{c: c}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.c.Close() }

// Device reads one WireGuard interface by name.
func (c *Client) Device(name string) (*Device, error) {
	ae := netlink.NewAttributeEncoder()
	ae.String(WGDEVICE_A_IFNAME, name)
	data, err := ae.Encode()
	if err != nil {
		return nil, err
	}
	msgs, err := c.c.Execute(WG_CMD_GET_DEVICE, familyVersion, netlink.Dump, data)
	if err != nil {
		return nil, fmt.Errorf("read WireGuard interface %s: %w", name, err)
	}
	return decodeDevice(msgs)
}

// decodeDevice merges a dump's messages: a device with many peers comes in
// several, and a peer with many allowed IPs may continue in the next one
// under the same public key.
func decodeDevice(msgs []genetlink.Message) (*Device, error) {
	d := &Device{}
	for _, m := range msgs {
		ad, err := netlink.NewAttributeDecoder(m.Data)
		if err != nil {
			return nil, err
		}
		for ad.Next() {
			switch ad.Type() {
			case WGDEVICE_A_IFINDEX:
				d.Ifindex = int(ad.Uint32())
			case WGDEVICE_A_IFNAME:
				d.Name = ad.String()
			case WGDEVICE_A_PRIVATE_KEY:
				copy(d.PrivateKey[:], ad.Bytes())
			case WGDEVICE_A_PUBLIC_KEY:
				copy(d.PublicKey[:], ad.Bytes())
			case WGDEVICE_A_LISTEN_PORT:
				d.ListenPort = int(ad.Uint16())
			case WGDEVICE_A_FWMARK:
				d.FWMark = ad.Uint32()
			case WGDEVICE_A_PEERS:
				ad.Nested(func(nd *netlink.AttributeDecoder) error {
					for nd.Next() {
						nd.Nested(func(pd *netlink.AttributeDecoder) error {
							p := decodePeer(pd)
							if n := len(d.Peers); n > 0 && d.Peers[n-1].PublicKey == p.PublicKey {
								d.Peers[n-1].AllowedIPs = append(d.Peers[n-1].AllowedIPs, p.AllowedIPs...)
							} else {
								d.Peers = append(d.Peers, p)
							}
							return nil
						})
					}
					return nil
				})
			}
		}
		if err := ad.Err(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func decodePeer(ad *netlink.AttributeDecoder) Peer {
	var p Peer
	for ad.Next() {
		switch ad.Type() {
		case WGPEER_A_PUBLIC_KEY:
			copy(p.PublicKey[:], ad.Bytes())
		case WGPEER_A_ENDPOINT:
			p.Endpoint = parseSockaddr(ad.Bytes())
		case WGPEER_A_PERSISTENT_KEEPALIVE_INTERVAL:
			p.Keepalive = time.Duration(ad.Uint16()) * time.Second
		case WGPEER_A_LAST_HANDSHAKE_TIME:
			if b := ad.Bytes(); len(b) >= 16 {
				sec := int64(binary.NativeEndian.Uint64(b[0:8]))
				nsec := int64(binary.NativeEndian.Uint64(b[8:16]))
				if sec != 0 || nsec != 0 {
					p.LastHandshake = time.Unix(sec, nsec)
				}
			}
		case WGPEER_A_RX_BYTES:
			p.RxBytes = ad.Uint64()
		case WGPEER_A_TX_BYTES:
			p.TxBytes = ad.Uint64()
		case WGPEER_A_ALLOWEDIPS:
			ad.Nested(func(nd *netlink.AttributeDecoder) error {
				for nd.Next() {
					nd.Nested(func(ad *netlink.AttributeDecoder) error {
						var fam uint16
						var ip []byte
						var bits int
						for ad.Next() {
							switch ad.Type() {
							case WGALLOWEDIP_A_FAMILY:
								fam = ad.Uint16()
							case WGALLOWEDIP_A_IPADDR:
								ip = ad.Bytes()
							case WGALLOWEDIP_A_CIDR_MASK:
								bits = int(ad.Uint8())
							}
						}
						if a, ok := netip.AddrFromSlice(ip); ok && (fam == afInet || fam == afInet6) {
							if fam == afInet {
								a = a.Unmap()
							}
							p.AllowedIPs = append(p.AllowedIPs, netip.PrefixFrom(a, bits))
						}
						return nil
					})
				}
				return nil
			})
		}
	}
	return p
}

const (
	afInet  = 2
	afInet6 = 10
)

// parseSockaddr reads a struct sockaddr_in or sockaddr_in6.
func parseSockaddr(b []byte) netip.AddrPort {
	if len(b) < 2 {
		return netip.AddrPort{}
	}
	fam := binary.NativeEndian.Uint16(b[0:2])
	switch {
	case fam == afInet && len(b) >= 8:
		a, _ := netip.AddrFromSlice(b[4:8])
		return netip.AddrPortFrom(a, binary.BigEndian.Uint16(b[2:4]))
	case fam == afInet6 && len(b) >= 24:
		a, _ := netip.AddrFromSlice(b[8:24])
		return netip.AddrPortFrom(a, binary.BigEndian.Uint16(b[2:4]))
	}
	return netip.AddrPort{}
}

// sockaddr writes a struct sockaddr_in or sockaddr_in6.
func sockaddr(ap netip.AddrPort) []byte {
	a := ap.Addr()
	if a.Is4() || a.Is4In6() {
		b := make([]byte, 16)
		binary.NativeEndian.PutUint16(b[0:2], afInet)
		binary.BigEndian.PutUint16(b[2:4], ap.Port())
		v4 := a.Unmap().As4()
		copy(b[4:8], v4[:])
		return b
	}
	b := make([]byte, 28)
	binary.NativeEndian.PutUint16(b[0:2], afInet6)
	binary.BigEndian.PutUint16(b[2:4], ap.Port())
	v6 := a.As16()
	copy(b[8:24], v6[:])
	return b
}

// Config is what Configure sets on a device. Peers not listed are left as
// they are.
type Config struct {
	PrivateKey Key
	ListenPort int
	FWMark     uint32
	Peers      []PeerConfig
}

// PeerConfig adds, changes or removes one peer.
type PeerConfig struct {
	PublicKey Key
	// Remove takes the peer away; nothing else is used.
	Remove bool
	// Endpoint is set when valid; otherwise the peer's stays as it is
	// (WireGuard follows a peer that moves).
	Endpoint netip.AddrPort
	// Keepalive is the persistent keepalive interval; 0 turns it off.
	Keepalive time.Duration
	// AllowedIPs replace the peer's.
	AllowedIPs []netip.Prefix
}

// Configure sets a device's key, port and mark, and the peers listed. The
// kernel keeps what is unchanged: the same key, port or peer leaves the
// sessions as they are.
func (c *Client) Configure(name string, cfg Config) error {
	ae := netlink.NewAttributeEncoder()
	ae.String(WGDEVICE_A_IFNAME, name)
	ae.Bytes(WGDEVICE_A_PRIVATE_KEY, cfg.PrivateKey[:])
	ae.Uint16(WGDEVICE_A_LISTEN_PORT, uint16(cfg.ListenPort))
	ae.Uint32(WGDEVICE_A_FWMARK, cfg.FWMark)
	if len(cfg.Peers) > 0 {
		ae.Nested(WGDEVICE_A_PEERS, func(pe *netlink.AttributeEncoder) error {
			for i, p := range cfg.Peers {
				pe.Nested(uint16(i), func(ae *netlink.AttributeEncoder) error {
					encodePeer(ae, p)
					return nil
				})
			}
			return nil
		})
	}
	data, err := ae.Encode()
	if err != nil {
		return err
	}
	if _, err := c.c.Execute(WG_CMD_SET_DEVICE, familyVersion, netlink.Acknowledge, data); err != nil {
		return fmt.Errorf("configure WireGuard interface %s: %w", name, err)
	}
	return nil
}

func encodePeer(ae *netlink.AttributeEncoder, p PeerConfig) {
	ae.Bytes(WGPEER_A_PUBLIC_KEY, p.PublicKey[:])
	if p.Remove {
		ae.Uint32(WGPEER_A_FLAGS, WGPEER_F_REMOVE_ME)
		return
	}
	ae.Uint32(WGPEER_A_FLAGS, WGPEER_F_REPLACE_ALLOWEDIPS)
	if p.Endpoint.IsValid() {
		ae.Bytes(WGPEER_A_ENDPOINT, sockaddr(p.Endpoint))
	}
	ae.Uint16(WGPEER_A_PERSISTENT_KEEPALIVE_INTERVAL, uint16(p.Keepalive/time.Second))
	ae.Nested(WGPEER_A_ALLOWEDIPS, func(le *netlink.AttributeEncoder) error {
		for i, pf := range p.AllowedIPs {
			le.Nested(uint16(i), func(ae *netlink.AttributeEncoder) error {
				a := pf.Addr()
				if a.Is4() {
					ae.Uint16(WGALLOWEDIP_A_FAMILY, afInet)
					v := a.As4()
					ae.Bytes(WGALLOWEDIP_A_IPADDR, v[:])
				} else {
					ae.Uint16(WGALLOWEDIP_A_FAMILY, afInet6)
					v := a.As16()
					ae.Bytes(WGALLOWEDIP_A_IPADDR, v[:])
				}
				ae.Uint8(WGALLOWEDIP_A_CIDR_MASK, uint8(pf.Bits()))
				return nil
			})
		}
		return nil
	})
}
