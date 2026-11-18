// Package wav is the voice engine's file audio, for the lab and tests: a
// capture that plays a WAV file (or a tone, or silence) at real time, and
// a playback that records to a WAV file. Format: 16-bit PCM, 48 kHz.
package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"meridian/trellis/internal/voice"
)

// Capture plays mono samples one frame per FrameMs, in real time, then
// silence. It is the engine's clock in the lab.
type Capture struct {
	samples []int16
	pos     int
	next    time.Time
}

// ToneCapture is an endless sine of hz at about -10 dBFS.
func ToneCapture(hz float64, seconds int) *Capture {
	s := make([]int16, voice.SampleRate*seconds)
	for i := range s {
		s[i] = int16(10000 * math.Sin(2*math.Pi*hz*float64(i)/voice.SampleRate))
	}
	return &Capture{samples: s}
}

// SilenceCapture is silence.
func SilenceCapture() *Capture { return &Capture{} }

// FileCapture reads a mono 48 kHz 16-bit WAV file.
func FileCapture(path string) (*Capture, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ch, rate, data, err := read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if ch != 1 || rate != voice.SampleRate {
		return nil, fmt.Errorf("%s: %d channels at %d Hz; need mono at %d Hz", path, ch, rate, voice.SampleRate)
	}
	return &Capture{samples: data}, nil
}

func (c *Capture) Read(frame []int16) error {
	now := time.Now()
	if c.next.IsZero() {
		c.next = now
	}
	c.next = c.next.Add(voice.FrameMs * time.Millisecond)
	if d := c.next.Sub(now); d > 0 {
		time.Sleep(d)
	} else if d < -time.Second {
		c.next = now // fell far behind (a suspended process): start over
	}
	n := 0
	if c.pos < len(c.samples) {
		n = copy(frame, c.samples[c.pos:])
		c.pos += n
		if c.pos >= len(c.samples) && len(c.samples) > 0 {
			c.pos = 0 // loop
		}
	}
	clear(frame[n:])
	return nil
}

// Recorder writes stereo frames to a WAV file; Close finishes the header.
type Recorder struct {
	mu sync.Mutex
	f  *os.File
	n  int
}

func NewRecorder(path string) (*Recorder, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if err := writeHeader(f, 2, 0); err != nil {
		f.Close()
		return nil, err
	}
	return &Recorder{f: f}, nil
}

func (r *Recorder) Write(frame []int16) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return errors.New("recorder closed")
	}
	if err := binary.Write(r.f, binary.LittleEndian, frame); err != nil {
		return err
	}
	r.n += len(frame)
	// Keep the header current, so a killed process leaves a readable file.
	if _, err := r.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := writeHeader(r.f, 2, r.n*2); err != nil {
		return err
	}
	_, err := r.f.Seek(0, io.SeekEnd)
	return err
}

func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// Discard is a playback that drops everything.
type Discard struct{}

func (Discard) Write([]int16) error { return nil }

func writeHeader(w io.Writer, channels, dataLen int) error {
	h := make([]byte, 0, 44)
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(36+dataLen))
	h = append(h, "WAVEfmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16)
	h = binary.LittleEndian.AppendUint16(h, 1) // PCM
	h = binary.LittleEndian.AppendUint16(h, uint16(channels))
	h = binary.LittleEndian.AppendUint32(h, voice.SampleRate)
	h = binary.LittleEndian.AppendUint32(h, uint32(voice.SampleRate*2*channels))
	h = binary.LittleEndian.AppendUint16(h, uint16(2*channels))
	h = binary.LittleEndian.AppendUint16(h, 16)
	h = append(h, "data"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(dataLen))
	_, err := w.Write(h)
	return err
}

// Read reads a 16-bit PCM WAV file: channels, sample rate and the
// interleaved samples.
func Read(path string) (int, int, []int16, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, nil, err
	}
	defer f.Close()
	return read(f)
}

func read(r io.Reader) (channels, rate int, data []int16, err error) {
	b, err := io.ReadAll(io.LimitReader(r, 1<<30))
	if err != nil {
		return 0, 0, nil, err
	}
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, 0, nil, errors.New("not a WAV file")
	}
	bits := 0
	for p := 12; p+8 <= len(b); {
		id, n := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		body := b[p+8:]
		if n > len(body) {
			n = len(body) // a header a killed writer left short
		}
		switch id {
		case "fmt ":
			if n < 16 || binary.LittleEndian.Uint16(body) != 1 {
				return 0, 0, nil, errors.New("not 16-bit PCM")
			}
			channels = int(binary.LittleEndian.Uint16(body[2:]))
			rate = int(binary.LittleEndian.Uint32(body[4:]))
			bits = int(binary.LittleEndian.Uint16(body[14:]))
		case "data":
			if bits != 16 {
				return 0, 0, nil, errors.New("not 16-bit PCM")
			}
			data = make([]int16, n/2)
			_ = binary.Read(bytes.NewReader(body[:n/2*2]), binary.LittleEndian, data)
			return channels, rate, data, nil
		}
		p += 8 + n + n%2
	}
	return 0, 0, nil, errors.New("no data chunk")
}
