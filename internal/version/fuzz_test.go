package version

import (
	"regexp"
	"testing"
)

// Versions come from other nodes' gossip and from packages: Debian()'s
// output goes to dpkg, and must be a valid Debian version whatever came in.
var debianVersion = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~]*$`)

func FuzzVersion(f *testing.F) {
	for _, s := range []string{"0.3.1", "v0.3.1-4-g1a2b3c4-dirty+1114.0930", "1a2b3c4", "0.3.1-rc1", "", "....", "9999999.1.1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v := Parse(s)
		_ = v.Series()
		if d := Debian(s); !debianVersion.MatchString(d) {
			t.Fatalf("Debian(%q) = %q, not a Debian version", s, d)
		}
	})
}
