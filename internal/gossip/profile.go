package gossip

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Mesh-wide profile changes (meshd meshchange.go). One node offers a new
// profile to every other node, unicast on the gossip port, with the moment
// they all switch to it; each answers with an acknowledgement. A node that
// sees, in gossip, another node on a higher revision asks it for that
// profile. All three are authenticated like records, under the same key:
// only a node that holds the mesh secret can offer a profile.
const (
	OfferMagic   = "TRO1"
	AckMagic     = "TRK1"
	RequestMagic = "TRR1"
	// MaxOfferPacket bounds an offer: a profile with many links does not
	// fit one gossip record's packet, and an offer is unicast, so IP
	// fragments it where it must.
	MaxOfferPacket = 8192
)

// Offer is a profile sent to every node.
type Offer struct {
	V        int    `json:"v"`
	MAC      string `json:"mac"` // the offering node's identity MAC
	Callsign string `json:"cs"`
	Rev      uint64 `json:"rev"`
	// ApplyAtMS is when every node switches to it, in Unix milliseconds
	// (the nodes keep one time); 0: at once (an answer to a
	// Request, for a change that has happened already).
	ApplyAtMS int64 `json:"at,omitempty"`
	// InMS is the same moment as a delay from when the offer was sent, in
	// milliseconds; 0 when not given (older versions). A node that has it
	// goes by it: it holds whatever the two nodes' clocks say.
	InMS int64 `json:"in,omitempty"`
	// Profile is the profile's YAML, the mesh secret included: offers
	// travel inside the mesh, on its encrypted links.
	Profile string `json:"p"`
}

// Ack answers an offer.
type Ack struct {
	V   int    `json:"v"`
	MAC string `json:"mac"`
	Rev uint64 `json:"rev"`
	OK  bool   `json:"ok"`
	// Err says why a node refused an offer.
	Err string `json:"err,omitempty"`
}

// Request asks a node for the profile of a revision it runs.
type Request struct {
	V   int    `json:"v"`
	MAC string `json:"mac"`
	Rev uint64 `json:"rev"`
}

// SealOffer encodes and authenticates an offer, which may be larger than a
// record.
func SealOffer(key []byte, o *Offer) ([]byte, error) {
	return seal(key, OfferMagic, o, MaxOfferPacket)
}

// OpenOffer verifies and decodes an offer.
func OpenOffer(key, pkt []byte) (*Offer, error) {
	var o Offer
	if err := OpenMessage(key, OfferMagic, pkt, &o); err != nil {
		return nil, err
	}
	switch {
	case !validMAC(o.MAC):
		return nil, fmt.Errorf("%w: bad mac %q", ErrInvalid, o.MAC)
	case o.Rev == 0 || o.Profile == "" || o.ApplyAtMS < 0 || o.InMS < 0:
		return nil, fmt.Errorf("%w: offer without revision or profile", ErrInvalid)
	case o.Callsign != "" && !reCallsign.MatchString(o.Callsign):
		return nil, fmt.Errorf("%w: callsign %q", ErrInvalid, o.Callsign)
	}
	return &o, nil
}

// OpenAck verifies and decodes an acknowledgement.
func OpenAck(key, pkt []byte) (*Ack, error) {
	var a Ack
	if err := OpenMessage(key, AckMagic, pkt, &a); err != nil {
		return nil, err
	}
	if !validMAC(a.MAC) || a.Rev == 0 || !plainText(a.Err, 300) {
		return nil, fmt.Errorf("%w: bad acknowledgement", ErrInvalid)
	}
	return &a, nil
}

// OpenRequest verifies and decodes a request.
func OpenRequest(key, pkt []byte) (*Request, error) {
	var r Request
	if err := OpenMessage(key, RequestMagic, pkt, &r); err != nil {
		return nil, err
	}
	if !validMAC(r.MAC) || r.Rev == 0 {
		return nil, fmt.Errorf("%w: bad request", ErrInvalid)
	}
	return &r, nil
}

// TrimErr keeps a refusal's reason short and on one line for an Ack.
func TrimErr(s string) string {
	s = strings.ToValidUTF8(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s), "?")
	if len(s) <= 300 {
		return s
	}
	cut := 297
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// plainText reports whether s is at most max bytes of valid UTF-8 without
// control characters: a sentence that may reach an operator's screen.
func plainText(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
