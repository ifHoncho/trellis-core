package pcie

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tree builds a sysfs with a root port and a card behind it, both radios
// of the card on the one device.
func tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	port := filepath.Join(root, "devices", "platform", "scb", "fd500000.pcie", "pci0000:00", "0000:00:00.0")
	card := filepath.Join(port, "0000:01:00.0")
	write := func(dir, name, v string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(port, "vendor", "0x14e4")
	write(port, "device", "0x2711")
	write(port, "current_link_speed", "2.5 GT/s PCIe")
	write(port, "current_link_width", "1")
	write(port, "max_link_speed", "5.0 GT/s PCIe")
	write(port, "max_link_width", "1")
	write(port, "aer_dev_correctable", "RxErr 0\nBadTLP 2\nTOTAL_ERR_COR 2")
	write(port, "aer_dev_fatal", "DLP 0\nTOTAL_ERR_FATAL 0")
	write(port, "aer_dev_nonfatal", "TOTAL_ERR_NONFATAL 0")
	write(card, "vendor", "0x14c3")
	write(card, "device", "0x7906")
	write(card, "current_link_speed", "2.5 GT/s PCIe")
	write(card, "current_link_width", "1")
	write(card, "max_link_speed", "5.0 GT/s PCIe")
	write(card, "max_link_width", "2")
	write(card, "aer_dev_correctable", "Rollover 3\nTOTAL_ERR_COR 3")
	write(card, "aer_dev_fatal", "TOTAL_ERR_FATAL 1")
	write(card, "aer_dev_nonfatal", "TOTAL_ERR_NONFATAL 0")
	bus := filepath.Join(root, "bus", "pci", "devices")
	if err := os.MkdirAll(bus, 0o755); err != nil {
		t.Fatal(err)
	}
	for addr, dir := range map[string]string{"0000:00:00.0": port, "0000:01:00.0": card} {
		if err := os.Symlink(dir, filepath.Join(bus, addr)); err != nil {
			t.Fatal(err)
		}
	}
	for _, phy := range []string{"phy0", "phy1"} {
		d := filepath.Join(root, "class", "ieee80211", phy)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(card, filepath.Join(d, "device")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestReadLink(t *testing.T) {
	root := tree(t)
	l, err := ReadLink(root, "0000:01:00.0")
	if err != nil {
		t.Fatal(err)
	}
	if l.Port != "0000:00:00.0" || l.PortID != "14e4:2711" {
		t.Errorf("port = %q %q", l.Port, l.PortID)
	}
	if l.SpeedGTs != 2.5 || l.MaxSpeedGTs != 5 || l.Width != 1 {
		t.Errorf("speed %v of %v, width %d", l.SpeedGTs, l.MaxSpeedGTs, l.Width)
	}
	// The card can do two lanes, the port one: the link can do one.
	if l.MaxWidth != 1 {
		t.Errorf("max width = %d, want the port's 1", l.MaxWidth)
	}
	if !l.Counted || l.Correctable != 5 || l.Fatal != 1 || l.NonFatal != 0 {
		t.Errorf("counters = %+v", l)
	}
	if Gen(l.SpeedGTs) != 1 || Gen(l.MaxSpeedGTs) != 2 || Gen(8) != 3 || Gen(0) != 0 {
		t.Errorf("generations wrong")
	}
	if _, err := ReadLink(root, "../../etc"); err == nil {
		t.Error("a bad address was read")
	}
	if got := RadioDevices(root); len(got) != 1 || got[0] != "0000:01:00.0" {
		t.Errorf("RadioDevices = %v", got)
	}
	if p := UpstreamPort(root, "0000:00:00.0"); p != "" {
		t.Errorf("the root port's upstream port = %q", p)
	}
}

// space is a configuration space whose correctable status is
// write-one-to-clear, and which sets bits again before chosen reads.
type space struct {
	b     []byte
	limit int // bytes a reader gets
	aer   int64
	// onRead is called before each read of the correctable status and
	// returns the bits that came up since the last.
	onRead func(n int) uint32
	reads  int
	writes int
}

func newSpace(devCtl uint16) *space {
	s := &space{b: make([]byte, 4096), limit: 4096, aer: 0x100}
	binary.LittleEndian.PutUint16(s.b[0x06:], 0x10) // capability list
	s.b[0x34] = 0x40
	s.b[0x40], s.b[0x41] = 0x01, 0x50 // power management, next 0x50
	s.b[0x50], s.b[0x51] = 0x10, 0x00 // PCI Express
	binary.LittleEndian.PutUint16(s.b[0x50+pcieDevCtl:], devCtl)
	// A vendor capability first, AER second.
	binary.LittleEndian.PutUint32(s.b[0x100:], 0x000b|1<<16|0x148<<20)
	binary.LittleEndian.PutUint32(s.b[0x148:], 0x0001|2<<16)
	s.aer = 0x148
	return s
}

func (s *space) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > int64(s.limit) {
		return 0, io.EOF
	}
	if off == s.aer+aerCor && s.onRead != nil {
		v := binary.LittleEndian.Uint32(s.b[off:]) | s.onRead(s.reads)
		binary.LittleEndian.PutUint32(s.b[off:], v)
		s.reads++
	}
	return copy(p, s.b[off:]), nil
}

func (s *space) WriteAt(p []byte, off int64) (int, error) {
	if off == s.aer+aerCor && len(p) == 4 {
		v := binary.LittleEndian.Uint32(s.b[off:]) &^ binary.LittleEndian.Uint32(p)
		binary.LittleEndian.PutUint32(s.b[off:], v)
		s.writes++
		return 4, nil
	}
	return 0, errors.New("write to a register the sampler must not touch")
}

func noSleep(time.Duration) {}

func TestSampleCountsRollovers(t *testing.T) {
	port := newSpace(0)
	// Stale bits before the sample; then a rollover in windows 0, 1 and 5
	// (reads 1, 2 and 6: read 0 is the clearing one), a bad TLP in 3.
	port.b[port.aer+aerCor] = CorBadDLLP
	port.onRead = func(n int) uint32 {
		switch n {
		case 1, 2, 6:
			return CorRollover | CorReplayTimer
		case 4:
			return CorBadTLP
		}
		return 0
	}
	binary.LittleEndian.PutUint32(port.b[port.aer+aerUncor:], 1<<14)
	dev := newSpace(0)
	dev.onRead = func(n int) uint32 {
		if n == 1 {
			return CorReplayTimer
		}
		return 0
	}
	pr, err := Find(port)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := Find(dev)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Sample(pr, dr, 10, 10*time.Millisecond, noSleep)
	if err != nil {
		t.Fatal(err)
	}
	if h.Mode != ModeRegisters || h.Windows != 10 || h.ReplayWindows != 3 || h.WindowMS != 10 {
		t.Fatalf("sample = %+v", h)
	}
	if got := strings.Join(h.PortErrors, ","); got != "BadTLP,Rollover,Timeout" {
		t.Errorf("port errors = %s (the stale BadDLLP must not count)", got)
	}
	if got := strings.Join(h.DeviceErrors, ","); got != "Timeout" {
		t.Errorf("device errors = %s", got)
	}
	if got := strings.Join(h.PortFatal, ","); got != "CmpltTO" {
		t.Errorf("port fatal = %s", got)
	}
	if !h.Marginal() || h.ReplayPct() != 30 {
		t.Errorf("marginal %v, %.0f%%", h.Marginal(), h.ReplayPct())
	}
	rate, atLeast := h.ReplayPerSec()
	if atLeast || rate < 35 || rate > 36 { // -ln(0.7)/0.01
		t.Errorf("rate = %.1f, floor %v", rate, atLeast)
	}
	if !strings.Contains(h.Describe(), "rollovers 30% of 10 ms windows") {
		t.Errorf("Describe = %q", h.Describe())
	}
	// The device's end is read twice (clear, then the close), never polled.
	if dev.reads != 2 {
		t.Errorf("device status read %d times", dev.reads)
	}
}

func TestSampleCleanAndSaturated(t *testing.T) {
	clean := newSpace(0)
	r, _ := Find(clean)
	h, err := Sample(r, nil, 20, 10*time.Millisecond, noSleep)
	if err != nil || h.Marginal() || h.Windows != 20 || clean.writes != 0 {
		t.Fatalf("clean link: %+v, %d writes, %v", h, clean.writes, err)
	}
	if h.Describe() != "no errors" {
		t.Errorf("Describe = %q", h.Describe())
	}

	bad := newSpace(0)
	bad.onRead = func(int) uint32 { return CorRollover }
	r, _ = Find(bad)
	h, _ = Sample(r, nil, 100, 10*time.Millisecond, noSleep)
	rate, atLeast := h.ReplayPerSec()
	if h.ReplayWindows != 100 || !atLeast || rate < 460 {
		t.Errorf("saturated: %+v rate %.0f floor %v", h, rate, atLeast)
	}
}

// A port whose errors the kernel handles is left alone.
func TestSampleLeavesReportedPort(t *testing.T) {
	port := newSpace(devCtlCorrReport)
	binary.LittleEndian.PutUint32(port.b[port.aer+aerCor:], CorRollover)
	r, _ := Find(port)
	h, err := Sample(r, nil, 10, time.Millisecond, noSleep)
	if err != nil || h.Mode != ModeKernel || h.Windows != 0 || port.writes != 0 {
		t.Fatalf("reported port: %+v, %d writes, %v", h, port.writes, err)
	}
	if binary.LittleEndian.Uint32(port.b[port.aer+aerCor:]) != CorRollover {
		t.Error("the status was changed")
	}
}

func TestFindNeedsPrivilegeOrAER(t *testing.T) {
	s := newSpace(0)
	s.limit = 64
	if _, err := Find(s); !errors.Is(err, ErrNoAccess) {
		t.Errorf("64 bytes: %v", err)
	}
	s.limit = 256
	if _, err := Find(s); !errors.Is(err, ErrNoAER) {
		t.Errorf("256 bytes: %v", err)
	}
	none := newSpace(0)
	binary.LittleEndian.PutUint32(none.b[0x148:], 0x0019|1<<16)
	if _, err := Find(none); !errors.Is(err, ErrNoAER) {
		t.Errorf("no AER capability: %v", err)
	}
}

func TestWatchWritesReportWithPeak(t *testing.T) {
	root := tree(t)
	path := filepath.Join(t.TempDir(), "run", "links.json")
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	done := make(chan struct{})
	sample := func(addr string) Health {
		n++
		h := Health{Device: addr, Time: time.Now(), Mode: ModeRegisters, Windows: 100, WindowMS: 10}
		if n == 1 {
			h.ReplayWindows = 80
		}
		if n == 3 {
			close(done)
		}
		return h
	}
	errc := make(chan error, 1)
	go func() { errc <- Watch(ctx, root, path, time.Millisecond, sample) }()
	<-done
	var rep *Report
	for i := 0; i < 200 && (rep == nil || len(rep.Links) == 0 || rep.Links[0].ReplayWindows != 0); i++ {
		time.Sleep(5 * time.Millisecond)
		rep = ReadReport(path, time.Now())
	}
	if rep == nil {
		t.Fatal("no report")
	}
	h, ok := rep.Find("0000:01:00.0")
	if !ok || h.ReplayWindows != 0 || h.PeakReplayPct != 80 {
		t.Errorf("report = %+v", rep)
	}
	if ReadReport(path, time.Now().Add(StaleAfter+time.Second)) != nil {
		t.Error("a stale report was read")
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the report outlived the watch")
	}
}
