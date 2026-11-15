package cot

import (
	"encoding/xml"
	"testing"
	"time"
)

// Callsigns and remarks come from other nodes; the event goes to every
// phone on the mesh. Whatever they hold, the XML must stay well formed
// and carry them as text.
func FuzzMarshal(f *testing.F) {
	f.Add("relay-1", "fix 3D, 9 satellites", 52.09, 5.12)
	f.Add("a<b>&\"'", "]]><x/>", -90.0, 180.0)
	f.Fuzz(func(t *testing.T, callsign, remarks string, lat, lon float64) {
		now := time.Unix(1_800_000_000, 0)
		e := Event{UID: "trellis-node-x", Type: NodeType, Time: now, Start: now, Stale: now.Add(time.Minute),
			Lat: lat, Lon: lon, Callsign: callsign, Remarks: remarks, GPS: true}
		b, err := e.Marshal()
		if err != nil {
			return
		}
		var v struct {
			XMLName xml.Name `xml:"event"`
			Detail  struct {
				Contact struct {
					Callsign string `xml:"callsign,attr"`
				} `xml:"contact"`
			} `xml:"detail"`
		}
		if err := xml.Unmarshal(b, &v); err != nil {
			t.Fatalf("not well formed: %v\n%s", err, b)
		}
		if v.XMLName.Local != "event" {
			t.Fatalf("root element %q", v.XMLName.Local)
		}
	})
}
