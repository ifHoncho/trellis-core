// Package rtp reads and writes RTP packets (RFC 3550 §5) and the RTCP
// packets voice uses: sender reports, source descriptions (CNAME) and BYE
// (RFC 3550 §6). Packets come from the network, so every parser checks
// lengths before it reads.
package rtp

import (
	"encoding/binary"
	"errors"
)

const version = 2

// Packet is one RTP packet.
type Packet struct {
	Marker      bool
	PayloadType uint8
	Seq         uint16
	Timestamp   uint32
	SSRC        uint32
	Payload     []byte // aliases the buffer given to Parse
}

// HeaderLen is the length of a header without CSRCs or an extension.
const HeaderLen = 12

var (
	errShort   = errors.New("rtp: packet too short")
	errVersion = errors.New("rtp: not version 2")
	errPadding = errors.New("rtp: bad padding")
)

// Append appends the packet, with a plain 12-byte header, to b.
func (p *Packet) Append(b []byte) []byte {
	var h [HeaderLen]byte
	h[0] = version << 6
	h[1] = p.PayloadType & 0x7f
	if p.Marker {
		h[1] |= 0x80
	}
	binary.BigEndian.PutUint16(h[2:], p.Seq)
	binary.BigEndian.PutUint32(h[4:], p.Timestamp)
	binary.BigEndian.PutUint32(h[8:], p.SSRC)
	return append(append(b, h[:]...), p.Payload...)
}

// Parse reads an RTP packet. CSRCs and a header extension are skipped,
// padding removed. Payload aliases b.
func Parse(b []byte) (Packet, error) {
	var p Packet
	if len(b) < HeaderLen {
		return p, errShort
	}
	if b[0]>>6 != version {
		return p, errVersion
	}
	padding, ext, cc := b[0]&0x20 != 0, b[0]&0x10 != 0, int(b[0]&0x0f)
	p.Marker = b[1]&0x80 != 0
	p.PayloadType = b[1] & 0x7f
	p.Seq = binary.BigEndian.Uint16(b[2:])
	p.Timestamp = binary.BigEndian.Uint32(b[4:])
	p.SSRC = binary.BigEndian.Uint32(b[8:])
	n := HeaderLen + 4*cc
	if len(b) < n {
		return p, errShort
	}
	if ext {
		if len(b) < n+4 {
			return p, errShort
		}
		n += 4 + 4*int(binary.BigEndian.Uint16(b[n+2:]))
		if len(b) < n {
			return p, errShort
		}
	}
	end := len(b)
	if padding {
		pad := int(b[end-1])
		if pad == 0 || end-pad < n {
			return p, errPadding
		}
		end -= pad
	}
	p.Payload = b[n:end]
	return p, nil
}

// Less reports whether sequence number a comes before b, allowing for
// wrap-around (RFC 3550 appendix A.1's half-range rule).
func Less(a, b uint16) bool { return a != b && b-a < 0x8000 }
