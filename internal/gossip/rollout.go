package gossip

import (
	"encoding/json"
	"fmt"
)

// Rolling mesh-wide upgrades (internal/rollout) travel as two messages,
// unicast on the gossip port and authenticated under the gossip key like a
// profile offer:
//
//   - a rollout, from the node the operator gave the release to, to every
//     node it knows (and again, with Cancelled set, to call it off);
//   - a request for one, from a node that sees a rollout in another node's
//     gossip record and does not have it (it was away when it was sent).
const (
	RolloutMagic    = "TRU1"
	RolloutAskMagic = "TRA1"
)

// RolloutMsg carries a rollout (internal/rollout.Rollout, as JSON: this
// package does not interpret it).
type RolloutMsg struct {
	V        int             `json:"v"`
	MAC      string          `json:"mac"` // the sending node's identity MAC
	Callsign string          `json:"cs"`
	Rollout  json.RawMessage `json:"r"`
}

// RolloutAsk asks a node for the rollout it names in gossip.
type RolloutAsk struct {
	V   int    `json:"v"`
	MAC string `json:"mac"`
	ID  string `json:"id"`
}

// SealRollout signs a rollout message.
func SealRollout(key []byte, m *RolloutMsg) ([]byte, error) {
	return seal(key, RolloutMagic, m, MaxOfferPacket)
}

// OpenRollout verifies and decodes a rollout message.
func OpenRollout(key, pkt []byte) (*RolloutMsg, error) {
	var m RolloutMsg
	if err := OpenMessage(key, RolloutMagic, pkt, &m); err != nil {
		return nil, err
	}
	switch {
	case !validMAC(m.MAC):
		return nil, fmt.Errorf("%w: bad mac %q", ErrInvalid, m.MAC)
	case m.Callsign != "" && !reCallsign.MatchString(m.Callsign):
		return nil, fmt.Errorf("%w: callsign %q", ErrInvalid, m.Callsign)
	case len(m.Rollout) == 0:
		return nil, fmt.Errorf("%w: rollout message without a rollout", ErrInvalid)
	}
	m.MAC = canonMAC(m.MAC)
	return &m, nil
}

// SealRolloutAsk signs a request for a rollout.
func SealRolloutAsk(key []byte, a *RolloutAsk) ([]byte, error) {
	return seal(key, RolloutAskMagic, a, MaxPacket)
}

// OpenRolloutAsk verifies and decodes a request for a rollout.
func OpenRolloutAsk(key, pkt []byte) (*RolloutAsk, error) {
	var a RolloutAsk
	if err := OpenMessage(key, RolloutAskMagic, pkt, &a); err != nil {
		return nil, err
	}
	if !validMAC(a.MAC) || a.ID == "" || !printable(a.ID, 32) {
		return nil, fmt.Errorf("%w: bad rollout request", ErrInvalid)
	}
	a.MAC = canonMAC(a.MAC)
	return &a, nil
}
