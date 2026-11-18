package cm108

import "testing"

func TestReport(t *testing.T) {
	r := Report{0x01, 0x05, 0, 0} // volume up; GPIO1 and GPIO3 high
	for pin, want := range map[int]bool{1: true, 2: false, 3: true, 4: false} {
		if high, ok := r.GPIO(pin); !ok || high != want {
			t.Errorf("GPIO%d = %v %v", pin, high, ok)
		}
	}
	if !r.VolumeUp() || r.VolumeDown() {
		t.Error("volume bits")
	}
	if _, ok := (Report{0x80, 0xff}).GPIO(1); ok {
		t.Error("IR1 read as GPIO while it carries EEPROM data")
	}
	if _, ok := r.GPIO(5); ok {
		t.Error("GPIO5")
	}
}

func TestIdentify(t *testing.T) {
	strap := Report{0, 0x01}
	if m := Identify(0x0012, "USB Audio Device", strap); m == nil || m.Name != "OpenVLM" || m.PTTPin != 3 || !m.PTTHigh || !m.Mono {
		t.Errorf("strapped CM108B: %+v", m)
	}
	if m := Identify(0x0012, "OpenVLM", Report{}); m == nil {
		t.Error("provisioned OpenVLM (product string) not known")
	}
	if m := Identify(0x0012, "USB Audio Device", Report{}); m != nil {
		t.Errorf("a generic CM108B taken for %s", m.Name)
	}
	if m := Identify(0x000c, "", strap); m != nil {
		t.Errorf("a CM108 taken for %s", m.Name)
	}
}

// HIDIOCGINPUT(5) as linux/hidraw.h computes it.
func TestIoctlNumber(t *testing.T) {
	if got := hidiocgInput(5); got != 0xc005480a {
		t.Errorf("HIDIOCGINPUT(5) = %#x", got)
	}
}
