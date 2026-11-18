package gossip

import "fmt"

// Channel surveys. A node asked for a mesh-wide survey
// sends every node a query; each scans the bands of its mesh links once and
// answers with what it found per channel. Authenticated like records, under
// the same key.
const (
	SurveyQueryMagic = "TRS1"
	SurveyReplyMagic = "TRV1"
)

// SurveyQuery asks a node to survey its links' bands.
type SurveyQuery struct {
	V     int    `json:"v"`
	MAC   string `json:"mac"`
	Nonce uint64 `json:"n"`
}

// SurveyReply is a node's survey.
type SurveyReply struct {
	V        int          `json:"v"`
	MAC      string       `json:"mac"`
	Callsign string       `json:"cs"`
	Nonce    uint64       `json:"n"`
	Links    []SurveyLink `json:"l"`
	Err      string       `json:"err,omitempty"`
}

// SurveyLink is one mesh link's band as a node surveyed it.
type SurveyLink struct {
	Link     int        `json:"k"` // mesh link, from 1 in the profile's order
	Band     string     `json:"b"`
	Channels []SurveyCh `json:"c"`
	Err      string     `json:"err,omitempty"`
}

// SurveyCh is one channel: how busy, how noisy, and what else is there.
type SurveyCh struct {
	Ch int `json:"c"`
	// Busy is the share of time the channel was busy, in percent; -1 when
	// the driver does not measure it (ath9k_htc, rt2800usb).
	Busy int `json:"b"`
	// Noise in dBm; 0 when unknown.
	Noise int `json:"n,omitempty"`
	// Nets is how many other networks were heard there, and Strongest the
	// strongest of them in dBm (0 with none).
	Nets      int `json:"w,omitempty"`
	Strongest int `json:"s,omitempty"`
}

// MaxSurveyChannels bounds a link's channels in a reply.
const MaxSurveyChannels = 64

// SealSurveyReply encodes and authenticates a reply, which may be larger
// than a record (it is unicast).
func SealSurveyReply(key []byte, r *SurveyReply) ([]byte, error) {
	return seal(key, SurveyReplyMagic, r, MaxOfferPacket)
}

// OpenSurveyQuery verifies and decodes a query.
func OpenSurveyQuery(key, pkt []byte) (*SurveyQuery, error) {
	var q SurveyQuery
	if err := OpenMessage(key, SurveyQueryMagic, pkt, &q); err != nil {
		return nil, err
	}
	if !validMAC(q.MAC) {
		return nil, fmt.Errorf("%w: bad mac %q", ErrInvalid, q.MAC)
	}
	q.MAC = canonMAC(q.MAC)
	return &q, nil
}

// OpenSurveyReply verifies and decodes a reply; its contents reach an
// operator's screen, so they are checked.
func OpenSurveyReply(key, pkt []byte) (*SurveyReply, error) {
	var r SurveyReply
	if err := OpenMessage(key, SurveyReplyMagic, pkt, &r); err != nil {
		return nil, err
	}
	switch {
	case !validMAC(r.MAC):
		return nil, fmt.Errorf("%w: bad mac %q", ErrInvalid, r.MAC)
	case r.Callsign != "" && !reCallsign.MatchString(r.Callsign):
		return nil, fmt.Errorf("%w: callsign %q", ErrInvalid, r.Callsign)
	case !plainText(r.Err, 300) || len(r.Links) > MaxLinkEntries:
		return nil, fmt.Errorf("%w: bad survey", ErrInvalid)
	}
	for _, l := range r.Links {
		if !printable(l.Band, 8) || !plainText(l.Err, 300) || len(l.Channels) > MaxSurveyChannels {
			return nil, fmt.Errorf("%w: bad survey link", ErrInvalid)
		}
		for _, c := range l.Channels {
			if c.Busy < -1 || c.Busy > 100 || c.Ch <= 0 || c.Nets < 0 {
				return nil, fmt.Errorf("%w: bad survey channel", ErrInvalid)
			}
		}
	}
	r.MAC = canonMAC(r.MAC)
	return &r, nil
}
