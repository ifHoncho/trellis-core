package wpa

import "testing"

// wpa_supplicant's events carry peers' addresses and, from a hostile peer,
// whatever it managed to put in a frame.
func FuzzParseEvent(f *testing.F) {
	for _, s := range []string{"<3>MESH-PEER-CONNECTED aa:bb:cc:dd:ee:ff", "<3>MESH-SAE-AUTH-BLOCKED addr=aa:bb:cc:dd:ee:ff duration=300", "<", "<9", "<3>X a=b=c ="} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		ev, ok := ParseEvent(raw)
		if ok {
			_ = ev.Arg()
		}
	})
}
