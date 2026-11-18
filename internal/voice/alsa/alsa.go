// Package alsa is the voice engine's audio on a node: capture and playback
// through the system's libasound, loaded at run time without cgo (like
// internal/opus, so only trellis-voiced imports it). Devices are opened by
// ALSA card id through the plug layer ("plughw:CARD=<id>"), which converts
// rate and channels when the card cannot do 48 kHz mono capture or stereo
// playback itself.
package alsa

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

const libName = "libasound.so.2"

// From alsa/pcm.h.
const (
	streamPlayback      = 0
	streamCapture       = 1
	formatS16LE         = 2
	accessRWInterleaved = 3
)

var lib struct {
	once sync.Once
	err  error

	open      func(pcm *uintptr, name string, stream, mode int32) int32
	setParams func(pcm uintptr, format, access int32, channels, rate uint32, softResample int32, latencyUS uint32) int32
	readi     func(pcm uintptr, buf unsafe.Pointer, frames uint64) int64
	writei    func(pcm uintptr, buf unsafe.Pointer, frames uint64) int64
	recover   func(pcm uintptr, err int32, silent int32) int32
	start     func(pcm uintptr) int32
	close     func(pcm uintptr) int32
	strerror  func(err int32) string
}

// Load loads libasound.
func Load() error {
	lib.once.Do(func() {
		h, err := purego.Dlopen(libName, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			lib.err = fmt.Errorf("cannot load %s (install the libasound2 package): %w", libName, err)
			return
		}
		for name, fn := range map[string]any{
			"snd_pcm_open":       &lib.open,
			"snd_pcm_set_params": &lib.setParams,
			"snd_pcm_readi":      &lib.readi,
			"snd_pcm_writei":     &lib.writei,
			"snd_pcm_recover":    &lib.recover,
			"snd_pcm_start":      &lib.start,
			"snd_pcm_close":      &lib.close,
			"snd_strerror":       &lib.strerror,
		} {
			sym, err := purego.Dlsym(h, name)
			if err != nil {
				lib.err = fmt.Errorf("%s has no %s: %w", libName, name, err)
				return
			}
			purego.RegisterFunc(fn, sym)
		}
	})
	return lib.err
}

func alsaErr(op string, code int32) error {
	return fmt.Errorf("alsa %s: %s", op, lib.strerror(code))
}

// PCM is an open capture or playback stream.
type PCM struct {
	h        uintptr
	channels int
	op       string
	capture  bool
}

func open(card string, stream int32, channels, rate int, latencyUS int) (*PCM, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	name := "plughw:CARD=" + card
	var h uintptr
	if rc := lib.open(&h, name, stream, 0); rc < 0 {
		return nil, alsaErr("open "+name, rc)
	}
	if rc := lib.setParams(h, formatS16LE, accessRWInterleaved, uint32(channels), uint32(rate), 1, uint32(latencyUS)); rc < 0 {
		lib.close(h)
		return nil, alsaErr("set parameters on "+name, rc)
	}
	p := &PCM{h: h, channels: channels, op: "playback on " + name, capture: stream == streamCapture}
	if p.capture {
		// A read does not start capture by itself with these parameters
		// (it fails with EIO): start it.
		p.op = "capture on " + name
		if rc := lib.start(h); rc < 0 {
			lib.close(h)
			return nil, alsaErr("start "+p.op, rc)
		}
	}
	return p, nil
}

// OpenCapture opens a card's microphone: mono, 16-bit, at rate.
func OpenCapture(card string, rate, latencyUS int) (*PCM, error) {
	return open(card, streamCapture, 1, rate, latencyUS)
}

// OpenPlayback opens a card's output: stereo, 16-bit, at rate.
func OpenPlayback(card string, rate, latencyUS int) (*PCM, error) {
	return open(card, streamPlayback, 2, rate, latencyUS)
}

// Read fills frame (interleaved samples), blocking until it is full. An
// overrun is recovered from and reading goes on.
func (p *PCM) Read(frame []int16) error {
	return p.transfer(frame, lib.readi)
}

// Write plays frame (interleaved), blocking while the device's buffer is
// full. An underrun is recovered from.
func (p *PCM) Write(frame []int16) error {
	return p.transfer(frame, lib.writei)
}

func (p *PCM) transfer(frame []int16, fn func(uintptr, unsafe.Pointer, uint64) int64) error {
	if p.h == 0 {
		return errors.New("alsa: closed")
	}
	frames := len(frame) / p.channels
	var pin runtime.Pinner
	defer pin.Unpin()
	pin.Pin(&frame[0])
	for done := 0; done < frames; {
		n := fn(p.h, unsafe.Pointer(&frame[done*p.channels]), uint64(frames-done))
		switch {
		case n >= 0:
			done += int(n)
		case n == -int64(syscall.EAGAIN):
		default:
			if rc := lib.recover(p.h, int32(n), 1); rc < 0 {
				return alsaErr(p.op, int32(n))
			}
			if p.capture {
				lib.start(p.h)
			}
		}
	}
	return nil
}

// Close closes the stream.
func (p *PCM) Close() error {
	if p.h == 0 {
		return nil
	}
	rc := lib.close(p.h)
	p.h = 0
	if rc < 0 {
		return alsaErr("close", rc)
	}
	return nil
}
