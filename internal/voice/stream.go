package voice

import (
	"time"

	"meridian/trellis/internal/rtp"
)

// stream is one talker as heard on a group: a jitter buffer in front of
// the decoder. It holds what arrives for the buffer's time (target
// frames: counted from the first packet's arrival, or reached sooner by
// that many packets), then plays one frame per tick: the packet due, else
// the next packet's FEC, else Opus concealment. A run of missing frames
// with nothing buffered means the talk ended (or paused): playing stops
// and the next packet buffers again.
//
// The start is by time, not by a count of packets: packets lost at the
// start of a talk must not become delay for the rest of it.
type stream struct {
	ssrc    uint32
	dec     Decoder
	target  int
	buf     map[uint16][]byte
	due     uint16
	first   time.Time // when the oldest packet of this run arrived
	started bool
	heard   bool
	bye     bool

	lastArrival time.Time
	misses      int // frames missing in a row
	pending     int // missing with nothing buffered: lost only if more comes
	lost        int // since takeLost
}

func newStream(ssrc uint32, dec Decoder, target int) *stream {
	return &stream{ssrc: ssrc, dec: dec, target: max(target, 1), buf: map[uint16][]byte{}}
}

type putResult int

const (
	putOK putResult = iota
	putLate
	putDropped
)

func (s *stream) put(p rtp.Packet, now time.Time) putResult {
	s.lastArrival = now
	switch {
	case !s.started && len(s.buf) == 0:
		s.due, s.first = p.Seq, now
	case !s.started && rtp.Less(p.Seq, s.due):
		s.due = p.Seq
	case rtp.Less(p.Seq, s.due):
		return putLate
	}
	if p.Seq-s.due >= maxAhead {
		if s.started {
			return putDropped
		}
		// Not playing yet, and far past what is held: that is from before
		// a gap. The run starts over from this packet.
		clear(s.buf)
		s.due, s.first = p.Seq, now
	}
	if _, dup := s.buf[p.Seq]; dup {
		return putDropped
	}
	s.buf[p.Seq] = append([]byte(nil), p.Payload...)
	return putOK
}

// next decodes the frame due into pcm, or reports false when the stream is
// not playing.
func (s *stream) next(pcm []int16, now time.Time) bool {
	if !s.started {
		if len(s.buf) == 0 {
			return false
		}
		held := now.Sub(s.first) >= time.Duration(s.target)*FrameMs*time.Millisecond
		if len(s.buf) < s.target && !held && !s.bye {
			return false
		}
		s.started, s.misses = true, 0
	}
	if b, ok := s.buf[s.due]; ok {
		delete(s.buf, s.due)
		s.misses = 0
		s.lost += s.pending
		s.pending = 0
		s.decode(b, pcm, false)
		s.due++
		return true
	}
	s.misses++
	if len(s.buf) > 0 {
		s.lost++
	} else {
		s.pending++
		if s.misses >= underrunStop {
			s.started, s.pending = false, 0
			return false
		}
	}
	if b, ok := s.buf[s.due+1]; ok {
		s.decode(b, pcm, true)
	} else {
		s.decode(nil, pcm, false)
	}
	s.due++
	return true
}

// decode fills pcm from a packet (its FEC with fec; concealment for a nil
// packet, or when the packet does not decode). What the decoder did not
// fill is silence: a packet with a frame shorter than pcm (a sender that
// is not Trellis) must not leave what was in the buffer before.
func (s *stream) decode(b []byte, pcm []int16, fec bool) {
	n, err := s.dec.Decode(b, pcm, fec)
	if err != nil && b != nil {
		n, err = s.dec.Decode(nil, pcm, false)
	}
	if err != nil {
		n = 0
	}
	if n >= 0 && n < len(pcm) {
		clear(pcm[n:])
	}
}

func (s *stream) takeLost() int {
	n := s.lost
	s.lost = 0
	return n
}

// done reports a talker that said goodbye and has nothing left to play,
// or has gone quiet.
func (s *stream) done(now time.Time) bool {
	return (s.bye && len(s.buf) == 0) || now.Sub(s.lastArrival) > streamQuiet
}
