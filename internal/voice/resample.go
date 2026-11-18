package voice

import "math"

// Console audio is 16 kHz mono: a third of the engine's 48 kHz. These are
// the two directions, each a windowed-sinc low-pass FIR (cutoff 7 kHz)
// that keeps its history across frames.

const (
	ConsoleRate     = 16000
	ConsoleFrameLen = ConsoleRate * FrameMs / 1000 // 320 samples
	ratio           = SampleRate / ConsoleRate     // 3
	firTaps         = 47
)

var fir = func() []float32 {
	h := make([]float32, firTaps)
	fc := 7000.0 / SampleRate // normalised cutoff
	mid := float64(firTaps-1) / 2
	var sum float64
	for i := range h {
		x := float64(i) - mid
		s := 2 * fc
		if x != 0 {
			s = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		w := 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(firTaps-1)) // Hamming
		h[i] = float32(s * w)
		sum += s * w
	}
	for i := range h {
		h[i] /= float32(sum) // unity gain at DC
	}
	return h
}()

// Downsampler takes 48 kHz frames to 16 kHz.
type Downsampler struct{ hist []float32 }

// Process filters in (FrameLen samples at 48 kHz) into out
// (ConsoleFrameLen samples).
func (d *Downsampler) Process(in, out []int16) {
	if d.hist == nil {
		d.hist = make([]float32, firTaps-1)
	}
	buf := make([]float32, len(d.hist)+len(in))
	copy(buf, d.hist)
	for i, v := range in {
		buf[len(d.hist)+i] = float32(v)
	}
	for o := range out {
		end := len(d.hist) + o*ratio // the newest input sample for this output
		var acc float32
		for k := 0; k < firTaps; k++ {
			acc += fir[k] * buf[end-k]
		}
		out[o] = clip(acc)
	}
	copy(d.hist, buf[len(buf)-len(d.hist):])
}

// Upsampler takes 16 kHz frames to 48 kHz.
type Upsampler struct{ hist []float32 }

// Process interpolates in (ConsoleFrameLen at 16 kHz) into out (FrameLen
// at 48 kHz): zeros between samples, then the low-pass (gain 3).
func (u *Upsampler) Process(in, out []int16) {
	if u.hist == nil {
		u.hist = make([]float32, firTaps-1)
	}
	buf := make([]float32, len(u.hist)+len(out))
	copy(buf, u.hist)
	for i, v := range in {
		buf[len(u.hist)+i*ratio] = float32(v)
	}
	for o := range out {
		end := len(u.hist) + o
		var acc float32
		for k := 0; k < firTaps; k++ {
			acc += fir[k] * buf[end-k]
		}
		out[o] = clip(acc * ratio)
	}
	copy(u.hist, buf[len(buf)-len(u.hist):])
}
