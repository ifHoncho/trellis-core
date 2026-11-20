// Package pcie reads the health of a PCIe link: the speed and width it
// trained at against what both ends can do, the kernel's error counters,
// and the error status registers themselves.
//
// The registers matter where the kernel does not count: a port whose
// correctable error reporting is off raises no interrupt, so its counters
// stay at zero while the link corrupts packets and recovers by replaying
// them. A link in that state carries a radio until traffic pushes it past
// what replay can recover, and the card then stops answering.
//
// Link facts and counters are readable by anyone. The registers sit past
// the first 64 bytes of a device's configuration space, which sysfs gives
// only to a process with CAP_SYS_ADMIN: the watch (Watch) runs with it and
// writes what it finds to a file others read.
package pcie

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Link is a PCIe device's link to its upstream port.
type Link struct {
	Device string `json:"device"`         // 0000:01:00.0
	Port   string `json:"port,omitempty"` // the upstream port, when there is one
	// SpeedGTs and Width are what the link trained at; MaxSpeedGTs and
	// MaxWidth the most both ends support. Zero when sysfs does not say.
	SpeedGTs    float64 `json:"speed_gts,omitempty"`
	Width       int     `json:"width,omitempty"`
	MaxSpeedGTs float64 `json:"max_speed_gts,omitempty"`
	MaxWidth    int     `json:"max_width,omitempty"`
	// Counted: the kernel exposes error counters for the device or its
	// port. Correctable, NonFatal and Fatal are their totals, both ends
	// added.
	Counted     bool   `json:"counted"`
	Correctable uint64 `json:"correctable"`
	NonFatal    uint64 `json:"nonfatal"`
	Fatal       uint64 `json:"fatal"`
	// PortID is the upstream port's "vendor:device", lower-case hex.
	PortID string `json:"port_id,omitempty"`
}

// Gen is the PCIe generation of a link speed: 1 for 2.5 GT/s, 2 for 5,
// 3 for 8, 4 for 16, 5 for 32; 0 when unknown.
func Gen(gts float64) int {
	switch {
	case gts <= 0:
		return 0
	case gts < 4:
		return 1
	case gts < 7:
		return 2
	case gts < 12:
		return 3
	case gts < 24:
		return 4
	case gts < 48:
		return 5
	}
	return 6
}

var reAddr = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// devDir is a device's sysfs directory, resolved (the entry under
// bus/pci/devices is a link into the device tree).
func devDir(sysRoot, addr string) (string, error) {
	if !reAddr.MatchString(addr) {
		return "", fmt.Errorf("%q is not a PCI address", addr)
	}
	return filepath.EvalSymlinks(filepath.Join(sysRoot, "bus", "pci", "devices", addr))
}

// UpstreamPort returns the address of the port a device hangs from, or ""
// for one directly on a host bridge.
func UpstreamPort(sysRoot, addr string) string {
	dir, err := devDir(sysRoot, addr)
	if err != nil {
		return ""
	}
	if p := filepath.Base(filepath.Dir(dir)); reAddr.MatchString(p) {
		return p
	}
	return ""
}

