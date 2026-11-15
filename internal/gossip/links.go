package gossip

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
)

// Link queries (architecture §12.5). Gossip records carry no link state:
// at 50 nodes it would multiply their airtime. A node that needs the whole
// mesh graph (someone has its topology page open) asks instead: one small
// unicast query to each node, answered with that node's direct links.
// Both packets are authenticated like records, under the same key, and
// travel on the gossip port, which client ports filter.
const (
	QueryMagic = "TRQ1"
	LinksMagic = "TRL1"
	// MaxLinks bounds a reply so it stays one packet: the strongest links
	// are kept.
	MaxLinks = 12
)

// Query asks a node for its links.
type Query struct {
	V     int    `json:"v"`
	MAC   string `json:"mac"` // the asking node's identity MAC
	Nonce uint64 `json:"n"`   // echoed in the reply
}

// Links is a node's answer: its direct links and a few figures that are
// cheap to send along and that only it knows.
type Links struct {
	V     int    `json:"v"`
	MAC   string `json:"mac"` // the answering node's identity MAC
	Nonce uint64 `json:"n"`
	Links []Link `json:"l"`
	More  int    `json:"more,omitempty"` // links left out of the reply
	// Clients is the number of end-user devices on the node.
	Clients int `json:"eud"`
	// Selected is the node ID of the gateway its clients use; 0 for none.
	Selected int `json:"sel,omitempty"`
}

// Link is one direct link as the reporting node sees it.
type Link struct {
	Local  string `json:"a"`           // this node's interface MAC
	Remote string `json:"b"`           // the neighbour's interface MAC
	Kbps   uint32 `json:"t"`           // batman-adv's throughput estimate towards the neighbour
	Signal int    `json:"s,omitempty"` // dBm received from the neighbour; 0 when wired or unknown
	Wired  bool   `json:"w,omitempty"`
	// MeshLink is the mesh link the neighbour is heard on, numbered from 1
	// in the mesh profile's order, which every node of the mesh shares: the
	// asker names it by its band and channel. 0 when wired, or from a node
	// that does not send it (older releases ignore and omit the field).
	MeshLink int `json:"k,omitempty"`
}

// SealMessage encodes and authenticates a query or a reply.
func SealMessage(key []byte, magic string, v any) ([]byte, error) {
	return seal(key, magic, v, MaxPacket)
}

func seal(key []byte, magic string, v any, max int) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	pkt := make([]byte, 0, len(magic)+len(body)+MACLen)
	pkt = append(pkt, magic...)
	pkt = append(pkt, body...)
	m := hmac.New(sha256.New, key)
	m.Write(pkt)
	pkt = append(pkt, m.Sum(nil)[:MACLen]...)
	if len(pkt) > max {
		return nil, fmt.Errorf("gossip message is %d bytes, over the %d-byte cap", len(pkt), max)
	}
	return pkt, nil
}

// Kind returns the magic of a packet (Magic, QueryMagic, LinksMagic,
// OfferMagic, AckMagic, RequestMagic), or "" for anything else.
func Kind(pkt []byte) string {
	for _, m := range []string{Magic, QueryMagic, LinksMagic, OfferMagic, AckMagic, RequestMagic, SurveyQueryMagic, SurveyReplyMagic, RolloutMagic, RolloutAskMagic} {
		if bytes.HasPrefix(pkt, []byte(m)) {
			return m
		}
	}
	return ""
}

// OpenMessage verifies a packet with the given magic and decodes it into v.
func OpenMessage(key []byte, magic string, pkt []byte, v any) error {
	if len(pkt) < len(magic)+2+MACLen || !bytes.HasPrefix(pkt, []byte(magic)) {
		return ErrNotGossip
	}
	signed, tag := pkt[:len(pkt)-MACLen], pkt[len(pkt)-MACLen:]
	m := hmac.New(sha256.New, key)
	m.Write(signed)
	if !hmac.Equal(tag, m.Sum(nil)[:MACLen]) {
		return ErrAuth
	}
	if err := json.Unmarshal(signed[len(magic):], v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

// OpenQuery verifies and decodes a link query.
func OpenQuery(key, pkt []byte) (*Query, error) {
	var q Query
	if err := OpenMessage(key, QueryMagic, pkt, &q); err != nil {
		return nil, err
	}
	if !validMAC(q.MAC) {
		return nil, fmt.Errorf("%w: bad mac %q", ErrInvalid, q.MAC)
	}
	return &q, nil
}

// OpenLinks verifies and decodes a link reply. Like a record's fields,
// its contents reach an operator's screen, so they are checked.
func OpenLinks(key, pkt []byte) (*Links, error) {
	var l Links
	if err := OpenMessage(key, LinksMagic, pkt, &l); err != nil {
		return nil, err
	}
	if !validMAC(l.MAC) {
		return nil, fmt.Errorf("%w: bad mac %q", ErrInvalid, l.MAC)
	}
	if len(l.Links) > MaxLinks {
		return nil, fmt.Errorf("%w: %d links", ErrInvalid, len(l.Links))
	}
	for _, k := range l.Links {
		if !validMAC(k.Local) || !validMAC(k.Remote) {
			return nil, fmt.Errorf("%w: bad link address", ErrInvalid)
		}
		if k.Signal > 0 || k.Signal < -150 {
			return nil, fmt.Errorf("%w: signal %d dBm", ErrInvalid, k.Signal)
		}
	}
	if l.Clients < 0 || l.Clients > 100000 || l.Selected < 0 || l.Selected > 254 || l.More < 0 {
		return nil, fmt.Errorf("%w: figures out of range", ErrInvalid)
	}
	return &l, nil
}

func validMAC(s string) bool {
	m, err := net.ParseMAC(s)
	return err == nil && len(m) == 6
}

// SendTo sends one packet to a single node's address on the gossip port.
func (c *Conn) SendTo(pkt []byte, ip net.IP) error {
	_, err := c.pc.WriteToUDP(pkt, &net.UDPAddr{IP: ip, Port: c.group.Port})
	return err
}
