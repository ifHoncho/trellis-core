package voice

import (
	"math"
	"testing"
)

func tone(n int, hz, rate float64, at int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(8000 * math.Sin(2*math.Pi*hz*float64(at+i)/rate))
	}
	return out
}

func rmsOf(s []int16) float64 {
	var a float64
	for _, v := range s {
		a += float64(v) * float64(v)
	}
	return math.Sqrt(a / float64(len(s)))
}

// A 1 kHz tone keeps its level both ways; 10 kHz (above 16 kHz's Nyquist)
// is removed on the way down.
func TestResample(t *testing.T) {
	var d Downsampler
	out := make([]int16, ConsoleFrameLen)
	for f := 0; f < 10; f++ {
		d.Process(tone(FrameLen, 1000, SampleRate, f*FrameLen), out)
	}
	if r := rmsOf(out); math.Abs(r-5657) > 300 {
		t.Errorf("1 kHz down: rms %.0f, want about 5657", r)
	}
	var d2 Downsampler
	for f := 0; f < 10; f++ {
		d2.Process(tone(FrameLen, 10000, SampleRate, f*FrameLen), out)
	}
	if r := rmsOf(out); r > 300 {
		t.Errorf("10 kHz leaked: rms %.0f", r)
	}
	var u Upsampler
	up := make([]int16, FrameLen)
	for f := 0; f < 10; f++ {
		u.Process(tone(ConsoleFrameLen, 1000, ConsoleRate, f*ConsoleFrameLen), up)
	}
	if r := rmsOf(up); math.Abs(r-5657) > 300 {
		t.Errorf("1 kHz up: rms %.0f, want about 5657", r)
	}
}
