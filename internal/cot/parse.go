package cot

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Point is the position an SA message reports.
type Point struct {
	UID, Type   string
	Lat, Lon    float64
	HAE, CE, LE float64 // Unknown when the sender does not know
	// How is the event's how ("m-g": a GPS fix, "h-e": entered by hand).
	How string
	// Self: the message is the sender's report of its own position (its
	// position report, PLI), not an item on its map: it carries the
	// device's software (takv) or a contact endpoint, and no link to a
	// creator (p-p), and was not placed by hand (how h-g-i).
	Self bool
}

// reHow is a CoT how as senders write it ("m-g", "h-g-i-g-o"): a few
// letters between hyphens. Anything else is not passed on.
var reHow = regexp.MustCompile(`^[a-z](-[a-zA-Z0-9]{1,8}){0,6}$`)

// cleanHow is how when it looks like one, else "".
func cleanHow(how string) string {
	if len(how) > 32 || !reHow.MatchString(how) {
		return ""
	}
	return how
}

// selfReport is the Self rule (Point.Self).
func selfReport(how string, takv, endpoint, placed bool) bool {
	return (takv || endpoint) && !placed && !strings.HasPrefix(how, "h-g-i")
}

// maxSA bounds the messages read: a phone's SA is well under a kilobyte.
const maxSA = 8 << 10

// ParseSA reads a position from one SA datagram: CoT XML, or TAK protocol
// version 1 as ATAK sends it on a mesh (a 0xbf, the version, 0xbf header,
// then a TakMessage in protobuf). Untrusted input: anything malformed is
// an error.
func ParseSA(b []byte) (Point, error) {
	if len(b) > maxSA {
		return Point{}, errors.New("cot: message too large")
	}
	if len(b) >= 3 && b[0] == 0xbf {
		ver, n := binary.Uvarint(b[1:])
		if n <= 0 || 1+n >= len(b) || b[1+n] != 0xbf {
			return Point{}, errors.New("cot: bad TAK protocol header")
		}
		if ver != 1 {
			return Point{}, fmt.Errorf("cot: TAK protocol version %d", ver)
		}
		return parseTakMessage(b[2+n:])
	}
	return parseXML(b)
}

func parseXML(b []byte) (Point, error) {
	var x xmlEvent
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	if err := d.Decode(&x); err != nil {
		return Point{}, fmt.Errorf("cot: %w", err)
	}
	p := Point{UID: x.UID, Type: x.Type, HAE: Unknown, CE: Unknown, LE: Unknown}
	p.How = cleanHow(x.How)
	placed := false
	for _, l := range x.Detail.Links {
		placed = placed || l.Relation == "p-p"
	}
	p.Self = selfReport(x.How, x.Detail.Takv != nil, x.Detail.Contact != nil && x.Detail.Contact.Endpoint != "", placed)
	var err error
	if p.Lat, err = strconv.ParseFloat(x.Point.Lat, 64); err != nil {
		return Point{}, errors.New("cot: bad lat")
	}
	if p.Lon, err = strconv.ParseFloat(x.Point.Lon, 64); err != nil {
		return Point{}, errors.New("cot: bad lon")
	}
	for _, f := range []struct {
		s string
		v *float64
	}{{x.Point.HAE, &p.HAE}, {x.Point.CE, &p.CE}, {x.Point.LE, &p.LE}} {
		if v, err := strconv.ParseFloat(f.s, 64); err == nil {
			*f.v = v
		}
	}
	p.clean()
	return p, p.check()
}

