package rtp

import (
	"encoding/binary"
	"errors"
)

// RTCP packet types (RFC 3550 §12.1).
const (
	typeSR    = 200
	typeRR    = 201
	typeSDES  = 202
	typeBYE   = 203
	sdesCNAME = 1
	sdesNOTE  = 7
)

// SenderReport is an RTCP SR without reception report blocks: a sender
// that only sends (half-duplex voice) has nothing to report on.
type SenderReport struct {
	SSRC        uint32
	NTPTime     uint64 // 32.32 fixed point seconds since 1900
	RTPTime     uint32
	PacketCount uint32
	OctetCount  uint32
}

// Report is what one RTCP compound packet said.
type Report struct {
	SR    *SenderReport
	CNAME map[uint32]string // by SSRC
	Note  map[uint32]string // SDES NOTE, by SSRC (Trellis: "all-call")
	Bye   []uint32
}

// maxCNAME bounds a CNAME's length: it is shown to people.
const maxCNAME = 64

// AppendCompound appends a compound RTCP packet: a sender report, a
// source description with the CNAME, and, when bye, a BYE.
func AppendCompound(b []byte, sr SenderReport, cname string, bye bool) []byte {
	return AppendCompoundNote(b, sr, cname, "", bye)
}

// AppendCompoundNote is AppendCompound with an SDES NOTE item when note is
// not empty.
func AppendCompoundNote(b []byte, sr SenderReport, cname, note string, bye bool) []byte {
	// SR: header + 20 bytes of sender info.
	b = appendHeader(b, 0, typeSR, 6)
	b = binary.BigEndian.AppendUint32(b, sr.SSRC)
	b = binary.BigEndian.AppendUint64(b, sr.NTPTime)
	b = binary.BigEndian.AppendUint32(b, sr.RTPTime)
	b = binary.BigEndian.AppendUint32(b, sr.PacketCount)
	b = binary.BigEndian.AppendUint32(b, sr.OctetCount)

	// SDES: one chunk, CNAME, then an end item and padding to 32 bits.
	if len(cname) > maxCNAME {
		cname = cname[:maxCNAME]
	}
	if len(note) > maxCNAME {
		note = note[:maxCNAME]
	}
	items := 2 + len(cname)
	if note != "" {
		items += 2 + len(note)
	}
	chunk := 4 + items + 1
	chunk = (chunk + 3) &^ 3
	b = appendHeader(b, 1, typeSDES, chunk/4)
	b = binary.BigEndian.AppendUint32(b, sr.SSRC)
	b = append(b, sdesCNAME, byte(len(cname)))
	b = append(b, cname...)
	if note != "" {
		b = append(b, sdesNOTE, byte(len(note)))
		b = append(b, note...)
	}
	for i := 4 + items; i < chunk; i++ {
		b = append(b, 0)
	}
	if bye {
		b = appendHeader(b, 1, typeBYE, 1)
		b = binary.BigEndian.AppendUint32(b, sr.SSRC)
	}
	return b
}

// appendHeader writes a header for a packet of words 32-bit words after it.
func appendHeader(b []byte, count, typ, words int) []byte {
	b = append(b, version<<6|byte(count), byte(typ))
	return binary.BigEndian.AppendUint16(b, uint16(words))
}

var errRTCP = errors.New("rtcp: malformed packet")

// ParseCompound reads a compound RTCP packet. Packet types it does not use
// are skipped; any packet that runs past the buffer fails the whole parse.
func ParseCompound(b []byte) (Report, error) {
	var r Report
	if len(b) < 4 {
		return r, errRTCP
	}
	for len(b) > 0 {
		if len(b) < 4 || b[0]>>6 != version {
			return r, errRTCP
		}
		count, typ := int(b[0]&0x1f), int(b[1])
		n := 4 + 4*int(binary.BigEndian.Uint16(b[2:]))
		if n > len(b) {
			return r, errRTCP
		}
		body := b[4:n]
		if b[0]&0x20 != 0 { // padding, only allowed in the last packet
			if n != len(b) || len(body) == 0 || int(body[len(body)-1]) > len(body) {
				return r, errRTCP
			}
			body = body[:len(body)-int(body[len(body)-1])]
		}
		switch typ {
		case typeSR:
			if len(body) < 24 {
				return r, errRTCP
			}
			r.SR = &SenderReport{
				SSRC:        binary.BigEndian.Uint32(body),
				NTPTime:     binary.BigEndian.Uint64(body[4:]),
				RTPTime:     binary.BigEndian.Uint32(body[12:]),
				PacketCount: binary.BigEndian.Uint32(body[16:]),
				OctetCount:  binary.BigEndian.Uint32(body[20:]),
			}
		case typeSDES:
			if err := parseSDES(body, count, &r); err != nil {
				return r, err
			}
		case typeBYE:
			if len(body) < 4*count {
				return r, errRTCP
			}
			for i := 0; i < count; i++ {
				r.Bye = append(r.Bye, binary.BigEndian.Uint32(body[4*i:]))
			}
		}
		b = b[n:]
	}
	return r, nil
}

func parseSDES(b []byte, chunks int, r *Report) error {
	for c := 0; c < chunks; c++ {
		if len(b) < 4 {
			return errRTCP
		}
		ssrc := binary.BigEndian.Uint32(b)
		i := 4
		for {
			if i >= len(b) {
				return errRTCP
			}
			typ := b[i]
			if typ == 0 { // end: skip to the next 32-bit boundary
				i = (i + 4) &^ 3
				break
			}
			if i+2 > len(b) || i+2+int(b[i+1]) > len(b) {
				return errRTCP
			}
			if typ == sdesCNAME || typ == sdesNOTE {
				v := b[i+2 : i+2+int(b[i+1])]
				if len(v) > maxCNAME {
					v = v[:maxCNAME]
				}
				m := &r.CNAME
				if typ == sdesNOTE {
					m = &r.Note
				}
				if *m == nil {
					*m = map[uint32]string{}
				}
				(*m)[ssrc] = printable(v)
			}
			i += 2 + int(b[i+1])
		}
		if i > len(b) {
			return errRTCP
		}
		b = b[i:]
	}
	return nil
}

// printable keeps a name safe to show and log: printable ASCII only.
func printable(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			out = append(out, c)
		}
	}
	return string(out)
}