// ReadLink reads a device's link facts and the kernel's error counters.
func ReadLink(sysRoot, addr string) (Link, error) {
	dir, err := devDir(sysRoot, addr)
	if err != nil {
		return Link{}, err
	}
	l := Link{Device: addr, Port: UpstreamPort(sysRoot, addr)}
	l.SpeedGTs = parseSpeed(readTrim(dir, "current_link_speed"))
	l.Width, _ = strconv.Atoi(readTrim(dir, "current_link_width"))
	l.MaxSpeedGTs = parseSpeed(readTrim(dir, "max_link_speed"))
	l.MaxWidth, _ = strconv.Atoi(readTrim(dir, "max_link_width"))
	dirs := []string{dir}
	if l.Port != "" {
		pdir := filepath.Dir(dir)
		dirs = append(dirs, pdir)
		// The link can do no more than its slower, narrower end.
		if s := parseSpeed(readTrim(pdir, "max_link_speed")); s > 0 && (l.MaxSpeedGTs == 0 || s < l.MaxSpeedGTs) {
			l.MaxSpeedGTs = s
		}
		if w, _ := strconv.Atoi(readTrim(pdir, "max_link_width")); w > 0 && (l.MaxWidth == 0 || w < l.MaxWidth) {
			l.MaxWidth = w
		}
		v, d := strings.TrimPrefix(readTrim(pdir, "vendor"), "0x"), strings.TrimPrefix(readTrim(pdir, "device"), "0x")
		if v != "" && d != "" {
			l.PortID = v + ":" + d
		}
	}
	for _, d := range dirs {
		for file, total := range map[string]*uint64{
			"aer_dev_correctable": &l.Correctable, "aer_dev_nonfatal": &l.NonFatal, "aer_dev_fatal": &l.Fatal,
		} {
			if n, ok := aerTotal(d, file); ok {
				l.Counted = true
				*total += n
			}
		}
	}
	return l, nil
}

