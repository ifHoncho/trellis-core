package hoststat

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"meridian/trellis/internal/status"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// piHost is a CM4 node, seen from meshd's
// sandbox: the root filesystem also bind-mounted on /usr, /etc and
// /var/lib/trellis.
func piHost(t *testing.T) (*Reader, string, string) {
	proc, sys := t.TempDir(), t.TempDir()
	write(t, sys, "firmware/devicetree/base/model", "Raspberry Pi Compute Module 4 Rev 1.1\x00")
	write(t, sys, "class/thermal/thermal_zone0/type", "cpu-thermal\n")
	write(t, sys, "class/thermal/thermal_zone0/temp", "43329\n")
	write(t, sys, "class/thermal/thermal_zone0/trip_point_0_type", "critical\n")
	write(t, sys, "class/thermal/thermal_zone0/trip_point_0_temp", "110000\n")
	write(t, sys, "class/hwmon/hwmon1/name", "rpi_volt\n")
	write(t, sys, "class/hwmon/hwmon1/in0_lcrit_alarm", "0\n")
	write(t, sys, "class/hwmon/hwmon2/name", "mt7915_phy0\n")
	write(t, sys, "devices/system/cpu/cpu0/cpufreq/scaling_cur_freq", "600000\n")
	write(t, sys, "devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq", "1500000\n")
	write(t, proc, "uptime", "357.59 1300.10\n")
	write(t, proc, "loadavg", "0.52 0.31 0.12 1/180 1234\n")
	write(t, proc, "stat", "cpu  1553 0 1087 35758 970 0 26 0 0 0\ncpu0 400 0 300 9000 200 0 6 0 0 0\ncpu1 1 0 1 1 1 0 0 0 0 0\ncpu2 1 0 1 1 1 0 0 0 0 0\ncpu3 1 0 1 1 1 0 0 0 0 0\nintr 1\n")
	write(t, proc, "meminfo", "MemTotal:        1891372 kB\nMemFree:          900000 kB\nMemAvailable:    1500000 kB\nSwapTotal:       1891324 kB\nSwapFree:        1891324 kB\n")
	write(t, proc, "self/mountinfo", strings.Join([]string{
		`22 1 179:2 / / ro,relatime shared:1 - ext4 /dev/mmcblk0p2 rw`,
		`23 22 0:5 / /dev rw shared:2 - devtmpfs udev rw`,
		`30 22 179:1 / /boot/firmware rw shared:5 - vfat /dev/mmcblk0p1 rw`,
		`40 22 179:2 /boot /boot ro shared:1 - ext4 /dev/mmcblk0p2 rw`,
		`41 22 179:2 /etc /etc ro shared:1 - ext4 /dev/mmcblk0p2 rw`,
		`42 22 179:2 /usr /usr ro shared:1 - ext4 /dev/mmcblk0p2 rw`,
		`43 22 179:2 /var/lib/trellis /var/lib/trellis rw shared:1 - ext4 /dev/mmcblk0p2 rw`,
		`44 22 7:0 / /snap/core/1 ro - squashfs /dev/loop0 ro`,
		`45 22 8:1 / /mnt/my\040data rw - ext4 /dev/sda1 rw`,
		`46 22 0:40 / /run rw - tmpfs tmpfs rw`,
	}, "\n")+"\n")
	r := New(proc, sys)
	r.statfs = func(path string, st *syscall.Statfs_t) error {
		*st = syscall.Statfs_t{Bsize: 4096}
		switch path {
		case "/":
			st.Blocks, st.Bfree, st.Bavail = 7_600_000, 6_300_000, 5_900_000
		case "/boot/firmware":
			st.Blocks, st.Bfree, st.Bavail = 129_000, 111_000, 111_000
		case "/mnt/my data":
			st.Blocks, st.Bfree, st.Bavail = 1000, 20, 10
		default:
			return syscall.ENOENT
		}
		return nil
	}
	return r, proc, sys
}

