package devices

import (
	"os"
	"path/filepath"
	"testing"
)

// sysfs builds a fake /sys: a CM108B on USB port 1-1.3 with its HID
// interface, and the Pi's onboard audio (playback only).
func sysfs(t *testing.T) string {
	t.Helper()
	sys := t.TempDir()
	w := func(p, s string) {
		p = filepath.Join(sys, p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := func(link, target string) {
		link = filepath.Join(sys, link)
		os.MkdirAll(filepath.Dir(link), 0o755)
		os.MkdirAll(filepath.Join(sys, target), 0o755)
		if err := os.Symlink(filepath.Join(sys, target), link); err != nil {
			t.Fatal(err)
		}
	}
	usb := "devices/platform/scb/fd500000.pcie/pci0000:00/0000:01:00.0/usb1/1-1/1-1.3"
	w(usb+"/idVendor", "0d8c")
	w(usb+"/idProduct", "0012")
	w(usb+"/product", "USB Audio Device")
	l("class/sound/card1", usb+"/1-1.3:1.0/sound/card1")
	w(usb+"/1-1.3:1.0/sound/card1/id", "Device")
	l("class/sound/card1/device", usb+"/1-1.3:1.0")
	l("class/sound/pcmC1D0c", usb+"/1-1.3:1.0/sound/card1/pcmC1D0c")
	l("class/sound/pcmC1D0p", usb+"/1-1.3:1.0/sound/card1/pcmC1D0p")
	l("class/hidraw/hidraw2", usb+"/1-1.3:1.3/0003:0D8C:0012.0001/hidraw/hidraw2")
	l("class/hidraw/hidraw0", "devices/virtual/misc/uhid/0003:046D:C52B.0002/hidraw/hidraw0")
	l("class/input/event3", usb+"/1-1.3:1.3/0003:0D8C:0012.0001/input/input5/event3")
	l("class/input/event0", "devices/platform/gpio-keys/input/input0/event0")

	onboard := "devices/platform/soc/soc:audio/bcm2835_audio"
	l("class/sound/card0", onboard+"/sound/card0")
	w(onboard+"/sound/card0/id", "Headphones")
	l("class/sound/card0/device", onboard)
	l("class/sound/pcmC0D0p", onboard+"/sound/card0/pcmC0D0p")
	return sys
}

func TestScan(t *testing.T) {
	devs, err := Scan(sysfs(t))
	if err != nil || len(devs) != 2 {
		t.Fatalf("%+v %v", devs, err)
	}
	pi, cm := devs[0], devs[1]
	if pi.ID != "onboard:Headphones" || pi.USB || pi.Capture || !pi.Playback || pi.HIDRaw != "" {
		t.Errorf("onboard: %+v", pi)
	}
	if cm.ID != "usb:0d8c:0012@1-1.3" || !cm.USB || cm.Card != 1 || cm.HIDRaw != "/dev/hidraw2" || !cm.Capture || !cm.Playback || cm.Name != "USB Audio Device" ||
		len(cm.Inputs) != 1 || cm.Inputs[0] != "/dev/input/event3" {
		t.Errorf("CM108B: %+v", cm)
	}
	if Find(devs, "usb:0d8c:0012@1-1.3") == nil || Find(devs, "usb:0d8c:0012@1-1.4") != nil {
		t.Error("Find")
	}
}
