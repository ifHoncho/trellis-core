package opus

import (
	"math"
	"testing"
)

const (
	rate  = 48000
	frame = rate / 50 // 20 ms
)

func need(t *testing.T) {
	t.Helper()
	if err := Load(); err != nil {
		t.Skip(err)
	}
}

func sine(n int, hz float64, at int) []int16 {
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(8000 * math.Sin(2*math.Pi*hz*float64(at+i)/rate))
	}
	return pcm
}

func rms(pcm []int16) float64 {
	var s float64
	for _, v := range pcm {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(pcm)))
}

// The settings voice uses: a tone survives the trip at about the
// bitrate asked for, a lost packet is concealed, and FEC fills one.
func TestRoundTrip(t *testing.T) {
	need(t)
	v, _ := Version()
	t.Log(v)
	enc, err := NewEncoder(EncoderConfig{SampleRate: rate, Channels: 1, Bitrate: 32000, Complexity: 5, InbandFEC: true, PacketLossPerc: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := NewDecoder(rate, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()

	const frames = 50
	var bytes int
	out := make([]int16, frame)
	buf := make([]byte, 1500)
	var packets [][]byte
	for f := 0; f < frames; f++ {
		n, err := enc.Encode(sine(frame, 440, f*frame), buf)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, append([]byte(nil), buf[:n]...))
		bytes += n
	}
	if kbps := float64(bytes*8) / 1000; kbps < 20 || kbps > 45 {
		t.Errorf("%.1f kbit/s for 32 asked", kbps)
	}
	for f, p := range packets {
		switch {
		case f == 30: // lost: concealed
			if n, err := dec.Decode(nil, out, false); err != nil || n != frame {
				t.Fatalf("concealment: %d %v", n, err)
			}
		case f == 40: // lost, its successor's FEC fills it
			if n, err := dec.Decode(packets[41], out, true); err != nil || n != frame || rms(out) < 1000 {
				t.Fatalf("FEC: %d %v rms %.0f", n, err, rms(out))
			}
		default:
			n, err := dec.Decode(p, out, false)
			if err != nil || n != frame {
				t.Fatalf("frame %d: %d %v", f, n, err)
			}
			// The frame after a loss is still recovering.
			if f > 5 && f != 31 && f != 41 && math.Abs(rms(out)-rms(sine(frame, 440, 0))) > 1500 {
				t.Errorf("frame %d: rms %.0f, sent %.0f", f, rms(out), rms(sine(frame, 440, 0)))
			}
		}
	}
	// The bitrate setting takes (opus_encoder_ctl is variadic in C).
	if err := enc.SetBitrate(12000); err != nil {
		t.Fatal(err)
	}
	bytes = 0
	for f := 0; f < frames; f++ {
		n, _ := enc.Encode(sine(frame, 440, f*frame), buf)
		bytes += n
	}
	if kbps := float64(bytes*8) / 1000; kbps > 16 {
		t.Errorf("%.1f kbit/s for 12 asked", kbps)
	}
	if _, err := dec.Decode([]byte{0xff, 0xff, 0xff}, out, false); err == nil {
		t.Error("a corrupt packet decoded without error")
	}
}