// The protobuf fields read (takproto's takmessage.proto, cotevent.proto
// and detail.proto): TakMessage.cotEvent = 2; CotEvent.type = 1, uid = 5,
// how = 9, lat = 10, lon = 11, hae = 12, ce = 13, le = 14, detail = 15;
// Detail.xmlDetail = 1, contact = 2 (Contact.endpoint = 1), takv = 6.
func parseTakMessage(b []byte) (Point, error) {
	var ev []byte
	err := protoFields(b, func(num int, wire int, v uint64, data []byte) {
		if num == 2 && wire == 2 {
			ev = data
		}
	})
	if err != nil {
		return Point{}, err
	}
	if ev == nil {
		return Point{}, errors.New("cot: TAK message without an event")
	}
	// A hae, ce or le left out stays unknown. proto3 leaves out a 0, so a
	// device at exactly 0 m loses its height; but not every client sends
	// 9999999 for unknown, and no height is safer than a wrong one (0 m
	// can be hundreds of metres off).
	p := Point{HAE: Unknown, CE: Unknown, LE: Unknown}
	var how string
	var detail []byte
	err = protoFields(ev, func(num int, wire int, v uint64, data []byte) {
		switch {
		case num == 1 && wire == 2:
			p.Type = string(data)
		case num == 5 && wire == 2:
			p.UID = string(data)
		case num == 9 && wire == 2:
			how = string(data)
		case num == 15 && wire == 2:
			detail = data
		case wire == 1:
			f := math.Float64frombits(v)
			switch num {
			case 10:
				p.Lat = f
			case 11:
				p.Lon = f
			case 12:
				p.HAE = f
			case 13:
				p.CE = f
			case 14:
				p.LE = f
			}
		}
	})
	if err != nil {
		return Point{}, err
	}
	// proto3 leaves out a field that is 0: a missing lat or lon is the
	// equator or the prime meridian; check refuses (0,0), no position.
	var takv, endpoint, placed bool
	if detail != nil {
		err = protoFields(detail, func(num int, wire int, v uint64, data []byte) {
			switch {
			case num == 1 && wire == 2:
				placed = bytes.Contains(data, []byte(`relation="p-p"`)) || bytes.Contains(data, []byte(`relation='p-p'`))
			case num == 6 && wire == 2:
				takv = true
			case num == 2 && wire == 2:
				_ = protoFields(data, func(num int, wire int, v uint64, d []byte) {
					if num == 1 && wire == 2 && len(d) > 0 {
						endpoint = true
					}
				})
			}
		})
		if err != nil {
			return Point{}, err
		}
	}
	p.Self, p.How = selfReport(how, takv, endpoint, placed), cleanHow(how)
	p.clean()
	return p, p.check()
}

// protoFields walks the fields of one protobuf message: varints (wire 0)
// and 64-bit values (1) in v, length-delimited ones (2) in data; 32-bit
// values (5) are skipped. Groups are refused.
func protoFields(b []byte, fn func(num int, wire int, v uint64, data []byte)) error {
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return errors.New("cot: bad protobuf key")
		}
		b = b[n:]
		num, wire := int(key>>3), int(key&7)
		if num <= 0 {
			return errors.New("cot: bad protobuf field number")
		}
		switch wire {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return errors.New("cot: bad protobuf varint")
			}
			b = b[n:]
			fn(num, wire, v, nil)
		case 1:
			if len(b) < 8 {
				return errors.New("cot: short protobuf fixed64")
			}
			fn(num, wire, binary.LittleEndian.Uint64(b), nil)
			b = b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return errors.New("cot: bad protobuf length")
			}
			fn(num, wire, 0, b[n:n+int(l)])
			b = b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return errors.New("cot: short protobuf fixed32")
			}
			b = b[4:]
		default:
			return fmt.Errorf("cot: protobuf wire type %d", wire)
		}
	}
	return nil
}

// clean makes a height or error no node could pass on Unknown: not a
// number, or out of the range gossip accepts (gossip.Position: height
// -1000..100000 m, error 0..1000000 m).
func (p *Point) clean() {
	bad := func(v, lo, hi float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) || v < lo || v > hi }
	if p.HAE != Unknown && bad(p.HAE, -1000, 100000) {
		p.HAE = Unknown
	}
	if p.CE != Unknown && bad(p.CE, 0, 1000000) {
		p.CE = Unknown
	}
	if p.LE != Unknown && bad(p.LE, 0, 1000000) {
		p.LE = Unknown
	}
}

func (p Point) check() error {
	if math.IsNaN(p.Lat) || math.IsNaN(p.Lon) || p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180 {
		return fmt.Errorf("cot: position %v,%v out of range", p.Lat, p.Lon)
	}
	if p.Lat == 0 && p.Lon == 0 {
		return errors.New("cot: no position (0,0)")
	}
	return nil
}
