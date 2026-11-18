package cm108

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// HIDRaw is a CM108's HID interface, /dev/hidrawN.
type HIDRaw struct {
	f               *os.File
	outputs, levels byte // pins driven as outputs, and their levels (SetGPIO)
}

// OpenHIDRaw opens a hidraw node for reading reports.
func OpenHIDRaw(path string) (*HIDRaw, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &HIDRaw{f: f}, nil
}

// hidiocgInput is HIDIOCGINPUT(len) from linux/hidraw.h:
// _IOC(_IOC_READ|_IOC_WRITE, 'H', 0x0A, len).
func hidiocgInput(n int) uintptr {
	const iocRead, iocWrite = 2, 1
	return uintptr(uint32(iocRead|iocWrite)<<30 | uint32(n)<<16 | 'H'<<8 | 0x0a)
}

// Input asks the chip for its input report (a GET_REPORT control
// transfer, Linux 5.11 and later). The answer starts with the report
// number, 0 for a device without numbered reports.
func (h *HIDRaw) Input() (Report, error) {
	var buf [1 + len(Report{})]byte
	conn, err := h.f.SyscallConn()
	if err != nil {
		return Report{}, err
	}
	var n uintptr
	var errno unix.Errno
	cerr := conn.Control(func(fd uintptr) {
		n, _, errno = unix.Syscall(unix.SYS_IOCTL, fd, hidiocgInput(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	})
	if cerr != nil {
		return Report{}, cerr
	}
	if errno != 0 {
		return Report{}, errno
	}
	if int(n) < len(buf) {
		return Report{}, errors.New("cm108: short input report")
	}
	var r Report
	copy(r[:], buf[1:])
	return r, nil
}

func (h *HIDRaw) Close() error { return h.f.Close() }

// SetGPIO drives a GPIO pin (1–4) as an output, high or low, through the
// output report (OR0 bits 7–6 00: OR1 the levels, OR2 the directions).
// Pins never set stay inputs, so a PTT read on another pin keeps working.
func (h *HIDRaw) SetGPIO(pin int, high bool) error {
	if pin < 1 || pin > 4 {
		return errors.New("cm108: GPIO pin is 1–4")
	}
	bit := byte(1) << (pin - 1)
	h.outputs |= bit
	if high {
		h.levels |= bit
	} else {
		h.levels &^= bit
	}
	// Report number 0, then OR0–OR3.
	_, err := h.f.Write([]byte{0, 0, h.levels, h.outputs, 0})
	return err
}
