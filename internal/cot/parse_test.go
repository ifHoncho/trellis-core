package cot

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseSAXML(t *testing.T) {
	// As ATAK sends SA in XML (a reduced example).
	b := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><event version="2.0" uid="ANDROID-1234" type="a-f-G-U-C" how="h-e" time="2026-11-21T02:00:00.000Z" start="2026-11-21T02:00:00.000Z" stale="2026-11-21T02:06:00.000Z"><point lat="52.0912" lon="5.1215" hae="31.4" ce="4.9" le="9999999.0"/><detail><contact callsign="ALPHA-1"/></detail></event>`)
	p, err := ParseSA(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != "ANDROID-1234" || p.Type != "a-f-G-U-C" || p.Lat != 52.0912 || p.Lon != 5.1215 || p.HAE != 31.4 || p.CE != 4.9 || p.LE != Unknown || p.How != "h-e" {
		t.Fatalf("%+v", p)
	}
	// A height or error no peer would accept is taken as unknown.
	bad := []byte(`<event version="2.0" uid="ANDROID-1" type="a-f-G-U-C" how="h-e"><point lat="52.0912" lon="5.1215" hae="NaN" ce="25000000" le="-4"/></event>`)
	if p, err := ParseSA(bad); err != nil || p.HAE != Unknown || p.CE != Unknown || p.LE != Unknown {
		t.Fatalf("bad height and error: %+v %v", p, err)
	}
	// Our own events read back.
	now := time.Unix(1_800_000_000, 0)
	own, _ := Event{UID: "trellis-node-1", Type: NodeType, Time: now, Start: now, Stale: now, Lat: -33.9, Lon: 18.4, HAE: Unknown, CE: 3, LE: Unknown}.Marshal()
	if p, err := ParseSA(own); err != nil || p.UID != "trellis-node-1" || p.Lat != -33.9 || p.CE != 3 {
		t.Fatalf("own event: %+v %v", p, err)
	}
}

// protobuf helpers for building TAK messages in tests.
func pbKey(num, wire int) []byte { return binary.AppendUvarint(nil, uint64(num<<3|wire)) }
func pbBytes(num int, v []byte) []byte {
	return append(append(pbKey(num, 2), binary.AppendUvarint(nil, uint64(len(v)))...), v...)
}
func pbDouble(num int, v float64) []byte {
	return binary.LittleEndian.AppendUint64(pbKey(num, 1), math.Float64bits(v))
}
func pbVarint(num int, v uint64) []byte { return binary.AppendUvarint(pbKey(num, 0), v) }

func takMessage(ev []byte) []byte {
	msg := append(pbBytes(1, pbVarint(1, 1)), pbBytes(2, ev)...) // a TakControl, then the event
	return append([]byte{0xbf, 0x01, 0xbf}, msg...)
}

func TestParseSATakProtocol(t *testing.T) {
	var ev []byte
	ev = append(ev, pbBytes(1, []byte("a-f-G-U-C"))...)
	ev = append(ev, pbBytes(5, []byte("ANDROID-99"))...)
	ev = append(ev, pbVarint(6, 1_800_000_000_000)...)
	ev = append(ev, pbBytes(9, []byte("m-g"))...)
	ev = append(ev, pbDouble(10, 52.5)...)
	ev = append(ev, pbDouble(11, 13.4)...)
	ev = append(ev, pbDouble(12, 40)...)
	ev = append(ev, pbDouble(13, 6.5)...)
	ev = append(ev, pbDouble(14, Unknown)...)
	ev = append(ev, pbBytes(15, []byte{0x0a, 0x00})...) // a detail, skipped
	p, err := ParseSA(takMessage(ev))
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != "ANDROID-99" || p.Type != "a-f-G-U-C" || p.Lat != 52.5 || p.Lon != 13.4 || p.HAE != 40 || p.CE != 6.5 || p.Self || p.How != "m-g" {
		t.Fatalf("%+v", p)
	}
	// A position report: a detail with takv and a contact endpoint.
	pli := append(ev[:len(ev):len(ev)], pbBytes(15, append(pbBytes(2, pbBytes(1, []byte("*:-1:stcp"))), pbBytes(6, nil)...))...)
	if p, err := ParseSA(takMessage(pli)); err != nil || !p.Self {
		t.Fatalf("position report: %+v %v", p, err)
	}
	// A marker the device shares: takv, but linked to its creator.
	marker := append(ev[:len(ev):len(ev)], pbBytes(15, append(pbBytes(1, []byte(`<link uid="ANDROID-1" relation="p-p"/>`)), pbBytes(6, nil)...))...)
	if p, err := ParseSA(takMessage(marker)); err != nil || p.Self {
		t.Fatalf("marker: %+v %v", p, err)
	}
}

// Only a device's own position report is Self, in XML.
func TestParseSASelfXML(t *testing.T) {
	const head = `<event version="2.0" uid="ANDROID-1" type="a-f-G-U-C" how="%s"><point lat="52.0912" lon="5.1215" hae="31.4" ce="4.9" le="9999999.0"/><detail>%s</detail></event>`
	for _, c := range []struct {
		name, how, detail string
		self              bool
	}{
		{"position report", "m-g", `<takv device="x"/><contact endpoint="*:-1:stcp" callsign="A-1"/>`, true},
		{"endpoint alone", "h-e", `<contact endpoint="*:-1:stcp" callsign="A-1"/>`, true},
		{"marker placed by hand", "h-g-i-g-o", `<contact callsign="hostile-1"/><link uid="ANDROID-1" relation="p-p"/>`, false},
		{"marker sent with takv", "m-g", `<takv device="x"/><link uid="ANDROID-1" relation="p-p"/>`, false},
		{"no device detail", "m-g", `<contact callsign="A-1"/>`, false},
	} {
		p, err := ParseSA([]byte(fmt.Sprintf(head, c.how, c.detail)))
		if err != nil || p.Self != c.self {
			t.Errorf("%s: self %v (%v), want %v", c.name, p.Self, err, c.self)
		}
	}
}

// proto3 leaves out a 0: a phone on the equator sends no lat.
func TestParseSAZeroLat(t *testing.T) {
	ev := append(pbBytes(5, []byte("ANDROID-7")), pbDouble(11, 32.5)...)
	if p, err := ParseSA(takMessage(ev)); err != nil || p.Lat != 0 || p.Lon != 32.5 || p.HAE != Unknown {
		t.Fatalf("on the equator, hae left out (unknown): %+v %v", p, err)
	}
}

func TestParseSARefuses(t *testing.T) {
	noPos := pbBytes(5, []byte("x")) // neither lat nor lon: (0,0)
	for name, b := range map[string][]byte{
		"empty":         nil,
		"not xml":       []byte("hello"),
		"bad header":    {0xbf, 0x01, 0x00, 0x01},
		"version 2":     {0xbf, 0x02, 0xbf},
		"no event":      {0xbf, 0x01, 0xbf},
		"no position":   takMessage(noPos),
		"bad length":    {0xbf, 0x01, 0xbf, 0x12, 0xff, 0x01},
		"short fixed64": takMessage(append(pbKey(10, 1), 1, 2, 3)),
		"group":         takMessage(pbKey(3, 3)),
		"out of range":  []byte(`<event uid="u" type="t"><point lat="91" lon="0"/></event>`),
		"zero position": []byte(`<event uid="u" type="t"><point lat="0" lon="0"/></event>`),
		"bad lat":       []byte(`<event uid="u" type="t"><point lat="x" lon="0"/></event>`),
		"too large":     make([]byte, maxSA+1),
	} {
		if _, err := ParseSA(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Datagrams from any device on the node's network: never a panic.
func FuzzParseSA(f *testing.F) {
	f.Add([]byte(`<event uid="u" type="t"><point lat="1" lon="2"/></event>`))
	f.Add(takMessage(append(pbDouble(10, 1), pbDouble(11, 2)...)))
	f.Add([]byte{0xbf, 0x01, 0xbf, 0x12, 0x02, 0x51})
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := ParseSA(b)
		if err == nil && (p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180) {
			t.Fatalf("out of range accepted: %+v", p)
		}
	})
}

// A how that is not one is not passed on.
func TestCleanHow(t *testing.T) {
	for how, want := range map[string]string{"m-g": "m-g", "h-g-i-g-o": "h-g-i-g-o", "h-e": "h-e", "": "", "m-g\n<x>": "", "a-" + strings.Repeat("x", 40): ""} {
		if got := cleanHow(how); got != want {
			t.Errorf("cleanHow(%q) = %q, want %q", how, got, want)
		}
	}
}
