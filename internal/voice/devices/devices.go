// Package devices finds a node's audio devices in sysfs and names each by
// what it is, not by its ALSA card number, which can change between boots
// (architecture §13, Voice): a USB adapter by vendor, product and port path, an
// onboard card by its ALSA id. For a USB adapter it also finds the HID
// interface of the same device (the CM108's PTT and buttons).
package devices

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Device is one ALSA sound card.
type Device struct {
	ID      string `json:"id"` // usb:0d8c:0012@1-1.3, or onboard:<ALSA id>
	Card    int    `json:"card"`
	CardID  string `json:"card_id"` // ALSA's id, as in "hw:CARD=Device"
	Name    string `json:"name"`
	USB     bool   `json:"usb"`
	Vendor  uint16 `json:"vendor,omitempty"`
	Product uint16 `json:"product,omitempty"`
	Serial  string `json:"serial,omitempty"`
	Port    string `json:"port,omitempty"`   // the USB port path, 1-1.3
	HIDRaw  string `json:"hidraw,omitempty"` // /dev/hidrawN of the same USB device
	// Inputs are the same USB device's input devices (/dev/input/eventN):
	// a headset button, volume keys.
	Inputs   []string `json:"inputs,omitempty"`
	Capture  bool     `json:"capture"`
	Playback bool     `json:"playback"`
}

// Scan lists the sound cards under sys (normally /sys), ordered by ID so
// the order is the same every boot.
func Scan(sys string) ([]Device, error) {
	cards, err := filepath.Glob(filepath.Join(sys, "class/sound/card[0-9]*"))
	if err != nil {
		return nil, err
	}
	hid := nodes(sys, "class/hidraw/hidraw[0-9]*")
	inputs := nodes(sys, "class/input/event[0-9]*")
	var out []Device
	for _, c := range cards {
		n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(c), "card"))
		if err != nil {
			continue
		}
		d := Device{Card: n, CardID: read(filepath.Join(c, "id"))}
		d.Capture = exists(sys, fmt.Sprintf("class/sound/pcmC%dD*c", n))
		d.Playback = exists(sys, fmt.Sprintf("class/sound/pcmC%dD*p", n))
		real, err := filepath.EvalSymlinks(filepath.Join(c, "device"))
		if err == nil {
			if usb := usbDevice(real); usb != "" {
				d.USB = true
				d.Vendor, d.Product = hex16(read(filepath.Join(usb, "idVendor"))), hex16(read(filepath.Join(usb, "idProduct")))
				d.Name = read(filepath.Join(usb, "product"))
				d.Serial = read(filepath.Join(usb, "serial"))
				d.Port = filepath.Base(usb)
				d.ID = fmt.Sprintf("usb:%04x:%04x@%s", d.Vendor, d.Product, d.Port)
				for path, node := range hid {
					// The same one every time, when a device has several.
					if strings.HasPrefix(path, usb+string(filepath.Separator)) && (d.HIDRaw == "" || node < d.HIDRaw) {
						d.HIDRaw = node
					}
				}
				for path, node := range inputs {
					if strings.HasPrefix(path, usb+string(filepath.Separator)) {
						d.Inputs = append(d.Inputs, node)
					}
				}
				sort.Strings(d.Inputs)
			}
		}
		if !d.USB {
			d.ID = "onboard:" + d.CardID
		}
		if d.Name == "" {
			d.Name = d.CardID
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// usbDevice walks up from a device path to the USB device it belongs to:
// the first directory with idVendor and idProduct.
func usbDevice(p string) string {
	for d := p; d != "/" && d != "." && d != ""; d = filepath.Dir(d) {
		if fileExists(filepath.Join(d, "idVendor")) && fileExists(filepath.Join(d, "idProduct")) {
			return d
		}
	}
	return ""
}

// nodes maps each class device's resolved sysfs path to its /dev node:
// /dev/hidrawN, /dev/input/eventN.
func nodes(sys, pattern string) map[string]string {
	out := map[string]string{}
	found, _ := filepath.Glob(filepath.Join(sys, pattern))
	for _, n := range found {
		if real, err := filepath.EvalSymlinks(n); err == nil {
			dev := "/dev/" + filepath.Base(n)
			if strings.Contains(pattern, "input/") {
				dev = "/dev/input/" + filepath.Base(n)
			}
			out[real] = dev
		}
	}
	return out
}

func read(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func hex16(s string) uint16 {
	v, _ := strconv.ParseUint(s, 16, 16)
	return uint16(v)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func exists(sys, pattern string) bool {
	m, _ := filepath.Glob(filepath.Join(sys, pattern))
	return len(m) > 0
}

// Find returns the device with an ID, or nil.
func Find(devs []Device, id string) *Device {
	for i := range devs {
		if devs[i].ID == id {
			return &devs[i]
		}
	}
	return nil
}
