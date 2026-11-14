package gossip

import (
	"errors"
	"strings"
	"testing"
)

func TestProfileMessages(t *testing.T) {
	k, _ := Key("passphrase-1234", "trellis-alpha")
	other, _ := Key("another-passphrase", "trellis-alpha")
	// A profile with ten links is larger than a record's packet.
	profile := "schema: 0\nname: alpha\nmesh_id: trellis-alpha\n" + strings.Repeat("    - {band: 5g, channel: 149, width: 80}\n", 40)
	o := &Offer{V: 1, MAC: "02:00:00:00:00:01", Callsign: "relay-1", Rev: 7, ApplyAtMS: 1794645660000, Profile: profile}
	pkt, err := SealOffer(k, o)
	if err != nil || len(pkt) <= MaxPacket {
		t.Fatalf("offer of %d bytes: %v", len(pkt), err)
	}
	if Kind(pkt) != OfferMagic {
		t.Errorf("kind %q", Kind(pkt))
	}
	got, err := OpenOffer(k, pkt)
	if err != nil || got.Rev != 7 || got.Profile != profile || got.ApplyAtMS != o.ApplyAtMS || got.Callsign != "relay-1" {
		t.Fatalf("offer round trip: %+v %v", got, err)
	}
	// Only a node with the mesh secret can offer a profile.
	if _, err := OpenOffer(other, pkt); !errors.Is(err, ErrAuth) {
		t.Errorf("offer under another key: %v", err)
	}
	for name, bad := range map[string]*Offer{
		"no revision": {V: 1, MAC: "02:00:00:00:00:01", Profile: "x"},
		"no profile":  {V: 1, MAC: "02:00:00:00:00:01", Rev: 2},
		"bad mac":     {V: 1, MAC: "nope", Rev: 2, Profile: "x"},
		"bad name":    {V: 1, MAC: "02:00:00:00:00:01", Callsign: "a b", Rev: 2, Profile: "x"},
	} {
		pkt, _ := SealOffer(k, bad)
		if _, err := OpenOffer(k, pkt); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if _, err := SealOffer(k, &Offer{V: 1, MAC: "02:00:00:00:00:01", Rev: 1, Profile: strings.Repeat("x", MaxOfferPacket)}); err == nil {
		t.Error("an oversized offer was sealed")
	}

	ack, _ := SealMessage(k, AckMagic, &Ack{V: 1, MAC: "02:00:00:00:00:02", Rev: 7, Err: TrimErr("line\none" + strings.Repeat("!", 400))})
	if a, err := OpenAck(k, ack); err != nil || a.Rev != 7 || len(a.Err) != 300 || strings.Contains(a.Err, "\n") {
		t.Errorf("ack: %+v %v", a, err)
	}
	req, _ := SealMessage(k, RequestMagic, &Request{V: 1, MAC: "02:00:00:00:00:03", Rev: 7})
	if r, err := OpenRequest(k, req); err != nil || r.Rev != 7 || Kind(req) != RequestMagic {
		t.Errorf("request: %+v %v", r, err)
	}
	if _, err := OpenRequest(k, ack); !errors.Is(err, ErrNotGossip) {
		t.Errorf("an ack opened as a request: %v", err)
	}
}
