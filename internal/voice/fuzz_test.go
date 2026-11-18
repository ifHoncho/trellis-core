package voice

import (
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"meridian/trellis/internal/rtp"
)

// FuzzDeliver feeds packets from the network to a listening, playing
// engine: nothing may panic, and the jitter buffer stays bounded.
func FuzzDeliver(f *testing.F) {
	p := rtp.Packet{PayloadType: PayloadType, Seq: 1, SSRC: 9, Payload: []byte{0, 1, 0, 0}}
	f.Add(p.Append(nil), false)
	f.Add(rtp.AppendCompound(nil, rtp.SenderReport{SSRC: 9}, "x", true), true)
	f.Fuzz(func(t *testing.T, b []byte, rtcp bool) {
		bs := &bus{now: time.Unix(1000, 0)}
		var self *Engine
		e, err := New(cfg("x", Channel{Group: 1, Volume: 100}, Channel{}), fakeCodec{}, busPort{bs, &self}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		self = e
		e.now = func() time.Time { return bs.now }
		to := DefaultAddr(1)
		if rtcp {
			to = netip.AddrPortFrom(to.Addr(), to.Port()+1)
		}
		stereo := make([]int16, 2*FrameLen)
		for i := 0; i < 4; i++ {
			e.Deliver(to, b)
			bs.now = bs.now.Add(FrameMs * time.Millisecond)
			e.tick(make([]int16, FrameLen), stereo, make([]byte, 1500))
		}
		if s := e.groups[1].talker; s != nil && len(s.buf) > maxAhead {
			t.Fatalf("%d frames buffered", len(s.buf))
		}
	})
}
