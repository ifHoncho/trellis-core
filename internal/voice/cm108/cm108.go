// Package cm108 reads PTT and the volume buttons of C-Media CM108-family
// USB audio adapters through their HID interface (the CM108AH datasheet,
// §7.4), and knows the adapters Trellis supports by name, the OpenVLM
// first. It only reads: it never writes the chip's EEPROM.
package cm108

// Vendor is C-Media's USB vendor ID.
const Vendor = 0x0d8c

// Report is the chip's 4-byte HID input report (CM108AH datasheet §7.4):
// IR0 bit 0 volume up held, bit 1 volume down held; bits 7–6 00 means IR1
// holds the GPIO inputs, bits 3–0 = GPIO4…GPIO1.
type Report [4]byte

// GPIO reports whether pin (1–4) reads high. ok is false when the report
// does not carry GPIO states.
func (r Report) GPIO(pin int) (high, ok bool) {
	if pin < 1 || pin > 4 || r[0]>>6 != 0 {
		return false, false
	}
	return r[1]&(1<<(pin-1)) != 0, true
}

func (r Report) VolumeUp() bool   { return r[0]&0x01 != 0 }
func (r Report) VolumeDown() bool { return r[0]&0x02 != 0 }

// Model is an adapter Trellis knows.
type Model struct {
	Name    string
	Product uint16
	// PTTPin is the GPIO the PTT reads on, PTTHigh its level when pressed.
	PTTPin  int
	PTTHigh bool
	// StrapPin, when set, must read high for the adapter to be this model
	// (OpenVLM's GPIO1 strap; a generic CM108 reads it low).
	StrapPin int
	// Mono: one earpiece (both channels are mixed into the left output).
	Mono bool
	// VolumeButtons: it has volume buttons wired to the chip.
	VolumeButtons bool
}

// Models Trellis knows. OpenVLM's PTT polarity is from its schematic (an
// NPN inverter, collector on GPIO3 pulled up): released low, pressed
// high. A generic CM108 board wired as one (GPIO1 to 3.3 V through
// 10 kΩ, a PTT button from GPIO3 to 3.3 V) behaves the same.
var Models = []Model{
	{Name: "OpenVLM", Product: 0x0012, PTTPin: 3, PTTHigh: true, StrapPin: 1, Mono: true, VolumeButtons: true},
}

// Identify returns the model an adapter is, given its USB product ID and
// one input report; nil when it is a CM108 Trellis has no settings for.
func Identify(product uint16, productName string, r Report) *Model {
	for i := range Models {
		m := &Models[i]
		if m.Product != product {
			continue
		}
		if productName == m.Name {
			return m
		}
		if m.StrapPin != 0 {
			if high, ok := r.GPIO(m.StrapPin); ok && high {
				return m
			}
		}
	}
	return nil
}
