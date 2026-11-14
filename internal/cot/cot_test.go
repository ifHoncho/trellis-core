package cot

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestMarshal(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 600e6, time.FixedZone("x", 3600))
	e := Event{
		UID: "trellis-node-a0b0c0000001", Type: NodeType, Time: now, Start: now, Stale: now.Add(3 * time.Minute),
		Lat: 52.1234567, Lon: -5.7654321, HAE: 251.27, CE: 23, LE: Unknown,
		Callsign: "relay-1", Remarks: "Trellis node <relay-1> & co", GPS: true,
	}
	b, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?><event version="2.0" uid="trellis-node-a0b0c0000001" type="a-f-G-E-S" how="m-g"`,
		`time="2026-01-02T02:04:05.600Z" start="2026-01-02T02:04:05.600Z" stale="2026-01-02T02:07:05.600Z"`,
		`<point lat="52.1234567" lon="-5.7654321" hae="251.3" ce="23.0" le="9999999.0"></point>`,
		`<contact callsign="relay-1"></contact>`,
		`<precisionlocation geopointsrc="GPS" altsrc="GPS"></precisionlocation>`,
		`<remarks>Trellis node &lt;relay-1&gt; &amp; co</remarks>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s\nin %s", want, s)
		}
	}
	if strings.Contains(s, "\n") {
		t.Errorf("event spans lines: %q", s)
	}
	// It parses back as XML.
	var v struct {
		UID string `xml:"uid,attr"`
	}
	if err := xml.Unmarshal(b[strings.Index(s, "<event"):], &v); err != nil || v.UID != e.UID {
		t.Errorf("does not parse back: %v %q", err, v.UID)
	}
}

func TestMarshalUnknownHeight(t *testing.T) {
	now := time.Unix(0, 0)
	b, err := Event{UID: "u", Type: NodeType, Time: now, Start: now, Stale: now, HAE: Unknown, CE: Unknown, LE: Unknown, GPS: true}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `altsrc="???"`) || strings.Contains(string(b), "<contact") || strings.Contains(string(b), "<remarks") {
		t.Errorf("unexpected: %s", b)
	}
}

func TestMarshalRejects(t *testing.T) {
	for _, e := range []Event{{Type: NodeType}, {UID: "u"}, {UID: "u", Type: NodeType, Lat: 91}, {UID: "u", Type: NodeType, Lon: -181}} {
		if _, err := e.Marshal(); err == nil {
			t.Errorf("%+v: accepted", e)
		}
	}
}
