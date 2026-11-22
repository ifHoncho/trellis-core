package gnss

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// NMEA writes the GGA and RMC sentences for a fix, as an external GPS
// sends them: ATAK reads them on its external GPS port (UDP 4349). Empty
// when the state holds no fix.
func NMEA(s State) []byte {
	if s.Mode < Mode2D || math.IsNaN(s.Lat) || math.IsNaN(s.Lon) || (s.Lat == 0 && s.Lon == 0) {
		return nil
	}
	t := s.Time
	if t.IsZero() {
		t = s.Heard
	}
	// Cut to the hundredth NMEA prints: rounding up could print 60.00 s.
	t = t.UTC().Truncate(10 * time.Millisecond)
	hms := fmt.Sprintf("%02d%02d%05.2f", t.Hour(), t.Minute(), float64(t.Second())+float64(t.Nanosecond())/1e9)
	lat, ns := nmeaCoord(s.Lat, 2), "N"
	if s.Lat < 0 {
		ns = "S"
	}
	lon, ew := nmeaCoord(s.Lon, 3), "E"
	if s.Lon < 0 {
		ew = "W"
	}
	quality := 1 // a GPS fix
	// Satellites used: left empty when not known (before gpsd's first SKY,
	// or a receiver that does not say), not "00" with a valid fix.
	sats := ""
	if s.SatsUsed > 0 {
		sats = fmt.Sprintf("%02d", min(s.SatsUsed, 99))
	}
	// HDOP is not known here: an estimate from the horizontal error, at
	// about 5 m per unit for a consumer receiver; 99.9 when unknown.
	hdop := "99.9"
	if s.HAccM > 0 {
		hdop = fmt.Sprintf("%.1f", math.Max(0.5, math.Min(99.9, s.HAccM/5))) // 0.0 reads as unknown
	}
	// GGA's altitude is above mean sea level, with the geoid separation
	// after it (ellipsoid height = the two added): sent only with both
	// known (gpsd 3.20 on gives them). With one alone the other would be
	// a guess, and a reader that fills it in its own way (a missing
	// separation as 0, its own geoid model) is tens of metres off; no
	// height at all avoids that.
	alt, sep := "", ""
	if s.Mode >= Mode3D && s.HasMSL && s.HasSep {
		alt, sep = fmt.Sprintf("%.1f", s.MSLM), fmt.Sprintf("%.1f", s.SepM)
	}
	gga := fmt.Sprintf("GPGGA,%s,%s,%s,%s,%s,%d,%s,%s,%s,M,%s,M,,", hms, lat, ns, lon, ew, quality, sats, hdop, alt, sep)
	rmc := fmt.Sprintf("GPRMC,%s,A,%s,%s,%s,%s,,,%02d%02d%02d,,,A", hms, lat, ns, lon, ew, t.Day(), int(t.Month()), t.Year()%100)
	var b strings.Builder
	for _, body := range []string{gga, rmc} {
		fmt.Fprintf(&b, "$%s*%02X\r\n", body, nmeaChecksum(body))
	}
	return []byte(b.String())
}

// nmeaCoord writes degrees as NMEA's ddmm.mmmm (latitude, deg digits 2)
// or dddmm.mmmm (longitude, 3), without the sign.
func nmeaCoord(v float64, degDigits int) string {
	v = math.Abs(v)
	deg := math.Floor(v)
	// Rounded to the 4 decimals printed, carrying a full 60 minutes.
	min := math.Round((v-deg)*60*1e4) / 1e4
	if min >= 60 {
		deg, min = deg+1, min-60
	}
	return fmt.Sprintf("%0*d%07.4f", degDigits, int(deg), min)
}

func nmeaChecksum(body string) byte {
	var c byte
	for i := 0; i < len(body); i++ {
		c ^= body[i]
	}
	return c
}
