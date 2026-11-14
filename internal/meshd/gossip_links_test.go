package meshd

import (
	"strings"
	"testing"
)

// Another node's links are compared with this node's, link by link; the
// order does not matter, and a node that sends none is not judged.
func TestLinkDifferences(t *testing.T) {
	mine := []string{"2g/6/20", "5g/149/80", "s1g/26/2"}
	for name, c := range map[string]struct {
		theirs []string
		want   []string // substrings, in order; nil: no differences
	}{
		"same":                   {theirs: []string{"2g/6/20", "5g/149/80", "s1g/26/2"}},
		"same, another order":    {theirs: []string{"5g/149/80", "s1g/26/2", "2g/6/20"}},
		"older version":          {theirs: nil},
		"width (the bench case)": {theirs: []string{"2g/6/20", "5g/149/40", "s1g/26/2"}, want: []string{"runs the 5 GHz · ch 149 link at 40 MHz, this node at 80 MHz"}},
		"channel":                {theirs: []string{"2g/6/20", "5g/36/80", "s1g/26/2"}, want: []string{"has its 5 GHz link on channel 36, this node on channel 149"}},
		"missing and extra": {theirs: []string{"2g/6/20", "s1g/26/2", "6g/37/160"},
			want: []string{"its profile has no 5 GHz · ch 149 link", "has a 6 GHz · ch 37 link this node's lacks"}},
	} {
		got := linkDifferences(mine, c.theirs)
		if len(got) != len(c.want) {
			t.Errorf("%s: %q, want %d differences", name, got, len(c.want))
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: %q, want %q", name, got[i], w)
			}
		}
	}
}
