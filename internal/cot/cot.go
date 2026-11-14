// Package cot writes Cursor on Target events, the position messages ATAK
// and other TAK clients exchange. Only what a node needs to put itself on
// the map: an event with a point, a callsign and remarks, in CoT's XML
// form, which TAK clients accept on the mesh SA group whatever protocol
// version they send themselves.
package cot

import (
	"encoding/xml"
	"fmt"
	"math"
	"time"
)

// SA is where TAK clients exchange positions on a local network.
const (
	SAGroup = "239.2.3.1"
	SAPort  = 6969
)

// Unknown is CoT's value for an unknown height or error.
const Unknown = 9999999.0

// NodeType is a Trellis node's CoT type: friendly ground equipment, drawn
// with an equipment frame, distinct from the unit frame of a person
// (a-f-G-U-C).
const NodeType = "a-f-G-E-S"

// Event is one CoT event.
type Event struct {
	UID      string
	Type     string
	How      string // m-g: machine, GPS
	Time     time.Time
	Start    time.Time
	Stale    time.Time
	Lat, Lon float64
	// HAE is the height above the ellipsoid in metres; CE and LE the
	// horizontal and vertical errors in metres. Unknown when not known.
	HAE, CE, LE float64
	Callsign    string
	Remarks     string
	// GPS marks the position as from a GNSS receiver.
	GPS bool
}

type xmlEvent struct {
	XMLName xml.Name  `xml:"event"`
	Version string    `xml:"version,attr"`
	UID     string    `xml:"uid,attr"`
	Type    string    `xml:"type,attr"`
	How     string    `xml:"how,attr"`
	Time    string    `xml:"time,attr"`
	Start   string    `xml:"start,attr"`
	Stale   string    `xml:"stale,attr"`
	Point   xmlPoint  `xml:"point"`
	Detail  xmlDetail `xml:"detail"`
}

type xmlPoint struct {
	Lat string `xml:"lat,attr"`
	Lon string `xml:"lon,attr"`
	HAE string `xml:"hae,attr"`
	CE  string `xml:"ce,attr"`
	LE  string `xml:"le,attr"`
}

type xmlDetail struct {
	Contact *xmlContact `xml:"contact,omitempty"`
	Precise *xmlPrecise `xml:"precisionlocation,omitempty"`
	Remarks *xmlRemarks `xml:"remarks,omitempty"`
}

type xmlContact struct {
	Callsign string `xml:"callsign,attr"`
}

type xmlPrecise struct {
	Geo string `xml:"geopointsrc,attr"`
	Alt string `xml:"altsrc,attr"`
}

type xmlRemarks struct {
	Text string `xml:",chardata"`
}

// timeFormat is CoT's: UTC with milliseconds.
const timeFormat = "2006-01-02T15:04:05.000Z"

func num(v float64, decimals int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		v = Unknown
	}
	return fmt.Sprintf("%.*f", decimals, v)
}

// Marshal writes the event as one XML document.
func (e Event) Marshal() ([]byte, error) {
	if e.UID == "" || e.Type == "" {
		return nil, fmt.Errorf("cot: event needs a uid and a type")
	}
	if !(e.Lat >= -90 && e.Lat <= 90 && e.Lon >= -180 && e.Lon <= 180) {
		return nil, fmt.Errorf("cot: position %v,%v out of range", e.Lat, e.Lon)
	}
	how := e.How
	if how == "" {
		how = "m-g"
	}
	x := xmlEvent{
		Version: "2.0", UID: e.UID, Type: e.Type, How: how,
		Time: e.Time.UTC().Format(timeFormat), Start: e.Start.UTC().Format(timeFormat), Stale: e.Stale.UTC().Format(timeFormat),
		Point: xmlPoint{Lat: num(e.Lat, 7), Lon: num(e.Lon, 7), HAE: num(e.HAE, 1), CE: num(e.CE, 1), LE: num(e.LE, 1)},
	}
	if e.Callsign != "" {
		x.Detail.Contact = &xmlContact{Callsign: e.Callsign}
	}
	if e.GPS {
		alt := "GPS"
		if e.HAE == Unknown {
			alt = "???"
		}
		x.Detail.Precise = &xmlPrecise{Geo: "GPS", Alt: alt}
	}
	if e.Remarks != "" {
		x.Detail.Remarks = &xmlRemarks{Text: e.Remarks}
	}
	body, err := xml.Marshal(x)
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header[:len(xml.Header)-1]), body...), nil
}
