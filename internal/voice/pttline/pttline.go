// Package pttline reads a PTT from what is not a CM108's pin: a serial
// port's modem-status line (CTS, DSR or DCD; a USB serial PTT cable, the
// AIOC's COM port), a GPIO line through the GPIO character device (a
// button on the board's header, to ground, with the line's pull-up), or a
// USB audio adapter's headset button (a phone-style PTT adapter). Each is
// polled: Pressed reports its state now.
package pttline

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Line is a PTT input.
type Line interface {
	Pressed() (bool, error)
	Close() error
}

// Open opens a PTT spec: serial:<device>:<cts|dsr|dcd> (device under
// /dev) or gpio:<chip>:<line>.
func Open(spec string) (Line, error) {
	kind, rest, _ := strings.Cut(spec, ":")
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return nil, fmt.Errorf("PTT %q: want %s:<device>:<line>", spec, kind)
	}
	dev, line := rest[:i], rest[i+1:]
	switch kind {
	case "serial":
		return openSerial(dev, line)
	case "gpio":
		n, err := strconv.Atoi(line)
		if err != nil || n < 0 || n > 511 {
			return nil, fmt.Errorf("PTT %q: line %q is not a number", spec, line)
		}
		return openGPIO(dev, n)
	}
	return nil, fmt.Errorf("PTT %q: not a serial or GPIO line", spec)
}

func devPath(name string) (string, error) {
	p := name
	if !strings.HasPrefix(p, "/") {
		p = "/dev/" + p
	}
	if !strings.HasPrefix(p, "/dev/") || strings.Contains(p, "..") {
		return "", fmt.Errorf("%q is not a device under /dev", name)
	}
	return p, nil
}

// Serial reads one modem-status line of a serial port (TIOCMGET): asserted
// is pressed.
type Serial struct {
	f   *os.File
	bit int
}

func openSerial(dev, line string) (*Serial, error) {
	bit := map[string]int{"cts": unix.TIOCM_CTS, "dsr": unix.TIOCM_DSR, "dcd": unix.TIOCM_CAR}[line]
	if bit == 0 {
		return nil, fmt.Errorf("serial PTT line %q: want cts, dsr or dcd", line)
	}
	p, err := devPath(dev)
	if err != nil {
		return nil, err
	}
	// O_NONBLOCK: opening must not wait for carrier.
	f, err := os.OpenFile(p, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return &Serial{f: f, bit: bit}, nil
}

func (s *Serial) Pressed() (bool, error) {
	v, err := unix.IoctlGetInt(int(s.f.Fd()), unix.TIOCMGET)
	if err != nil {
		return false, err
	}
	return v&s.bit != 0, nil
}

func (s *Serial) Close() error { return s.f.Close() }

// GPIO is one line requested as an input with its pull-up, active low
// (pressed pulls it to ground), through the v2 character device uAPI
// (Linux 5.10 and later).
type GPIO struct {
	fd int
}

// From linux/gpio.h.
const (
	gpioV2FlagActiveLow = 1 << 1
	gpioV2FlagInput     = 1 << 2
	gpioV2FlagPullUp    = 1 << 8
)

type gpioV2LineAttribute struct {
	ID      uint32
	Padding uint32
	Value   uint64
}

type gpioV2LineConfigAttribute struct {
	Attr gpioV2LineAttribute
	Mask uint64
}

type gpioV2LineConfig struct {
	Flags    uint64
	NumAttrs uint32
	Padding  [5]uint32
	Attrs    [10]gpioV2LineConfigAttribute
}

type gpioV2LineRequest struct {
	Offsets         [64]uint32
	Consumer        [32]byte
	Config          gpioV2LineConfig
	NumLines        uint32
	EventBufferSize uint32
	Padding         [5]uint32
	FD              int32
}

type gpioV2LineValues struct {
	Bits uint64
	Mask uint64
}

func iowr(nr, size uintptr) uintptr { return 3<<30 | size<<16 | 0xb4<<8 | nr }

var (
	gpioV2GetLine   = iowr(0x07, unsafe.Sizeof(gpioV2LineRequest{}))
	gpioV2GetValues = iowr(0x0e, unsafe.Sizeof(gpioV2LineValues{}))
)

func openGPIO(chip string, line int) (*GPIO, error) {
	p, err := devPath(chip)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var req gpioV2LineRequest
	req.Offsets[0] = uint32(line)
	copy(req.Consumer[:], "trellis-voiced ptt")
	req.Config.Flags = gpioV2FlagInput | gpioV2FlagPullUp | gpioV2FlagActiveLow
	req.NumLines = 1
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), gpioV2GetLine, uintptr(unsafe.Pointer(&req))); errno != 0 {
		return nil, fmt.Errorf("GPIO %s line %d: %w", chip, line, errno)
	}
	if req.FD <= 0 {
		return nil, errors.New("GPIO: no line descriptor")
	}
	return &GPIO{fd: int(req.FD)}, nil
}

