// Package hoststat reads what the kernel says about the computer a node
// runs on: its temperatures, CPU and memory use, storage and power supply.
// Every read is of a small file in /proc or /sys (and one statfs per
// filesystem): cheap enough to do every few seconds.
package hoststat

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"meridian/trellis/internal/status"
)

// DefaultHotC is where a sensor with no passive trip point is called hot:
// a Raspberry Pi's firmware slows the CPU from 80 °C, and that is a common
// limit for small boards.
const DefaultHotC = 80

// cacheFor is how long a sample is reused: several readers (the web UI's
// poll, meshctl, the hardware reconciler) may ask within the same moment.
const cacheFor = time.Second

// Reader samples the host. The zero value is not ready: use New.
type Reader struct {
	proc, sys string
	// statfs is syscall.Statfs; variable for tests.
	statfs func(path string, st *syscall.Statfs_t) error

	mu      sync.Mutex
	prev    cpuTimes
	last    status.Host
	lastAt  time.Time
	lowAt   time.Time
	sampled bool
}

// New is a reader of the given /proc and /sys (normally "/proc", "/sys").
func New(proc, sys string) *Reader {
	return &Reader{proc: proc, sys: sys, statfs: syscall.Statfs}
}

// Sample reads the host now, or gives the sample taken less than a second
// ago.
func (r *Reader) Sample(now time.Time) status.Host {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sampled && now.Sub(r.lastAt) >= 0 && now.Sub(r.lastAt) < cacheFor {
		return r.last
	}
	h := status.Host{CPUPct: -1, Temps: []status.HostTemp{}, Filesystems: []status.Filesystem{}}
	h.Model = r.model()
	h.UptimeS = r.uptime()
	cpu, n := r.cpuTimes()
	h.CPUs = n
	if r.prev.total > 0 && cpu.total > r.prev.total {
		dt := float64(cpu.total - r.prev.total)
		h.CPUPct = round1(100 * float64(cpu.busy-min(cpu.busy, r.prev.busy)) / dt)
	} else if r.sampled && cpu.total == r.prev.total {
		// Too soon for the counters to move: the last figure stands.
		h.CPUPct = r.last.CPUPct
	}
	if cpu.total > 0 {
		r.prev = cpu
	}
	h.Load = r.load()
	h.CPUMHz = readInt(filepath.Join(r.sys, "devices/system/cpu/cpu0/cpufreq/scaling_cur_freq")) / 1000
	h.CPUMaxMHz = readInt(filepath.Join(r.sys, "devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq")) / 1000
	r.memory(&h)
	h.Temps = r.temps()
	if low, ok := r.undervoltage(); ok {
		h.Undervoltage = &low
		if low {
			r.lowAt = now
		}
	}
	if !r.lowAt.IsZero() {
		at := r.lowAt
		h.UndervoltageAt = &at
	}
	h.Filesystems = r.filesystems()
	r.last, r.lastAt, r.sampled = h, now, true
	return h
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
}

// readInt is a file's integer, 0 when it is missing or not one.
func readInt(p string) int {
	v, _ := strconv.Atoi(readTrim(p))
	return v
}

// model names the board: the device tree's model on ARM boards, else the
// firmware's (DMI) vendor and product.
func (r *Reader) model() string {
	if m := readTrim(filepath.Join(r.sys, "firmware/devicetree/base/model")); m != "" {
		return m
	}
	dmi := filepath.Join(r.sys, "class/dmi/id")
	placeholder := func(s string) bool {
		l := strings.ToLower(s)
		return s == "" || strings.Contains(l, "to be filled") || strings.Contains(l, "system product name") ||
			strings.Contains(l, "system manufacturer") || l == "default string" || l == "not applicable"
	}
	// The machine's name, else its mainboard's (a self-built PC's
	// firmware often leaves the first a placeholder).
	for _, f := range [][2]string{{"sys_vendor", "product_name"}, {"board_vendor", "board_name"}} {
		vendor, product := readTrim(filepath.Join(dmi, f[0])), readTrim(filepath.Join(dmi, f[1]))
		switch {
		case placeholder(product):
			continue
		case placeholder(vendor) || strings.HasPrefix(product, vendor):
			return product
		}
		return vendor + " " + product
	}
	return ""
}

