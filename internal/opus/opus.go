// Package opus encodes and decodes Opus voice with the system's libopus,
// loaded when first used. It needs no C toolchain to build, but a binary
// that imports it is linked dynamically (against the C library), so only
// trellis-voiced imports it; the trellis binary stays static.
//
// An Encoder or Decoder is not safe for concurrent use.
package opus

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libName is libopus's soname on Debian-family systems (package libopus0).
const libName = "libopus.so.0"

// From opus_defines.h: the public API's constants.
const (
	appVoIP = 2048

	ctlSetBitrate        = 4002
	ctlSetComplexity     = 4010
	ctlSetInbandFEC      = 4012
	ctlSetPacketLossPerc = 4014
	ctlSetDTX            = 4016
	ctlSetSignal         = 4024
	signalVoice          = 3001
)

var lib struct {
	once sync.Once
	err  error

	version       func() string
	strerror      func(code int32) string
	encoderCreate func(fs int32, channels int32, application int32, err *int32) uintptr
	encoderCtl    func(st uintptr, request int32, value int32) int32
	encode        func(st uintptr, pcm unsafe.Pointer, frameSize int32, data unsafe.Pointer, maxBytes int32) int32
	encoderFree   func(st uintptr)
	decoderCreate func(fs int32, channels int32, err *int32) uintptr
	decode        func(st uintptr, data unsafe.Pointer, length int32, pcm unsafe.Pointer, frameSize int32, decodeFEC int32) int32
	decoderFree   func(st uintptr)
}

