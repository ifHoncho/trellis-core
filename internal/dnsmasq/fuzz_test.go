package dnsmasq

import (
	"strings"
	"testing"
)

// The lease file holds what clients sent: their hostnames reach the web UI
// and the mesh's name service.
func FuzzParseLeases(f *testing.F) {
	f.Add("1800000000 aa:bb:cc:dd:ee:ff 10.73.2.171 SM-G981U1 01:aa:bb:cc:dd:ee:ff\n")
	f.Add("x y z\n99999999999999999999 a b c d\n")
	f.Fuzz(func(t *testing.T, s string) {
		for _, l := range ParseLeases(strings.NewReader(s)) {
			if strings.ContainsAny(l.Hostname+l.MAC+l.IP, " \t\n") {
				t.Fatalf("whitespace in a parsed lease: %+v", l)
			}
		}
	})
}
