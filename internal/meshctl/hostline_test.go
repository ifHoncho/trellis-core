package meshctl

import (
	"strings"
	"testing"

	"meridian/trellis/internal/status"
)

func TestHostLine(t *testing.T) {
	no, yes := false, true
	// A CM4 node (supply ok here).
	h := status.Host{Model: "Raspberry Pi Compute Module 4 Rev 1.1", UptimeS: 3*86400 + 7*3600 + 120, CPUs: 4, CPUPct: 2.5, Load: [3]float64{0.12, 0, 0},
		MemTotal: 1_933_062_144, MemAvailable: 1_717_923_840, Undervoltage: &no,
		Temps: []status.HostTemp{{Name: "cpu-thermal", TempC: 42.8, HotC: 80, CritC: 110}},
		Filesystems: []status.Filesystem{
			{Mount: "/", Size: 30_168_961_024, Used: 4_654_776_320, Avail: 24_243_888_128},
			{Mount: "/boot/firmware", Size: 528_592_896, Used: 69_558_272, Avail: 459_034_624}}}
	want := "Raspberry Pi Compute Module 4 Rev 1.1, up 3d 7h; cpu-thermal 43 °C; CPU 2% (load 0.12, 4 CPUs); memory 11% used of 1.9 GB; / 16% of 30 GB, /boot/firmware 13% of 529 MB; supply ok"
	if got := hostLine(h); got != want {
		t.Errorf("hostLine:\n got %s\nwant %s", got, want)
	}
	h.Undervoltage, h.Temps[0].TempC, h.CPUPct, h.UptimeS = &yes, 81, -1, 600
	got := hostLine(h)
	for _, s := range []string{"up 10m;", "81 °C HIGH (throttle at 80 °C)", "CPU – (load"} {
		if !strings.Contains(got, s) {
			t.Errorf("troubled: %s; want %q in it", got, s)
		}
	}
	if !strings.HasSuffix(got, "supply TOO LOW") {
		t.Errorf("troubled: %s", got)
	}
	if got := hostLine(status.Host{}); got != "" {
		t.Errorf("older meshd: %q", got)
	}
}
