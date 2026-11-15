package gossip

import (
	"crypto/hmac"
	"crypto/sha256"
	"strings"
	"testing"
)

// Gossip arrives from the air. Authentication proves only that a mesh
// member sent it; a buggy or compromised member can send anything under
// the key. These fuzz the decoding behind the authentication: the body is
// signed with the test key, so every input reaches the parser.

var fuzzKey = func() []byte { k, _ := Key("fuzz passphrase", "fuzz-mesh"); return k }()

func signed(magic string, body []byte) []byte {
	pkt := append([]byte(magic), body...)
	m := hmac.New(sha256.New, fuzzKey)
	m.Write(pkt)
	return append(pkt, m.Sum(nil)[:MACLen]...)
}

func FuzzOpenRecord(f *testing.F) {
	good := &Record{V: 1, MAC: "a0:b0:c0:00:00:01", ID: 7, Callsign: "relay-1", Addr: "10.73.7.1", Boot: 42, Seq: 3, Version: "0.3.1"}
	if pkt, err := Seal(fuzzKey, good); err == nil {
		f.Add(pkt[len(Magic) : len(pkt)-MACLen])
	}
	f.Add([]byte(`{"mac":"a0:b0:c0:00:00:01","boot":1,"cs":"x\ny"}`))
	f.Add([]byte(`{"mac":"a0:b0:c0:00:00:01","boot":1,"id":300,"addr":"::1"}`))
	f.Add([]byte(`{"mac":"a0:b0:c0:00:00:01","boot":1,"lk":["5g/149/80","s1g/26/2"],"ps":[1e308,-1e308]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		r, err := Open(fuzzKey, signed(Magic, body))
		if err != nil {
			return
		}
		// What validation promises the rest of meshd.
		for _, s := range []string{r.Callsign, r.Addr, r.Version, r.IDSource} {
			if strings.ContainsAny(s, "\n\r\x00") {
				t.Fatalf("a control character got through: %q", s)
			}
		}
		if r.ID < 0 || r.ID > 254 {
			t.Fatalf("node ID %d got through", r.ID)
		}
		// What was accepted survives sealing again.
		pkt, err := Seal(fuzzKey, r)
		if err != nil {
			return // too large to send: fine, it was only received
		}
		if _, err := Open(fuzzKey, pkt); err != nil {
			t.Fatalf("an accepted record does not open again: %v", err)
		}
	})
}

// Raw packets, signature and all: nothing may panic before the check.
func FuzzOpenRaw(f *testing.F) {
	f.Add([]byte(Magic))
	f.Add(signed(Magic, []byte("{}")))
	f.Add(signed(OfferMagic, []byte(`{"rev":1}`)))
	f.Fuzz(func(t *testing.T, pkt []byte) {
		_, _ = Open(fuzzKey, pkt)
		_, _ = OpenOffer(fuzzKey, pkt)
		_, _ = OpenAck(fuzzKey, pkt)
		_, _ = OpenRequest(fuzzKey, pkt)
		_, _ = OpenQuery(fuzzKey, pkt)
		_, _ = OpenLinks(fuzzKey, pkt)
		_, _ = OpenSurveyQuery(fuzzKey, pkt)
		_, _ = OpenSurveyReply(fuzzKey, pkt)
		_ = Kind(pkt)
	})
}

// The other messages, past the authentication.
func FuzzOpenMessages(f *testing.F) {
	f.Add(byte(0), []byte(`{"v":1,"mac":"a0:b0:c0:00:00:01","cs":"n1","rev":2,"p":"name: x"}`))
	f.Add(byte(1), []byte(`{"mac":"a0:b0:c0:00:00:01","rev":2}`))
	f.Add(byte(5), []byte(`{"nodes":[{"links":[{"channels":[{"ch":-1,"busy":1e9}]}]}]}`))
	f.Add(byte(7), []byte(`{"v":1,"mac":"a0:b0:c0:00:00:01","cs":"n1","r":{"id":"ab"}}`))
	f.Add(byte(8), []byte(`{"v":1,"mac":"a0:b0:c0:00:00:01","id":"ab"}`))
	magics := []string{OfferMagic, AckMagic, RequestMagic, QueryMagic, LinksMagic, SurveyQueryMagic, SurveyReplyMagic, RolloutMagic, RolloutAskMagic}
	f.Fuzz(func(t *testing.T, which byte, body []byte) {
		magic := magics[int(which)%len(magics)]
		pkt := signed(magic, body)
		switch magic {
		case OfferMagic:
			if o, err := OpenOffer(fuzzKey, pkt); err == nil {
				if !validMAC(o.MAC) || o.Rev == 0 || o.Profile == "" || o.ApplyAtMS < 0 {
					t.Fatalf("an invalid offer got through: %+v", o)
				}
				if o.Callsign != "" && !reCallsign.MatchString(o.Callsign) {
					t.Fatalf("callsign %q got through", o.Callsign)
				}
			}
		case AckMagic:
			_, _ = OpenAck(fuzzKey, pkt)
		case RequestMagic:
			_, _ = OpenRequest(fuzzKey, pkt)
		case QueryMagic:
			_, _ = OpenQuery(fuzzKey, pkt)
		case LinksMagic:
			_, _ = OpenLinks(fuzzKey, pkt)
		case SurveyQueryMagic:
			_, _ = OpenSurveyQuery(fuzzKey, pkt)
		case SurveyReplyMagic:
			_, _ = OpenSurveyReply(fuzzKey, pkt)
		case RolloutMagic:
			if m, err := OpenRollout(fuzzKey, pkt); err == nil && (!validMAC(m.MAC) || len(m.Rollout) == 0) {
				t.Fatalf("an invalid rollout message got through: %+v", m)
			}
		case RolloutAskMagic:
			if a, err := OpenRolloutAsk(fuzzKey, pkt); err == nil && (!validMAC(a.MAC) || a.ID == "") {
				t.Fatalf("an invalid rollout request got through: %+v", a)
			}
		}
	})
}

func FuzzParseLinkEntry(f *testing.F) {
	for _, s := range []string{"5g/149/80", "s1g/26/2", "2g/6/20", "//", "5g/-1/0", "x/99999999999999999999/1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, e string) {
		band, ch, w, ok := ParseLinkEntry(e)
		if ok && (band == "" || ch < 1 || w < 1) {
			t.Fatalf("%q parsed as %q/%d/%d", e, band, ch, w)
		}
	})
}