// parseSpeed reads "2.5 GT/s PCIe" (older kernels: "2.5 GT/s").
func parseSpeed(s string) float64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// aerTotal reads the TOTAL_ line of one of the kernel's AER counter files.
func aerTotal(dir, file string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.HasPrefix(f[0], "TOTAL_") {
			n, err := strconv.ParseUint(f[1], 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

func readTrim(dir, file string) string {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// RadioDevices lists the PCI addresses of the devices that carry a wiphy,
// sorted. A card with two radios is listed once.
func RadioDevices(sysRoot string) []string {
	phys, _ := filepath.Glob(filepath.Join(sysRoot, "class", "ieee80211", "*", "device"))
	seen := map[string]bool{}
	var out []string
	for _, p := range phys {
		dir, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		addr := filepath.Base(dir)
		if !reAddr.MatchString(addr) || seen[addr] || !exists(filepath.Join(dir, "vendor")) {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Configuration space ----------------------------------------------------------

// ErrNoAccess: the configuration space past its first 64 bytes cannot be
// read (sysfs gives it to CAP_SYS_ADMIN only).
var ErrNoAccess = errors.New("the device's extended configuration space is not readable (root with CAP_SYS_ADMIN only)")

// ErrNoAER: the device has no Advanced Error Reporting capability.
var ErrNoAER = errors.New("the device has no AER capability")

// Space is a device's configuration space.
type Space interface {
	io.ReaderAt
	io.WriterAt
}

const (
	capPCIe    = 0x10
	extCapAER  = 0x0001
	pcieDevCtl = 0x08 // in the PCI Express capability
	aerUncor   = 0x04 // in the AER capability
	aerCor     = 0x10
	// devCtlCorrReport: the device reports correctable errors (to the
	// kernel's or the firmware's handler, which then also clears them).
	devCtlCorrReport = 1 << 0
)

// Correctable error status bits.
const (
	CorReceiver     = 1 << 0
	CorBadTLP       = 1 << 6
	CorBadDLLP      = 1 << 7
	CorRollover     = 1 << 8 // REPLAY_NUM rollover: a packet went unacknowledged four times
	CorReplayTimer  = 1 << 12
	CorAdvisory     = 1 << 13
	CorInternal     = 1 << 14
	CorHeaderLogOvf = 1 << 15
)

var corNames = []struct {
	bit  uint32
	name string
}{
	{CorReceiver, "RxErr"}, {CorBadTLP, "BadTLP"}, {CorBadDLLP, "BadDLLP"}, {CorRollover, "Rollover"},
	{CorReplayTimer, "Timeout"}, {CorAdvisory, "NonFatalErr"}, {CorInternal, "CorrIntErr"}, {CorHeaderLogOvf, "HeaderOF"},
}

var uncorNames = []struct {
	bit  uint32
	name string
}{
	{1 << 4, "DLP"}, {1 << 5, "SDES"}, {1 << 12, "TLP"}, {1 << 13, "FCP"}, {1 << 14, "CmpltTO"}, {1 << 15, "CmpltAbrt"},
	{1 << 16, "UnxCmplt"}, {1 << 17, "RxOF"}, {1 << 18, "MalfTLP"}, {1 << 19, "ECRC"}, {1 << 20, "UnsupReq"}, {1 << 21, "ACSViol"},
	{1 << 22, "UncorrIntErr"},
}

// CorNames names the bits set in a correctable error status, as the
// kernel's counters name them.
func CorNames(v uint32) []string { return bitNames(v, corNames) }

// UncorNames names the bits set in an uncorrectable error status.
func UncorNames(v uint32) []string { return bitNames(v, uncorNames) }

func bitNames(v uint32, names []struct {
	bit  uint32
	name string
}) []string {
	out := []string{}
	for _, n := range names {
		if v&n.bit != 0 {
			out = append(out, n.name)
		}
	}
	return out
}

func read8(s Space, off int64) (uint8, error) {
	var b [1]byte
	if _, err := s.ReadAt(b[:], off); err != nil {
		return 0, err
	}
	return b[0], nil
}

func read16(s Space, off int64) (uint16, error) {
	var b [2]byte
	if _, err := s.ReadAt(b[:], off); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b[:]), nil
}

func read32(s Space, off int64) (uint32, error) {
	var b [4]byte
	if _, err := s.ReadAt(b[:], off); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

func write32(s Space, off int64, v uint32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, err := s.WriteAt(b[:], off)
	return err
}

// findCap returns the offset of a capability in the first 256 bytes, or 0.
func findCap(s Space, id uint8) int64 {
	status, err := read16(s, 0x06)
	if err != nil || status&0x10 == 0 {
		return 0
	}
	ptr, err := read8(s, 0x34)
	for i := 0; err == nil && ptr >= 0x40 && i < 48; i++ {
		ptr &^= 3
		var cid uint8
		if cid, err = read8(s, int64(ptr)); err != nil {
			return 0
		}
		if cid == id {
			return int64(ptr)
		}
		ptr, err = read8(s, int64(ptr)+1)
	}
	return 0
}

// findExtCap returns the offset of an extended capability, or an error:
// ErrNoAccess when the space ends at 64 or 256 bytes for this reader.
func findExtCap(s Space, id uint16) (int64, error) {
	off := int64(0x100)
	for i := 0; i < 96; i++ {
		h, err := read32(s, off)
		if err != nil {
			if off != 0x100 {
				return 0, err
			}
			// Nothing at 0x100: a reader without the privilege gets 64
			// bytes; a device without extended capabilities has 256.
			if _, err := read8(s, 0x40); err != nil {
				return 0, ErrNoAccess
			}
			return 0, ErrNoAER
		}
		if h == 0 || h == 0xffffffff {
			break
		}
		if uint16(h) == id {
			return off, nil
		}
		next := int64(h >> 20 &^ 3)
		if next < 0x100 {
			break
		}
		off = next
	}
	return 0, ErrNoAER
}

// Registers are a device's AER registers, found once.
type Registers struct {
	s      Space
	aer    int64
	devCtl int64 // 0 when the device has no PCI Express capability
}

// Find locates a device's AER registers.
func Find(s Space) (*Registers, error) {
	aer, err := findExtCap(s, extCapAER)
	if err != nil {
		return nil, err
	}
	r := &Registers{s: s, aer: aer}
	if c := findCap(s, capPCIe); c != 0 {
		r.devCtl = c + pcieDevCtl
	}
	return r, nil
}

// Correctable reads the correctable error status.
func (r *Registers) Correctable() (uint32, error) { return read32(r.s, r.aer+aerCor) }

// Uncorrectable reads the uncorrectable error status.
func (r *Registers) Uncorrectable() (uint32, error) { return read32(r.s, r.aer+aerUncor) }

// ClearCorrectable clears the given correctable status bits (they are
// write-one-to-clear).
func (r *Registers) ClearCorrectable(bits uint32) error { return write32(r.s, r.aer+aerCor, bits) }

// Reported says whether the device reports its correctable errors: someone
// (the kernel's AER service, or firmware) then counts and clears them, and
// clearing them here would hide errors from it.
func (r *Registers) Reported() bool {
	if r.devCtl == 0 {
		return false
	}
	v, err := read16(r.s, r.devCtl)
	return err == nil && v&devCtlCorrReport != 0
}

// OpenSpace opens a device's configuration space for reading and writing.
func OpenSpace(sysRoot, addr string) (*os.File, error) {
	dir, err := devDir(sysRoot, addr)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, "config"), os.O_RDWR, 0)
}

// Health -----------------------------------------------------------------------

// Sampling modes.
const (
	// ModeRegisters: the status registers were polled and cleared.
	ModeRegisters = "registers"
	// ModeKernel: the kernel handles the port's correctable errors; its
	// counters are the measure, and the registers were left alone.
	ModeKernel = "kernel"
)

// Health is what one sample of a link's error status found.
type Health struct {
	Device string    `json:"device"`
	Port   string    `json:"port,omitempty"`
	Time   time.Time `json:"time"`
	Mode   string    `json:"mode,omitempty"`
	// Windows is how many times the port's correctable status was read
	// and cleared, WindowMS apart; ReplayWindows in how many of them a
	// REPLAY_NUM rollover had been set since the read before.
	Windows       int `json:"windows,omitempty"`
	WindowMS      int `json:"window_ms,omitempty"`
	ReplayWindows int `json:"replay_windows,omitempty"`
	// PortErrors and DeviceErrors name the correctable errors seen at each
	// end during the sample; PortFatal and DeviceFatal the uncorrectable
	// ones standing in their status (not cleared here).
	PortErrors   []string `json:"port_errors,omitempty"`
	DeviceErrors []string `json:"device_errors,omitempty"`
	PortFatal    []string `json:"port_fatal,omitempty"`
	DeviceFatal  []string `json:"device_fatal,omitempty"`
	// PeakReplayPct is the highest ReplayPct of the samples the watch
	// took in the last PeakWindow, this one included.
	PeakReplayPct float64 `json:"peak_replay_pct,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// ReplayPct is the share of sample windows with a replay rollover, in
// percent.
func (h Health) ReplayPct() float64 {
	if h.Windows == 0 {
		return 0
	}
	return 100 * float64(h.ReplayWindows) / float64(h.Windows)
}

// ReplayPerSec estimates rollovers per second from the share of windows
// that saw one, taking them to arrive at random. atLeast is set when every
// window saw one: the figure is then a floor.
func (h Health) ReplayPerSec() (rate float64, atLeast bool) {
	if h.Windows == 0 || h.WindowMS == 0 || h.ReplayWindows == 0 {
		return 0, false
	}
	w := float64(h.WindowMS) / 1000
	if h.ReplayWindows >= h.Windows {
		return math.Log(float64(h.Windows)) / w, true
	}
	p := float64(h.ReplayWindows) / float64(h.Windows)
	return -math.Log(1-p) / w, false
}

// Marginal says whether the sample shows a link losing packets: any replay
// rollover or replay timeout, at either end.
func (h Health) Marginal() bool {
	if h.ReplayWindows > 0 {
		return true
	}
	for _, l := range [][]string{h.PortErrors, h.DeviceErrors} {
		for _, e := range l {
			if e == "Rollover" || e == "Timeout" {
				return true
			}
		}
	}
	return false
}

// Describe is the sample in a few words, for a status line.
func (h Health) Describe() string {
	switch {
	case h.Error != "":
		return "not measured: " + h.Error
	case h.Mode == ModeKernel:
		return "errors counted by the kernel"
	case h.Windows == 0:
		return "not measured"
	}
	errs := append(append([]string{}, h.PortErrors...), h.DeviceErrors...)
	sort.Strings(errs)
	errs = uniq(errs)
	if !h.Marginal() {
		if len(errs) > 0 {
			return "no replay rollovers; corrected: " + strings.Join(errs, ", ")
		}
		return "no errors"
	}
	rate, atLeast := h.ReplayPerSec()
	s := fmt.Sprintf("replay rollovers %.0f%% of %d ms windows", h.ReplayPct(), h.WindowMS)
	var more []string
	switch {
	case atLeast:
		more = append(more, fmt.Sprintf(">%.0f/s", rate))
	case rate > 0:
		more = append(more, fmt.Sprintf("~%.0f/s", rate))
	}
	if h.PeakReplayPct > h.ReplayPct()+0.5 {
		more = append(more, fmt.Sprintf("peak %.0f%%", h.PeakReplayPct))
	}
	if len(more) > 0 {
		s += " (" + strings.Join(more, ", ") + ")"
	}
	return s
}

func uniq(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// Sample measures a link for n windows of the given length. port is the
// upstream port's registers, where a rollover means packets from the host
// to the device went unacknowledged; dev the device's own, or nil. Reading
// the port's registers puts nothing on the link itself.
//
// When the port reports its correctable errors the registers are left
// alone (ModeKernel).
func Sample(port, dev *Registers, n int, window time.Duration, sleep func(time.Duration)) (Health, error) {
	h := Health{Time: time.Now()}
	if sleep == nil {
		sleep = time.Sleep
	}
	if u, err := port.Uncorrectable(); err == nil {
		h.PortFatal = UncorNames(u)
	}
	if dev != nil {
		if u, err := dev.Uncorrectable(); err == nil {
			h.DeviceFatal = UncorNames(u)
		}
	}
	if port.Reported() {
		h.Mode = ModeKernel
		return h, nil
	}
	h.Mode = ModeRegisters
	h.WindowMS = int(window / time.Millisecond)
	// What stands in the status now gathered over an unknown time: clear
	// it, and count from here.
	v, err := port.Correctable()
	if err != nil {
		return h, err
	}
	if v != 0 {
		if err := port.ClearCorrectable(v); err != nil {
			return h, err
		}
	}
	var devSeen uint32
	if dev != nil && !dev.Reported() {
		if v, err := dev.Correctable(); err == nil && v != 0 {
			_ = dev.ClearCorrectable(v)
		}
	}
	var seen uint32
	for i := 0; i < n; i++ {
		sleep(window)
		v, err := port.Correctable()
		if err != nil {
			return h, err
		}
		h.Windows++
		if v == 0 {
			continue
		}
		seen |= v
		if v&CorRollover != 0 {
			h.ReplayWindows++
		}
		if err := port.ClearCorrectable(v); err != nil {
			return h, err
		}
	}
	// The device's end once, at the close: every read of it crosses the
	// link being measured.
	if dev != nil && !dev.Reported() {
		if v, err := dev.Correctable(); err == nil {
			devSeen = v
			if v != 0 {
				_ = dev.ClearCorrectable(v)
			}
		}
	}
	h.PortErrors, h.DeviceErrors = CorNames(seen), CorNames(devSeen)
	return h, nil
}

// SampleDevice opens a radio's device and its upstream port and samples
// the link between them.
func SampleDevice(sysRoot, addr string, n int, window time.Duration) Health {
	h := Health{Device: addr, Port: UpstreamPort(sysRoot, addr), Time: time.Now()}
	if h.Port == "" {
		h.Error = "no upstream port"
		return h
	}
	pf, err := OpenSpace(sysRoot, h.Port)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	defer pf.Close()
	port, err := Find(pf)
	if err != nil {
		h.Error = "port " + h.Port + ": " + err.Error()
		return h
	}
	var dev *Registers
	if df, err := OpenSpace(sysRoot, addr); err == nil {
		defer df.Close()
		dev, _ = Find(df)
	}
	got, err := Sample(port, dev, n, window, nil)
	got.Device, got.Port = h.Device, h.Port
	if err != nil {
		got.Error = err.Error()
	}
	return got
}
