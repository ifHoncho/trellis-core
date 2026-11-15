package meshd

import (
	"strings"
	"testing"
)

func TestReleaseDifferences(t *testing.T) {
	cases := []struct {
		name   string
		mine   string
		others map[string]string
		code   string
		want   []string
	}{
		{"same release", "0.3.1", map[string]string{"a": "0.3.1", "b": "v0.3.1"}, "", nil},
		{"another series", "0.3.1", map[string]string{"a": "0.4.0", "b": "0.3.1", "c": "0.2.9"}, "release-differs", []string{"a (0.4.0) and c (0.2.9) run", "(0.3.1)"}},
		{"one node on another series", "1.2.0", map[string]string{"a": "2.0.0", "b": "1.9.9"}, "release-differs", []string{"a (2.0.0) runs"}},
		{"same series, other patch", "0.3.1", map[string]string{"a": "0.3.2", "b": "0.3.1"}, "version-differs", []string{"a (0.3.2) runs"}},
		{"1.x is one series", "1.2.0", map[string]string{"a": "1.9.9"}, "version-differs", nil},
		{"the series warning wins", "0.3.1", map[string]string{"a": "0.3.2", "b": "0.5.0"}, "release-differs", []string{"b (0.5.0)"}},
		{"development builds among themselves", "1a2b3c4-dirty+1114.0930", map[string]string{"a": "5d6e7f8+1114.0931", "b": "0.3.1"}, "", nil},
		{"a build after a release, same series", "0.3.1-4-g1a2b3c4", map[string]string{"a": "0.3.1", "b": "0.3.2-1-gabc"}, "", nil},
		{"a build after a release, other series", "0.4.0-4-g1a2b3c4", map[string]string{"a": "0.3.1"}, "release-differs", []string{"a (0.3.1)"}},
		{"nodes that say nothing or are unnumbered", "0.3.1", map[string]string{"a": "", "b": "dev"}, "", nil},
	}
	for _, c := range cases {
		_, code, msg := releaseDifferences(c.mine, c.others)
		if code != c.code {
			t.Errorf("%s: code %q, want %q (%s)", c.name, code, c.code, msg)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(msg, w) {
				t.Errorf("%s: message lacks %q: %s", c.name, w, msg)
			}
		}
	}
}
