package pttline

import (
	"encoding/binary"
	"os"
	"testing"
	"time"
	"unsafe"
)

// The uAPI structures and ioctl numbers as linux/gpio.h has them.
func TestGPIOABI(t *testing.T) {
	if s := unsafe.Sizeof(gpioV2LineRequest{}); s != 592 {
		t.Errorf("gpio_v2_line_request is %d bytes, want 592", s)
	}
	if s := unsafe.Sizeof(gpioV2LineConfig{}); s != 272 {
		t.Errorf("gpio_v2_line_config is %d bytes, want 272", s)
	}
	if gpioV2GetLine != 0xc250b407 || gpioV2GetValues != 0xc010b40e || gpioV2SetValues != 0xc010b40f {
		t.Errorf("ioctls %#x %#x", gpioV2GetLine, gpioV2GetValues)
	}
}

func TestOpenSpecs(t *testing.T) {
	for _, bad := range []string{"serial:ttyACM0:rts", "gpio:gpiochip0:x", "gpio:gpiochip0", "serial:../etc/passwd:cts", "usb:x:y"} {
		if _, err := Open(bad); err == nil {
			t.Errorf("%s opened", bad)
		}
	}
	for _, bad := range []string{"serial:ttyUSB0:cts", "gpio:gpiochip0:x", "cm108:3"} {
		if _, err := OpenOutput(bad); err == nil {
			t.Errorf("output %s opened", bad)
		}
	}
	if _, err := Open("serial:ttyNOSUCH9:cts"); err == nil {
		t.Error("a missing serial device opened")
	}
}

// A bounce shorter than Stable is ignored; a held press and release count
// once each.
func TestDebounce(t *testing.T) {
	d := Debounce{Stable: 20 * time.Millisecond}
	t0 := time.Unix(0, 0)
	ms := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	var got []bool
	for i, level := range []bool{false, false, true, false, false, true, true, true, true, false, false, false, false} {
		if changed, p := d.Update(level, ms(10*i)); changed || i == 0 {
			got = append(got, p)
		}
	}
	// read low at first; the 10 ms bounce at 20 ms ignored; pressed at
	// 70 ms (held since 50); released at 110 (low since 90).
	if len(got) != 3 || got[0] || !got[1] || got[2] {
		t.Errorf("got %v", got)
	}
}

// keyEvent is a struct input_event for this architecture.
func keyEvent(code uint16, val int32) []byte {
	b := make([]byte, inputEventSize)
	t := b[inputEventSize-8:]
	binary.LittleEndian.PutUint16(t, evKey)
	binary.LittleEndian.PutUint16(t[2:], code)
	binary.LittleEndian.PutUint32(t[4:], uint32(val))
	return b
}

// The hook key (as a phone-style PTT adapter sends it) held, then
// released; a volume press; an autorepeat ignored.
func TestButton(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	b := newButton([]*os.File{r})
	defer b.Close()
	wait := func(want bool) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if p, _ := b.Pressed(); p == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("pressed never %v", want)
	}
	w.Write(keyEvent(164, 1))
	wait(true)
	w.Write(keyEvent(164, 2))
	w.Write(keyEvent(115, 1))
	if v := <-b.Volume; v != 1 {
		t.Errorf("volume %d", v)
	}
	wait(true)
	w.Write(keyEvent(164, 0))
	wait(false)
	w.Close()
	time.Sleep(20 * time.Millisecond)
	if _, err := b.Pressed(); err == nil {
		t.Error("no error once the device is gone")
	}
}