func (g *GPIO) Pressed() (bool, error) {
	v := gpioV2LineValues{Mask: 1}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(g.fd), gpioV2GetValues, uintptr(unsafe.Pointer(&v))); errno != 0 {
		return false, errno
	}
	return v.Bits&1 != 0, nil
}

func (g *GPIO) Close() error { return unix.Close(g.fd) }

// Debounce turns a polled level into presses and releases: a new level
// counts once it has held for Stable. The first level read counts at once.
type Debounce struct {
	Stable    time.Duration
	pressed   bool
	candidate bool
	since     time.Time
	started   bool
}

// Update takes the level read at now and reports a change.
func (d *Debounce) Update(level bool, now time.Time) (changed, pressed bool) {
	switch {
	case !d.started:
		d.started, d.pressed, d.candidate = true, level, level
		return level, level
	case level != d.candidate:
		d.candidate, d.since = level, now
	case level != d.pressed && now.Sub(d.since) >= d.Stable:
		d.pressed = level
		return true, level
	}
	return false, d.pressed
}

// Output keys something: a bridged radio's PTT.
type Output interface {
	Set(on bool) error
	Close() error
}

// OpenOutput opens an output spec: serial:<device>:<rts|dtr> or
// gpio:<chip>:<line> (driven high when on). It starts off.
func OpenOutput(spec string) (Output, error) {
	kind, rest, _ := strings.Cut(spec, ":")
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return nil, fmt.Errorf("key %q: want %s:<device>:<line>", spec, kind)
	}
	dev, line := rest[:i], rest[i+1:]
	switch kind {
	case "serial":
		bit := map[string]int{"rts": unix.TIOCM_RTS, "dtr": unix.TIOCM_DTR}[line]
		if bit == 0 {
			return nil, fmt.Errorf("serial key line %q: want rts or dtr", line)
		}
		p, err := devPath(dev)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(p, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		o := &serialOut{f: f, bit: bit}
		// Opening a serial port raises both RTS and DTR. Both go down at
		// once: the one that keys, and the other, which some cables key on
		// too.
		if err := unix.IoctlSetPointerInt(int(f.Fd()), unix.TIOCMBIC, unix.TIOCM_RTS|unix.TIOCM_DTR); err != nil {
			f.Close()
			return nil, err
		}
		return o, nil
	case "gpio":
		n, err := strconv.Atoi(line)
		if err != nil || n < 0 || n > 511 {
			return nil, fmt.Errorf("key %q: line %q is not a number", spec, line)
		}
		return openGPIOOut(dev, n)
	}
	return nil, fmt.Errorf("key %q: not a serial or GPIO line", spec)
}

type serialOut struct {
	f   *os.File
	bit int
}

func (s *serialOut) Set(on bool) error {
	req := uint(unix.TIOCMBIC)
	if on {
		req = unix.TIOCMBIS
	}
	return unix.IoctlSetPointerInt(int(s.f.Fd()), req, s.bit)
}

func (s *serialOut) Close() error {
	_ = s.Set(false)
	return s.f.Close()
}

const gpioV2FlagOutput = 1 << 3

var gpioV2SetValues = iowr(0x0f, unsafe.Sizeof(gpioV2LineValues{}))

type gpioOut struct{ fd int }

func openGPIOOut(chip string, line int) (*gpioOut, error) {
	p, err := devPath(chip)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var req gpioV2LineRequest
	req.Offsets[0] = uint32(line)
	copy(req.Consumer[:], "trellis-voiced key")
	req.Config.Flags = gpioV2FlagOutput // starts low: off
	req.NumLines = 1
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), gpioV2GetLine, uintptr(unsafe.Pointer(&req))); errno != 0 {
		return nil, fmt.Errorf("GPIO %s line %d: %w", chip, line, errno)
	}
	if req.FD <= 0 {
		return nil, errors.New("GPIO: no line descriptor")
	}
	return &gpioOut{fd: int(req.FD)}, nil
}

