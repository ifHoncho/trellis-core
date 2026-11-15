// Package version reads Trellis version strings: releases ("0.3.1"),
// builds between releases as git describes them ("0.3.1-4-g1a2b3c4"), and
// development builds with no release behind them ("1a2b3c4-dirty+1114.0930").
//
// Every node of a mesh should run the same release series: nodes of one
// series speak the same gossip, profile and mesh-wide change formats. The
// series is the major number, or major.minor while the major is 0.
package version

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a parsed version string.
type Version struct {
	Raw                 string
	Major, Minor, Patch int
	// Numbered: the string starts with a release number. A development
	// build with no release behind it has none.
	Numbered bool
	// Exact: the build is the release itself, with nothing after it.
	Exact bool
}

// Parse reads a version string. It never fails: a string without a release
// number is a development build.
func Parse(s string) Version {
	v := Version{Raw: s}
	t := strings.TrimPrefix(strings.TrimSpace(s), "v")
	// The release number ends at the first character that cannot be part
	// of one.
	end := strings.IndexFunc(t, func(r rune) bool { return r != '.' && (r < '0' || r > '9') })
	num, rest := t, ""
	if end >= 0 {
		num, rest = t[:end], t[end:]
	}
	parts := strings.Split(num, ".")
	if len(parts) != 3 {
		return v
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || len(p) > 6 {
			return v
		}
		n[i] = x
	}
	v.Major, v.Minor, v.Patch = n[0], n[1], n[2]
	v.Numbered = true
	v.Exact = rest == ""
	return v
}

// Series names the release series, "0.3" or "2"; empty for a development
// build with no release number.
func (v Version) Series() string {
	if !v.Numbered {
		return ""
	}
	if v.Major == 0 {
		return fmt.Sprintf("0.%d", v.Minor)
	}
	return strconv.Itoa(v.Major)
}

// Compare orders two numbered versions by release number: -1, 0 or 1.
func (v Version) Compare(o Version) int {
	for _, d := range [3]int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d < 0 {
			return -1
		}
		if d > 0 {
			return 1
		}
	}
	return 0
}

// Debian turns a version string into a Debian package version, so that
// dpkg orders versions as they were made:
//
//   - a build after a release (git describe's "-N-gHASH", "-dirty", or
//     "+anything") sorts after it: "0.3.1-4-g1a2b3c4" → "0.3.1+4.g1a2b3c4";
//   - anything else after the number is a pre-release, which sorts before
//     it: "0.3.1-rc1" → "0.3.1~rc1";
//   - a development build with no release number sorts before every
//     release: "1a2b3c4-dirty" → "0.0.0~1a2b3c4.dirty".
func Debian(s string) string {
	v := Parse(s)
	clean := func(t string) string {
		var b strings.Builder
		for _, r := range t {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '+', r == '~':
				b.WriteRune(r)
			default:
				b.WriteByte('.')
			}
		}
		return strings.Trim(b.String(), ".+")
	}
	if !v.Numbered {
		t := clean(s)
		if t == "" {
			t = "dev"
		}
		return "0.0.0~" + t
	}
	num := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Exact {
		return num
	}
	t := strings.TrimPrefix(strings.TrimSpace(s), "v")
	raw := t[strings.IndexFunc(t, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }):]
	rest := clean(raw)
	switch {
	case rest == "":
		return num
	case afterRelease.MatchString(raw):
		return num + "+" + rest
	}
	return num + "~" + strings.TrimLeft(rest, "~")
}

// afterRelease matches what follows the number of a build made after that
// release.
var afterRelease = regexp.MustCompile(`^(-[0-9]+-g[0-9a-f]+|-dirty|\+)`)
