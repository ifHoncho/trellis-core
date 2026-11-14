package doctor

import "testing"

// A Raspberry Pi with the receiver on its UART: gpsd on ttyAMA0, the kernel
// console on serial0 (the same port).
func TestGNSSOnConsole(t *testing.T) {
	resolve := func(p string) string {
		if p == "/dev/serial0" {
			return "/dev/ttyAMA0"
		}
		return p
	}
	gpsd := "START_DAEMON=\"true\"\nGPSD_OPTIONS=\"-n\"\nDEVICES=\"/dev/ttyAMA0\"\n"
	for name, c := range map[string]struct {
		cmdline, gpsd, dev, console string
	}{
		"console on the GNSS port":           {"console=serial0,115200 console=tty1 root=PARTUUID=1 rootwait", gpsd, "/dev/ttyAMA0", "serial0,115200"},
		"fixed (console on the screen only)": {"console=tty1 root=PARTUUID=1", gpsd, "", ""},
		"console elsewhere":                  {"console=ttyS2,1500000 console=tty1", gpsd, "", ""},
		"gpsd through the symlink":           {"console=ttyAMA0,115200", "DEVICES=\"/dev/serial0\"\n", "/dev/serial0", "ttyAMA0,115200"},
		"no gpsd devices":                    {"console=serial0,115200", "START_DAEMON=\"true\"\n", "", ""},
	} {
		if dev, console := gnssOnConsole(c.cmdline, c.gpsd, resolve); dev != c.dev || console != c.console {
			t.Errorf("%s: %q %q, want %q %q", name, dev, console, c.dev, c.console)
		}
	}
}