func (g *gpioOut) Set(on bool) error {
	v := gpioV2LineValues{Mask: 1}
	if on {
		v.Bits = 1
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(g.fd), gpioV2SetValues, uintptr(unsafe.Pointer(&v))); errno != 0 {
		return errno
	}
	return nil
}

func (g *gpioOut) Close() error {
	_ = g.Set(false)
	return unix.Close(g.fd)
}

// Button reads a USB audio adapter's headset button through its input
// devices (/dev/input/eventN): a phone-style PTT adapter (3.5 mm to a
// Kenwood PTT, for instance) closes the microphone line when pressed,
// which the adapter reports as the headset's hook key (Play/Pause), held
// as long as the PTT. Its volume keys come out on Volume.
type Button struct {
	files []*os.File
	// down: per input device, whether one of its hook keys is held. The
	// button is pressed while any is.
	down   []atomic.Bool
	Volume chan int // +1 a volume-up press, -1 volume-down
	err    atomic.Pointer[error]
}

// The keys a headset button reports (linux/input-event-codes.h).
var hookKeys = map[uint16]bool{
	164: true, // KEY_PLAYPAUSE: the hook switch on most adapters
	226: true, // KEY_MEDIA
	169: true, // KEY_PHONE
	582: true, // KEY_VOICECOMMAND
	200: true, // KEY_PLAYCD
	201: true, // KEY_PAUSECD
}

const (
	keyVolumeDown = 114
	keyVolumeUp   = 115
	evKey         = 1
	eviocgrab     = 0x40044590 // _IOW('E', 0x90, int)
)

// inputEventSize is struct input_event: a timeval, then type, code, value.
var inputEventSize = int(unsafe.Sizeof(unix.Timeval{})) + 8

// OpenButton reads the adapter's input devices; each is taken for this
// process alone (EVIOCGRAB), so a press is not also a media key elsewhere.
func OpenButton(paths []string) (*Button, error) {
	if len(paths) == 0 {
		return nil, errors.New("the adapter has no input device: no headset button")
	}
	var files []*os.File
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			for _, x := range files {
				x.Close()
			}
			return nil, err
		}
		_ = unix.IoctlSetInt(int(f.Fd()), eviocgrab, 1)
		files = append(files, f)
	}
	return newButton(files), nil
}

func newButton(files []*os.File) *Button {
	b := &Button{Volume: make(chan int, 8), files: files, down: make([]atomic.Bool, len(files))}
	for dev, f := range files {
		go b.read(dev, f)
	}
	return b
}

func (b *Button) read(dev int, f *os.File) {
	buf := make([]byte, 64*inputEventSize)
	held := map[uint16]bool{}
	for {
		n, err := f.Read(buf)
		if err != nil {
			b.err.Store(&err)
			return
		}
		for i := 0; i+inputEventSize <= n; i += inputEventSize {
			ev := buf[i+inputEventSize-8:]
			typ, code := binary.LittleEndian.Uint16(ev), binary.LittleEndian.Uint16(ev[2:])
			val := int32(binary.LittleEndian.Uint32(ev[4:]))
			if typ != evKey || val == 2 { // 2: autorepeat
				continue
			}
			switch {
			case hookKeys[code]:
				held[code] = val == 1
				any := false
				for _, v := range held {
					any = any || v
				}
				b.down[dev].Store(any)
			case code == keyVolumeUp && val == 1:
				select {
				case b.Volume <- 1:
				default:
				}
			case code == keyVolumeDown && val == 1:
				select {
				case b.Volume <- -1:
				default:
				}
			}
		}
	}
}

// Pressed reports the button now; an input device that failed (the
// adapter unplugged) is an error.
func (b *Button) Pressed() (bool, error) {
	if e := b.err.Load(); e != nil {
		return false, *e
	}
	for i := range b.down {
		if b.down[i].Load() {
			return true, nil
		}
	}
	return false, nil
}

func (b *Button) Close() error {
	for _, f := range b.files {
		f.Close()
	}
	return nil
}
