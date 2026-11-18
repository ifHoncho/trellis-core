package alsa

import (
	"fmt"
	"strings"
	"sync"

	"github.com/ebitengine/purego"
)

// The card's own levels. Many USB adapters start their headphone output
// well below full (a KT0211 at -20 dB) and some their microphone low; the
// engine sets its levels itself, so the card's are set to unity:
//   - outputs (Master, Headphone, Headset, Speaker, PCM, Line Out): 0 dB,
//     or their top when that is lower, and on;
//   - paths that play an input straight to the output (a CM108's "Mic
//     Playback", "Line Playback", any monitor): off; the engine has its
//     own sidetone;
//   - the card's own automatic gain (a CM108's "Auto Gain Control", which
//     ALSA shows as a playback switch): off; the engine has its own AGC,
//     and two in a row pump;
//   - any other playback control (tone controls, a name Trellis does not
//     know): left as it is. Switching off what is not known by name could
//     silence the card;
//   - capture: raised to 0 dB when below (never lowered: a CM108's mic can
//     sit above 0 dB on purpose), and on.

var mx struct {
	once sync.Once
	err  error

	open       func(h *uintptr, mode int32) int32
	attach     func(h uintptr, name string) int32
	register   func(h uintptr, opt, classp uintptr) int32
	load       func(h uintptr) int32
	close      func(h uintptr) int32
	first      func(h uintptr) uintptr
	next       func(e uintptr) uintptr
	name       func(e uintptr) string
	hasPVol    func(e uintptr) int32
	hasCVol    func(e uintptr) int32
	hasPSw     func(e uintptr) int32
	hasCSw     func(e uintptr) int32
	pRange     func(e uintptr, min, max *int64) int32
	cRange     func(e uintptr, min, max *int64) int32
	getC       func(e uintptr, ch int32, v *int64) int32
	setP       func(e uintptr, v int64, dir int32) int32
	setC       func(e uintptr, v int64, dir int32) int32
	pVolRange  func(e uintptr, min, max *int64) int32
	setPVolAll func(e uintptr, v int64) int32
	setPSw     func(e uintptr, on int32) int32
	setCSw     func(e uintptr, on int32) int32
}

func loadMixer() error {
	mx.once.Do(func() {
		if mx.err = Load(); mx.err != nil {
			return
		}
		h, err := purego.Dlopen(libName, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			mx.err = err
			return
		}
		for name, fn := range map[string]any{
			"snd_mixer_open": &mx.open, "snd_mixer_attach": &mx.attach, "snd_mixer_selem_register": &mx.register,
			"snd_mixer_load": &mx.load, "snd_mixer_close": &mx.close,
			"snd_mixer_first_elem": &mx.first, "snd_mixer_elem_next": &mx.next,
			"snd_mixer_selem_get_name":                  &mx.name,
			"snd_mixer_selem_has_playback_volume":       &mx.hasPVol,
			"snd_mixer_selem_has_capture_volume":        &mx.hasCVol,
			"snd_mixer_selem_has_playback_switch":       &mx.hasPSw,
			"snd_mixer_selem_has_capture_switch":        &mx.hasCSw,
			"snd_mixer_selem_get_playback_dB_range":     &mx.pRange,
			"snd_mixer_selem_get_capture_dB_range":      &mx.cRange,
			"snd_mixer_selem_get_capture_dB":            &mx.getC,
			"snd_mixer_selem_set_playback_dB_all":       &mx.setP,
			"snd_mixer_selem_set_capture_dB_all":        &mx.setC,
			"snd_mixer_selem_get_playback_volume_range": &mx.pVolRange,
			"snd_mixer_selem_set_playback_volume_all":   &mx.setPVolAll,
			"snd_mixer_selem_set_playback_switch_all":   &mx.setPSw,
			"snd_mixer_selem_set_capture_switch_all":    &mx.setCSw,
		} {
			sym, err := purego.Dlsym(h, name)
			if err != nil {
				mx.err = fmt.Errorf("%s has no %s: %w", libName, name, err)
				return
			}
			purego.RegisterFunc(fn, sym)
		}
	})
	return mx.err
}

