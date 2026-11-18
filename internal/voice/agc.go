package voice

import "math"

// agc is the microphone's automatic gain: speech is brought to about
// agcTarget whatever the microphone (a radio speaker-mic puts out far less
// than a headset), by up to agcMax; quieter than agcFloor is noise and is
// not raised. It lowers its gain at once on a loud frame and raises it
// slowly (about 6 dB a second), so it does not pump between words.
type agc struct {
	gain float64 // linear
}

const (
	agcTarget = 0.125   // -18 dBFS RMS
	agcFloor  = 0.008   // -42 dBFS: below this (room noise, hum), the gain holds
	agcMax    = 31.6    // +30 dB
	agcRise   = 1.0139  // a frame: +0.12 dB, 6 dB a second
	agcLimit  = 0.89    // peaks above -1 dBFS are limited
	fullScale = 32768.0 // int16
)

// process scales a frame in place.
func (a *agc) process(f []int16) {
	if a.gain == 0 {
		a.gain = 1
	}
	var s float64
	for _, v := range f {
		x := float64(v) / fullScale
		s += x * x
	}
	rms := math.Sqrt(s / float64(len(f)))
	if rms > agcFloor {
		want := math.Min(agcMax, math.Max(1, agcTarget/rms))
		if want < a.gain {
			a.gain = want // down at once
		} else {
			a.gain = math.Min(want, a.gain*agcRise)
		}
	}
	for i, v := range f {
		x := float64(v) / fullScale * a.gain
		if x > agcLimit {
			x = agcLimit
		} else if x < -agcLimit {
			x = -agcLimit
		}
		f[i] = int16(x * fullScale)
	}
}

// dB is the gain now.
func (a *agc) dB() float64 {
	if a.gain <= 0 {
		return 0
	}
	return math.Round(20*math.Log10(a.gain)*10) / 10
}
