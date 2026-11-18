package voice

import (
	"math"
	"time"
)

// toneStep is a tone (hz > 0) or a pause, for ms milliseconds.
type toneStep struct{ hz, ms int }

// The local tones. They are heard only on this node, in the channel's
// ear.
var (
	toneGrant   = []toneStep{{1200, 40}, {0, 20}, {1200, 40}}
	toneRelease = []toneStep{{700, 60}}
	toneBusy    = []toneStep{{425, 120}, {0, 80}, {425, 120}, {0, 80}, {425, 120}}
	toneVolume  = []toneStep{{900, 30}}
	toneTest    = []toneStep{{1000, 1000}}
	// The talk was stopped: the microphone is silent while the PTT is
	// held. Low, long-short-long, unlike any other tone.
	toneMicCut = []toneStep{{330, 300}, {0, 100}, {330, 100}, {0, 100}, {330, 300}}
	// Flash override: a priority group's talk begins.
	toneOverride = []toneStep{{1400, 60}, {0, 40}, {1400, 60}, {0, 40}, {1400, 60}}
)

const toneLevel = 0.2 * math.MaxInt16

// tonePlayer plays one sequence of steps, a frame at a time.
type tonePlayer struct {
	samples []int16
}

func (t *tonePlayer) start(steps []toneStep) {
	t.samples = t.samples[:0]
	for _, s := range steps {
		n := SampleRate * s.ms / 1000
		ramp := SampleRate / 200 // 5 ms in and out, no clicks
		for i := 0; i < n; i++ {
			var v float64
			if s.hz > 0 {
				env := math.Min(1, math.Min(float64(i), float64(n-1-i))/float64(ramp))
				v = toneLevel * env * math.Sin(2*math.Pi*float64(s.hz)*float64(i)/SampleRate)
			}
			t.samples = append(t.samples, int16(v))
		}
	}
}

// next returns the next frame of the tone, or nil when none is playing.
func (t *tonePlayer) next() []int16 {
	if len(t.samples) == 0 {
		return nil
	}
	f := make([]int16, FrameLen)
	n := copy(f, t.samples)
	t.samples = t.samples[n:]
	return f
}

// level is a signal's peak RMS over the last 200 ms, in frames.
type level struct {
	peak float64
	at   time.Time
}

func (l *level) add(f []int16, now time.Time) {
	var r float64
	if len(f) > 0 {
		var s float64
		for _, v := range f {
			s += float64(v) * float64(v)
		}
		r = math.Sqrt(s / float64(len(f)))
	}
	if now.Sub(l.at) > 200*time.Millisecond || r > l.peak {
		l.peak, l.at = r, now
	}
}

// dbfs is the level against full scale: -96 for silence.
func (l *level) dbfs() float64 {
	if l.peak < 1 {
		return -96
	}
	return math.Round(20*math.Log10(l.peak/32768)*10) / 10
}