// Level is one control as Unity left it.
type Level struct {
	Control string  `json:"control"`
	Kind    string  `json:"kind"` // playback, capture, monitor off, or off (the card's AGC)
	DB      float64 `json:"db"`   // what it is now set to
}

// Unity sets a card's mixer as above and reports what it set; with dry,
// it sets nothing and reports what it would.
func Unity(card string, dry bool) ([]Level, error) {
	if err := loadMixer(); err != nil {
		return nil, err
	}
	var h uintptr
	if rc := mx.open(&h, 0); rc < 0 {
		return nil, alsaErr("mixer open", rc)
	}
	defer mx.close(h)
	if rc := mx.attach(h, "hw:CARD="+card); rc < 0 {
		return nil, alsaErr("mixer attach hw:CARD="+card, rc)
	}
	if rc := mx.register(h, 0, 0); rc < 0 {
		return nil, alsaErr("mixer register", rc)
	}
	if rc := mx.load(h); rc < 0 {
		return nil, alsaErr("mixer load", rc)
	}
	var out []Level
	for e := mx.first(h); e != 0; e = mx.next(e) {
		name := mx.name(e)
		if mx.hasPVol(e) != 0 || mx.hasPSw(e) != 0 {
			switch playbackKind(name) {
			case monitor:
				if mx.hasPSw(e) != 0 {
					if !dry {
						mx.setPSw(e, 0)
					}
					out = append(out, Level{Control: name, Kind: "monitor off"})
				}
				goto capture
			case cardAGC:
				if mx.hasPSw(e) != 0 {
					if !dry {
						mx.setPSw(e, 0)
					}
					out = append(out, Level{Control: name, Kind: "off"})
				}
				goto capture
			case unknown:
				goto capture
			}
		}
		if mx.hasPVol(e) != 0 {
			var lo, hi int64
			target := int64(0) // 0 dB, in hundredths
			if mx.pRange(e, &lo, &hi) == 0 && hi > lo {
				target = min(target, hi)
				if !dry {
					mx.setP(e, target, 1)
				}
			} else if mx.pVolRange(e, &lo, &hi) == 0 && !dry {
				mx.setPVolAll(e, hi) // no dB scale: the top
			}
			out = append(out, Level{Control: name, Kind: "playback", DB: float64(target) / 100})
		}
		if mx.hasPSw(e) != 0 && !dry {
			mx.setPSw(e, 1)
		}
	capture:
		if mx.hasCVol(e) != 0 {
			var lo, hi, cur int64
			if mx.cRange(e, &lo, &hi) == 0 && hi > lo && mx.getC(e, 0, &cur) == 0 {
				if cur < 0 && hi >= 0 {
					if !dry {
						mx.setC(e, 0, 1)
					}
					cur = 0
				}
				out = append(out, Level{Control: name, Kind: "capture", DB: float64(cur) / 100})
			}
		}
		if mx.hasCSw(e) != 0 && !dry {
			mx.setCSw(e, 1)
		}
	}
	return out, nil
}

// What a playback control is, by its name.
const (
	unknown = iota // not known: left alone
	output         // sets what the card plays
	monitor        // plays an input straight to the output
	cardAGC        // the card's own automatic gain
)

// playbackKind sorts a playback control by its name. An input's name
// wins ("Front Mic" is a monitor, though "Front" is an output).
func playbackKind(name string) int {
	n := strings.ToLower(name)
	if strings.Contains(n, "auto gain") || strings.Contains(n, "agc") {
		return cardAGC
	}
	for _, in := range []string{"mic", "line in", "aux", "capture", "loopback", "monitor", "sidetone", "cd", "video"} {
		if strings.Contains(n, in) {
			return monitor
		}
	}
	for _, o := range []string{"master", "headphone", "headset", "speaker", "pcm", "line out", "front", "earpiece", "digital", "playback", "analog", "dac", "output"} {
		if strings.Contains(n, o) {
			return output
		}
	}
	return unknown
}