func (r *Reader) uptime() float64 {
	f := strings.Fields(readTrim(filepath.Join(r.proc, "uptime")))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func (r *Reader) load() [3]float64 {
	var l [3]float64
	f := strings.Fields(readTrim(filepath.Join(r.proc, "loadavg")))
	for i := 0; i < 3 && i < len(f); i++ {
		l[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return l
}

// cpuTimes is all CPUs' time since boot, in ticks: busy, and in all.
type cpuTimes struct{ busy, total uint64 }

// cpuTimes reads /proc/stat's total line, and counts the CPUs.
func (r *Reader) cpuTimes() (cpuTimes, int) {
	b, err := os.ReadFile(filepath.Join(r.proc, "stat"))
	if err != nil {
		return cpuTimes{}, 0
	}
	var t cpuTimes
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			n++
			continue
		}
		// user nice system idle iowait irq softirq steal (guest time is
		// already in user).
		for i := 1; i < len(f) && i <= 8; i++ {
			v, _ := strconv.ParseUint(f[i], 10, 64)
			t.total += v
			if i != 4 && i != 5 {
				t.busy += v
			}
		}
	}
	return t, n
}

func (r *Reader) memory(h *status.Host) {
	b, err := os.ReadFile(filepath.Join(r.proc, "meminfo"))
	if err != nil {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		v *= 1024 // kB
		switch f[0] {
		case "MemTotal:":
			h.MemTotal = v
		case "MemAvailable:":
			h.MemAvailable = v
		case "SwapTotal:":
			h.SwapTotal = v
		case "SwapFree:":
			h.SwapFree = v
		}
	}
}

// temps reads the thermal zones, hottest first. A board with none (some
// PCs) gives its CPU's hwmon sensor instead.
func (r *Reader) temps() []status.HostTemp {
	out := []status.HostTemp{}
	zones, _ := filepath.Glob(filepath.Join(r.sys, "class/thermal/thermal_zone*"))
	for _, z := range zones {
		name := readTrim(filepath.Join(z, "type"))
		if name == "" || strings.HasPrefix(name, "iwlwifi") {
			continue // a Wi-Fi card's: radios report their own
		}
		s := readTrim(filepath.Join(z, "temp"))
		milli, err := strconv.Atoi(s)
		if err != nil || milli <= -273000 {
			continue
		}
		t := status.HostTemp{Name: name, TempC: round1(float64(milli) / 1000)}
		trips, _ := filepath.Glob(filepath.Join(z, "trip_point_*_type"))
		for _, tp := range trips {
			v := readInt(strings.TrimSuffix(tp, "_type") + "_temp")
			if v <= 0 {
				continue
			}
			c := float64(v) / 1000
			switch readTrim(tp) {
			case "passive":
				if t.HotC == 0 || c < t.HotC {
					t.HotC = c
				}
			case "critical":
				t.CritC = c
			}
		}
		if t.HotC == 0 {
			t.HotC = DefaultHotC
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		mons, _ := filepath.Glob(filepath.Join(r.sys, "class/hwmon/hwmon*"))
		for _, m := range mons {
			switch name := readTrim(filepath.Join(m, "name")); name {
			case "coretemp", "k10temp", "zenpower", "cpu_thermal", "soc_thermal":
				if v := readInt(filepath.Join(m, "temp1_input")); v != 0 {
					t := status.HostTemp{Name: name, TempC: round1(float64(v) / 1000), HotC: DefaultHotC}
					if c := readInt(filepath.Join(m, "temp1_crit")); c > 0 {
						t.CritC = float64(c) / 1000
					}
					out = append(out, t)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TempC > out[j].TempC })
	return out
}

// undervoltage is whether a Raspberry Pi's supply is low now (rpi_volt's
// alarm: low at any time since the driver last asked the firmware, about
// every two seconds). ok is false on boards that do not say.
func (r *Reader) undervoltage() (low, ok bool) {
	mons, _ := filepath.Glob(filepath.Join(r.sys, "class/hwmon/hwmon*"))
	for _, m := range mons {
		if readTrim(filepath.Join(m, "name")) != "rpi_volt" {
			continue
		}
		s := readTrim(filepath.Join(m, "in0_lcrit_alarm"))
		if s == "" {
			return false, false
		}
		return s != "0", true
	}
	return false, false
}

// filesystems lists the filesystems on block devices, one per device (a
// device also bind-mounted elsewhere, as meshd's sandbox does with /usr and
// /etc, is shown at its shortest mount point), with their space.
func (r *Reader) filesystems() []status.Filesystem {
	b, err := os.ReadFile(filepath.Join(r.proc, "self/mountinfo"))
	if err != nil {
		return []status.Filesystem{}
	}
	type mount struct {
		dev, root, point, fstype, source string
	}
	best := map[string]mount{}
	var order []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		sep := -1
		for i, x := range f {
			if x == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(f) < sep+3 {
			continue
		}
		m := mount{dev: f[2], root: f[3], point: unescape(f[4]), fstype: f[sep+1], source: f[sep+2]}
		if !strings.HasPrefix(m.source, "/dev/") || m.fstype == "squashfs" || strings.HasPrefix(m.point, "/snap/") {
			continue
		}
		cur, seen := best[m.dev]
		if !seen {
			order = append(order, m.dev)
		}
		// The device's own root first, then the shortest mount point.
		if !seen || (m.root == "/") != (cur.root == "/") && m.root == "/" ||
			(m.root == "/") == (cur.root == "/") && len(m.point) < len(cur.point) {
			best[m.dev] = m
		}
	}
	out := []status.Filesystem{}
	for _, dev := range order {
		m := best[dev]
		var st syscall.Statfs_t
		if err := r.statfs(m.point, &st); err != nil || st.Blocks == 0 {
			continue
		}
		bs := uint64(st.Bsize)
		out = append(out, status.Filesystem{Mount: m.point, Device: m.source, Type: m.fstype,
			Size: st.Blocks * bs, Used: (st.Blocks - st.Bfree) * bs, Avail: st.Bavail * bs})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

// unescape undoes mountinfo's octal escapes ("\040" for a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