func TestSamplePi(t *testing.T) {
	r, proc, sys := piHost(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	h := r.Sample(now)
	if h.Model != "Raspberry Pi Compute Module 4 Rev 1.1" {
		t.Errorf("model %q", h.Model)
	}
	if h.UptimeS != 357.59 || h.CPUs != 4 || h.Load != [3]float64{0.52, 0.31, 0.12} {
		t.Errorf("uptime %v cpus %d load %v", h.UptimeS, h.CPUs, h.Load)
	}
	if h.CPUPct != -1 {
		t.Errorf("first sample: cpu %v, want -1 (nothing to compare with)", h.CPUPct)
	}
	if h.CPUMHz != 600 || h.CPUMaxMHz != 1500 {
		t.Errorf("clock %d of %d MHz", h.CPUMHz, h.CPUMaxMHz)
	}
	if h.MemTotal != 1891372*1024 || h.MemAvailable != 1500000*1024 || h.SwapTotal != 1891324*1024 {
		t.Errorf("memory %+v", h)
	}
	if len(h.Temps) != 1 || h.Temps[0] != (status.HostTemp{Name: "cpu-thermal", TempC: 43.3, HotC: DefaultHotC, CritC: 110}) {
		t.Errorf("temps %+v", h.Temps)
	}
	if h.Undervoltage == nil || *h.Undervoltage {
		t.Errorf("undervoltage %v, want a false that is known", h.Undervoltage)
	}
	// One entry per device: / (not /usr, /etc...), the boot partition and
	// a data disk with a space in its name; no tmpfs, no snap.
	var mounts []string
	for _, f := range h.Filesystems {
		mounts = append(mounts, f.Mount+" "+f.Device)
	}
	if got := strings.Join(mounts, ", "); got != "/ /dev/mmcblk0p2, /boot/firmware /dev/mmcblk0p1, /mnt/my data /dev/sda1" {
		t.Errorf("filesystems: %s", got)
	}
	if f := h.Filesystems[0]; f.Size != 7_600_000*4096 || f.Used != 1_300_000*4096 || f.Avail != 5_900_000*4096 {
		t.Errorf("/: %+v", f)
	}

	// Within a second: the same sample.
	write(t, proc, "loadavg", "3.00 2.00 1.00 1/180 1234\n")
	if h2 := r.Sample(now.Add(500 * time.Millisecond)); h2.Load[0] != 0.52 {
		t.Errorf("cached sample re-read: load %v", h2.Load)
	}
	// Later: CPU use since the last sample (300 busy of 400 ticks), and a
	// low supply seen once is remembered.
	write(t, proc, "stat", "cpu  1753 0 1187 35858 970 0 26 0 0 0\ncpu0 1 0 1 1 1 0 0 0 0 0\n")
	write(t, sys, "class/hwmon/hwmon1/in0_lcrit_alarm", "1\n")
	h3 := r.Sample(now.Add(2 * time.Second))
	if h3.CPUPct != 75 || h3.Load[0] != 3 {
		t.Errorf("second sample: cpu %v load %v", h3.CPUPct, h3.Load)
	}
	if h.UndervoltageAt != nil {
		t.Errorf("never low, yet low at %v", h.UndervoltageAt)
	}
	if h3.Undervoltage == nil || !*h3.Undervoltage || h3.UndervoltageAt == nil || !h3.UndervoltageAt.Equal(now.Add(2*time.Second)) {
		t.Errorf("undervoltage %v at %v", h3.Undervoltage, h3.UndervoltageAt)
	}
	write(t, sys, "class/hwmon/hwmon1/in0_lcrit_alarm", "0\n")
	h4 := r.Sample(now.Add(4 * time.Second))
	if *h4.Undervoltage || h4.UndervoltageAt == nil || !h4.UndervoltageAt.Equal(now.Add(2*time.Second)) {
		t.Errorf("after: undervoltage %v at %v", *h4.Undervoltage, h4.UndervoltageAt)
	}
	if h4.CPUPct != 75 {
		t.Errorf("counters did not move: cpu %v, want the last figure", h4.CPUPct)
	}
}

// A PC: no device tree, DMI names, no thermal zone but coretemp, no supply
// alarm.
func TestSamplePC(t *testing.T) {
	proc, sys := t.TempDir(), t.TempDir()
	write(t, sys, "class/dmi/id/sys_vendor", "LENOVO\n")
	write(t, sys, "class/dmi/id/product_name", "ThinkCentre M720q\n")
	write(t, sys, "class/hwmon/hwmon0/name", "coretemp\n")
	write(t, sys, "class/hwmon/hwmon0/temp1_input", "55000\n")
	write(t, sys, "class/hwmon/hwmon0/temp1_crit", "100000\n")
	write(t, sys, "class/thermal/thermal_zone0/type", "iwlwifi_1\n")
	write(t, sys, "class/thermal/thermal_zone0/temp", "40000\n")
	r := New(proc, sys)
	h := r.Sample(time.Now())
	if h.Model != "LENOVO ThinkCentre M720q" {
		t.Errorf("model %q", h.Model)
	}
	if len(h.Temps) != 1 || h.Temps[0] != (status.HostTemp{Name: "coretemp", TempC: 55, HotC: DefaultHotC, CritC: 100}) {
		t.Errorf("temps %+v", h.Temps)
	}
	if h.Undervoltage != nil {
		t.Error("undervoltage known on a board that does not say")
	}
	if h.Filesystems == nil || h.Temps == nil {
		t.Error("nil lists: the API promises arrays")
	}
	// A self-built PC (a desktop mainboard's firmware): the
	// mainboard's name instead.
	write(t, sys, "class/dmi/id/sys_vendor", "ASUS\n")
	write(t, sys, "class/dmi/id/product_name", "System Product Name\n")
	write(t, sys, "class/dmi/id/board_vendor", "ASUSTeK COMPUTER INC.\n")
	write(t, sys, "class/dmi/id/board_name", "PRIME Z390-A\n")
	if m := New(proc, sys).model(); m != "ASUSTeK COMPUTER INC. PRIME Z390-A" {
		t.Errorf("placeholder product: model %q", m)
	}
	write(t, sys, "class/dmi/id/board_name", "Default string\n")
	if m := New(proc, sys).model(); m != "" {
		t.Errorf("all placeholders: model %q", m)
	}
}

func TestPassiveTrip(t *testing.T) {
	proc, sys := t.TempDir(), t.TempDir()
	for zone, temp := range map[string]string{"thermal_zone0": "61000", "thermal_zone1": "72500"} {
		write(t, sys, "class/thermal/"+zone+"/type", map[string]string{"thermal_zone0": "gpu-thermal", "thermal_zone1": "soc-thermal"}[zone])
		write(t, sys, "class/thermal/"+zone+"/temp", temp)
	}
	write(t, sys, "class/thermal/thermal_zone1/trip_point_0_type", "passive\n")
	write(t, sys, "class/thermal/thermal_zone1/trip_point_0_temp", "75000\n")
	write(t, sys, "class/thermal/thermal_zone1/trip_point_1_type", "passive\n")
	write(t, sys, "class/thermal/thermal_zone1/trip_point_1_temp", "70000\n")
	write(t, sys, "class/thermal/thermal_zone1/trip_point_2_type", "critical\n")
	write(t, sys, "class/thermal/thermal_zone1/trip_point_2_temp", "95000\n")
	h := New(proc, sys).Sample(time.Now())
	if len(h.Temps) != 2 || h.Temps[0].Name != "soc-thermal" || h.Temps[0].HotC != 70 || h.Temps[0].CritC != 95 || h.Temps[1].HotC != DefaultHotC {
		t.Errorf("temps %+v", h.Temps)
	}
}

func TestCheck(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	yes, no := true, false
	base := func() status.Host {
		return status.Host{MemTotal: 1000, MemAvailable: 500, Undervoltage: &no,
			Temps:       []status.HostTemp{{Name: "cpu-thermal", TempC: 45, HotC: 80, CritC: 110}},
			Filesystems: []status.Filesystem{{Mount: "/", Size: 100, Used: 50, Avail: 50}}}
	}
	codes := func(h status.Host) string {
		var s []string
		for _, f := range Check(h, now) {
			s = append(s, f.Level+" "+f.Code)
		}
		return strings.Join(s, ", ")
	}
	cases := []struct {
		name string
		edit func(h *status.Host)
		want string
	}{
		{"healthy", func(h *status.Host) {}, ""},
		{"warm", func(h *status.Host) { h.Temps[0].TempC = 72 }, "info host-hot"},
		{"hot", func(h *status.Host) { h.Temps[0].TempC = 81 }, "warning host-hot"},
		{"near shutdown", func(h *status.Host) { h.Temps[0].TempC = 106 }, "error host-hot"},
		{"one alert for two sensors", func(h *status.Host) {
			h.Temps = []status.HostTemp{{Name: "a", TempC: 85, HotC: 80}, {Name: "b", TempC: 82, HotC: 80}}
		}, "warning host-hot"},
		{"a cooler sensor near shutdown outranks a warm one", func(h *status.Host) {
			h.Temps = []status.HostTemp{{Name: "cpu-thermal", TempC: 72, HotC: 80, CritC: 110}, {Name: "nvme", TempC: 69, HotC: 75, CritC: 70}}
		}, "error host-hot"},
		{"undervoltage now", func(h *status.Host) { h.Undervoltage = &yes; h.UndervoltageAt = at(now) }, "warning undervoltage"},
		{"undervoltage lately", func(h *status.Host) { h.UndervoltageAt = at(now.Add(-3 * time.Minute)) }, "info undervoltage"},
		{"undervoltage long ago", func(h *status.Host) { h.UndervoltageAt = at(now.Add(-11 * time.Minute)) }, ""},
		{"nearly full", func(h *status.Host) { h.Filesystems[0].Used, h.Filesystems[0].Avail = 91, 9 }, "warning disk-full"},
		{"full", func(h *status.Host) { h.Filesystems[0].Used, h.Filesystems[0].Avail = 98, 2 }, "error disk-full"},
		{"memory low", func(h *status.Host) { h.MemAvailable = 80 }, "warning memory-low"},
		{"memory very low", func(h *status.Host) { h.MemAvailable = 40 }, "error memory-low"},
		{"nothing known", func(h *status.Host) { *h = status.Host{} }, ""},
	}
	for _, c := range cases {
		h := base()
		c.edit(&h)
		if got := codes(h); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	h := base()
	h.UndervoltageAt = at(now.Add(-3 * time.Minute))
	if m := Check(h, now)[0].Message; !strings.Contains(m, "3 min ago") {
		t.Errorf("message %q", m)
	}
	// A sensor that is not the CPU's is named, not called the CPU.
	for name, want := range map[string]string{"cpu-thermal": "CPU temperature high", "x86_pkg_temp": "CPU temperature high", "acpitz": "Sensor acpitz temperature high"} {
		h := base()
		h.Temps = []status.HostTemp{{Name: name, TempC: 82, HotC: 80}}
		if m := Check(h, now)[0].Message; !strings.HasPrefix(m, want) {
			t.Errorf("%s: message %q", name, m)
		}
	}
}

func at(t time.Time) *time.Time { return &t }

func TestBytes(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 512: "512 B", 1500: "1.5 kB", 512e6: "512 MB", 1_933_062_144: "1.9 GB", 30_168_961_024: "30 GB"} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}