// Load loads libopus. Every other call loads it too; Load reports, before
// anything else is set up, whether it can be.
func Load() error {
	lib.once.Do(func() {
		h, err := purego.Dlopen(libName, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			lib.err = fmt.Errorf("cannot load %s (install the libopus0 package): %w", libName, err)
			return
		}
		for name, fn := range map[string]any{
			"opus_get_version_string": &lib.version,
			"opus_strerror":           &lib.strerror,
			"opus_encoder_create":     &lib.encoderCreate,
			"opus_encoder_ctl":        &lib.encoderCtl, // variadic in C; one int argument passes the same way on amd64 and arm64
			"opus_encode":             &lib.encode,
			"opus_encoder_destroy":    &lib.encoderFree,
			"opus_decoder_create":     &lib.decoderCreate,
			"opus_decode":             &lib.decode,
			"opus_decoder_destroy":    &lib.decoderFree,
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

// Version is libopus's version string, such as "libopus 1.4".
func Version() (string, error) {
	if err := Load(); err != nil {
		return "", err
	}
	return lib.version(), nil
}

func opusErr(op string, code int32) error {
	return fmt.Errorf("opus %s: %s (%d)", op, lib.strerror(code), code)
}

// EncoderConfig sets up an encoder for voice.
type EncoderConfig struct {
	SampleRate     int  // 8000, 12000, 16000, 24000 or 48000
	Channels       int  // 1 or 2
	Bitrate        int  // bits per second
	Complexity     int  // 0–10
	InbandFEC      bool // redundancy a receiver can use to fill a lost packet
	PacketLossPerc int  // expected loss, 0–100: how much redundancy FEC spends
	DTX            bool // send almost nothing in silence
}

// Encoder is an Opus encoder in VoIP mode.
type Encoder struct {
	st       uintptr
	channels int
}

// NewEncoder makes an encoder. Close frees it.
func NewEncoder(c EncoderConfig) (*Encoder, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	var code int32
	st := lib.encoderCreate(int32(c.SampleRate), int32(c.Channels), appVoIP, &code)
	if code != 0 || st == 0 {
		return nil, opusErr("encoder", code)
	}
	e := &Encoder{st: st, channels: c.Channels}
	set := []struct {
		name     string
		req, val int32
	}{
		{"signal", ctlSetSignal, signalVoice},
		{"bitrate", ctlSetBitrate, int32(c.Bitrate)},
		{"complexity", ctlSetComplexity, int32(c.Complexity)},
		{"in-band FEC", ctlSetInbandFEC, b2i(c.InbandFEC)},
		{"packet loss", ctlSetPacketLossPerc, int32(c.PacketLossPerc)},
		{"DTX", ctlSetDTX, b2i(c.DTX)},
	}
	for _, s := range set {
		if err := e.ctl(s.name, s.req, s.val); err != nil {
			e.Close()
			return nil, err
		}
	}
	return e, nil
}

func b2i(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

func (e *Encoder) ctl(name string, req, val int32) error {
	if code := lib.encoderCtl(e.st, req, val); code != 0 {
		return opusErr("set "+name, code)
	}
	return nil
}

// SetPacketLossPerc changes the expected loss while encoding.
func (e *Encoder) SetPacketLossPerc(p int) error {
	return e.ctl("packet loss", ctlSetPacketLossPerc, int32(p))
}

// SetBitrate changes the bitrate while encoding.
func (e *Encoder) SetBitrate(bps int) error { return e.ctl("bitrate", ctlSetBitrate, int32(bps)) }

// Encode encodes one frame of interleaved samples (2.5 to 120 ms of them)
// into out, and returns the packet's length.
func (e *Encoder) Encode(pcm []int16, out []byte) (int, error) {
	if e.st == 0 {
		return 0, errors.New("opus: encoder closed")
	}
	if len(pcm) == 0 || len(pcm)%e.channels != 0 || len(out) == 0 {
		return 0, errors.New("opus encode: empty or uneven buffer")
	}
	var p runtime.Pinner
	p.Pin(&pcm[0])
	p.Pin(&out[0])
	n := lib.encode(e.st, unsafe.Pointer(&pcm[0]), int32(len(pcm)/e.channels), unsafe.Pointer(&out[0]), int32(len(out)))
	p.Unpin()
	if n < 0 {
		return 0, opusErr("encode", n)
	}
	return int(n), nil
}

// Close frees the encoder.
func (e *Encoder) Close() {
	if e.st != 0 {
		lib.encoderFree(e.st)
		e.st = 0
	}
}

// Decoder is an Opus decoder.
type Decoder struct {
	st       uintptr
	channels int
}

// NewDecoder makes a decoder. Close frees it.
func NewDecoder(sampleRate, channels int) (*Decoder, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	var code int32
	st := lib.decoderCreate(int32(sampleRate), int32(channels), &code)
	if code != 0 || st == 0 {
		return nil, opusErr("decoder", code)
	}
	return &Decoder{st: st, channels: channels}, nil
}

// Decode decodes a packet into pcm, interleaved, and returns the samples
// per channel. With fec, it decodes the redundancy the packet carries for
// the one before it (lost), not the packet itself; pcm must then be
// exactly the lost frame's length. A nil packet conceals a lost one for
// len(pcm) samples.
func (d *Decoder) Decode(packet []byte, pcm []int16, fec bool) (int, error) {
	if d.st == 0 {
		return 0, errors.New("opus: decoder closed")
	}
	if len(pcm) == 0 || len(pcm)%d.channels != 0 {
		return 0, errors.New("opus decode: empty or uneven buffer")
	}
	var p runtime.Pinner
	defer p.Unpin()
	p.Pin(&pcm[0])
	var data unsafe.Pointer
	if len(packet) > 0 {
		p.Pin(&packet[0])
		data = unsafe.Pointer(&packet[0])
	}
	n := lib.decode(d.st, data, int32(len(packet)), unsafe.Pointer(&pcm[0]), int32(len(pcm)/d.channels), b2i(fec))
	if n < 0 {
		return 0, opusErr("decode", n)
	}
	return int(n), nil
}

// Close frees the decoder.
func (d *Decoder) Close() {
	if d.st != 0 {
		lib.decoderFree(d.st)
		d.st = 0
	}
}
