package meshd

import (
	"fmt"
	"sort"
	"strings"

	"meridian/trellis/internal/version"
)

// releaseDifferences compares this node's version with the other nodes'
// (callsign → version, from gossip) and returns a warning for the mesh's
// operator, or an empty code when there is nothing to say.
//
//   - Nodes on another release series (the major number, or major.minor
//     below 1.0) are a warning: formats may differ between series.
//   - Nodes on another release of the same series are a notice.
//   - Development builds are compared with releases by the release they
//     were built from, and never with each other: a bench runs a different
//     build on every board.
func releaseDifferences(mine string, others map[string]string) (level, code, msg string) {
	me := version.Parse(mine)
	if !me.Numbered {
		return "", "", ""
	}
	var series, patch []string
	for callsign, v := range others {
		o := version.Parse(v)
		if !o.Numbered {
			continue
		}
		switch {
		case o.Series() != me.Series():
			series = append(series, fmt.Sprintf("%s (%s)", callsign, strings.TrimPrefix(v, "v")))
		case me.Exact && o.Exact && o.Compare(me) != 0:
			patch = append(patch, fmt.Sprintf("%s (%s)", callsign, strings.TrimPrefix(v, "v")))
		}
	}
	sort.Strings(series)
	sort.Strings(patch)
	shown := strings.TrimPrefix(mine, "v")
	switch {
	case len(series) > 0:
		return "warning", "release-differs", fmt.Sprintf(
			"%s %s another Trellis release series than this node (%s). Nodes of different series may not follow each other's mesh-wide changes; upgrade every node to the same release (meshctl upgrade)",
			listAnd(series), plural(len(series), "runs", "run"), shown)
	case len(patch) > 0:
		return "info", "version-differs", fmt.Sprintf(
			"%s %s another Trellis version than this node (%s), of the same release series; they work together. Upgrade them to the same release when convenient",
			listAnd(patch), plural(len(patch), "runs", "run"), shown)
	}
	return "", "", ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
