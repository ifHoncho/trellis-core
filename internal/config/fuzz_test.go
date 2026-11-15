package config

import (
	"strings"
	"testing"
)

// Configuration arrives from files, the web UI (JSON) and other people's
// QR codes (profile codes). None may panic the parser, and what is
// accepted must be valid and survive being written out again.

func FuzzParseProfile(f *testing.F) {
	f.Add([]byte("name: alpha\nmesh_id: trellis-alpha\nsae_passphrase: k3m9w2-xp7fhq-b5tn8r\nradio: {band: 5g, channel: 149, width: 80}\n"))
	f.Add([]byte("links: [{band: s1g, channel: 26, width: 2}]\n"))
	f.Add([]byte("{{{"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseProfile(data)
		if err != nil {
			return
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("ParseProfile accepted an invalid profile: %v", err)
		}
		code, err := p.Code()
		if err != nil {
			return
		}
		if _, err := DecodeProfileCode(code); err != nil {
			t.Fatalf("an accepted profile's code does not decode: %v", err)
		}
	})
}

func FuzzDecodeProfileCode(f *testing.F) {
	p := Profile{Name: "alpha", MeshID: "trellis-alpha", SAEPassphrase: "k3m9w2-xp7fhq-b5tn8r-dj4cvy",
		Regulatory: Regulatory{Mode: RegModeCountry, Country: "US"}, Radio: RadioPlan{Band: Band5G, Channel: 149, Width: 80}}
	p.ApplyDefaults()
	if code, err := p.Code(); err == nil {
		f.Add(code)
	}
	f.Add("TRP1:")
	f.Add("TRP1:!!!!")
	f.Fuzz(func(t *testing.T, code string) {
		got, err := DecodeProfileCode(code)
		if err != nil {
			return
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("a profile code decoded to an invalid profile: %v", err)
		}
	})
}

func FuzzParseNode(f *testing.F) {
	f.Add([]byte("schema: 0\ncallsign: relay-1\nnode_id: 12\n"), false)
	f.Add([]byte(`{"callsign":"relay-1","ports":[{"name":"eth1","role":"auto"}]}`), true)
	f.Fuzz(func(t *testing.T, data []byte, asJSON bool) {
		var n *Node
		var err error
		if asJSON {
			n, err = ParseNodeJSON(data)
		} else {
			n, err = ParseNode(data)
		}
		if err != nil {
			return
		}
		if err := n.Validate(); err != nil {
			t.Fatalf("an accepted node configuration is invalid: %v", err)
		}
		if strings.ContainsAny(n.Callsign, "\n\r") {
			t.Fatalf("callsign %q accepted", n.Callsign)
		}
	})
}
