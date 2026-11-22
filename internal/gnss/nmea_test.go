package gnss

import (
	"strings"
	"testing"
	"time"
)

func TestNMEA(t *testing.T) {
	s := State{Mode: Mode3D, Time: time.Date(2026, 11, 21, 2, 3, 4, 500_000_000, time.UTC),
		Lat: 52.0912, Lon: -5.1215, AltM: 31.44, HasAlt: true, HAccM: 4, SatsUsed: 9}
	got := string(NMEA(s))
	lines := strings.Split(strings.TrimSuffix(got, "\r\n"), "\r\n")
	if len(lines) != 2 {
		t.Fatalf("%q", got)
	}
	want := []string{
		"$GPGGA,020304.50,5205.4720,N,00507.2900,W,1,09,0.8,,M,,M,,*", // ellipsoid height only: no height
		"$GPRMC,020304.50,A,5205.4720,N,00507.2900,W,,,211126,,,A*41",
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d:\n got %s\nwant %s..", i, lines[i], w)
		}
		body := lines[i][1:strings.IndexByte(lines[i], '*')]
		if sum := lines[i][len(lines[i])-2:]; sum != strings.ToUpper(hex2(nmeaChecksum(body))) {
			t.Errorf("line %d: checksum %s", i, sum)
		}
	}
	// Height above sea level and the geoid separation, as GGA wants them.
	sm := s
	sm.MSLM, sm.SepM, sm.HasMSL, sm.HasSep = -13.6, 45.0, true, true
	if g := string(NMEA(sm)); !strings.Contains(g, ",0.8,-13.6,M,45.0,M,,*") {
		t.Errorf("sea level and separation: %q", g)
	}
	// Sea level with no separation: no height rather than a wrong one.
	so := s
	so.HasAlt, so.MSLM, so.HasMSL = false, 12.0, true
	if g := string(NMEA(so)); !strings.Contains(g, ",0.8,,M,,M,,*") {
		t.Errorf("sea level only: %q", g)
	}
	// Southern hemisphere, a 2D fix (no altitude), unknown error and
	// satellite count (left empty, not "00").
	s2 := State{Mode: Mode2D, Heard: s.Time, Lat: -33.9, Lon: 18.4}
	if g := string(NMEA(s2)); !strings.Contains(g, "3354.0000,S,01824.0000,E,1,,99.9,,M") {
		t.Errorf("2D fix: %q", g)
	}
	// Nor 60 seconds: 59.996 s is cut to 59.99.
	s3 := State{Mode: Mode2D, Time: time.Date(2026, 11, 21, 2, 3, 59, 996_000_000, time.UTC), Lat: 1, Lon: 1}
	if g := string(NMEA(s3)); !strings.HasPrefix(g, "$GPGGA,020359.99,") {
		t.Errorf("59.996 s: %q", g)
	}
	// Rounding never prints 60 minutes.
	// 59.99997 minutes prints as the next degree.
	for v, want := range map[float64]string{52.9999995: "5300.0000", 52.999999: "5259.9999", 179.9999999: "18000.0000", 0.5: "0030.0000"} {
		deg := 2
		if v > 99 {
			deg = 3
		}
		if c := nmeaCoord(v, deg); c != want {
			t.Errorf("nmeaCoord(%v) = %s, want %s", v, c, want)
		}
	}
	if c := nmeaCoord(10.99999999, 2); c != "1100.0000" {
		t.Errorf("rounding: %s", c)
	}
	for _, bad := range []State{{Mode: 1, Lat: 1, Lon: 1}, {Mode: Mode3D}} {
		if NMEA(bad) != nil {
			t.Errorf("no fix wrote sentences: %+v", bad)
		}
	}
}

func hex2(b byte) string {
	const h = "0123456789ABCDEF"
	return string([]byte{h[b>>4], h[b&15]})
}
