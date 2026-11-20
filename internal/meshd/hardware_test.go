package meshd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/pcie"
	"meridian/trellis/internal/radio"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/sysfs"
)

// hwTree is a sysfs with a dual-radio card behind a BCM2711 root port, a
// temperature sensor and a cooling device on each radio.
func hwTree(t *testing.T, speed string, tempMilli, throttle string) string {
	t.Helper()
	root := t.TempDir()
	write := func(dir, name, v string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	port := filepath.Join(root, "devices", "platform", "scb", "fd500000.pcie", "pci0000:00", "0000:00:00.0")
	card := filepath.Join(port, "0000:01:00.0")
	for dir, id := range map[string][2]string{port: {"0x14e4", "0x2711"}, card: {"0x14c3", "0x7906"}} {
		write(dir, "vendor", id[0])
		write(dir, "device", id[1])
		write(dir, "current_link_speed", speed)
		write(dir, "current_link_width", "1")
		write(dir, "max_link_speed", "5.0 GT/s PCIe")
		write(dir, "max_link_width", "1")
		write(dir, "aer_dev_correctable", "TOTAL_ERR_COR 0")
		write(dir, "aer_dev_nonfatal", "TOTAL_ERR_NONFATAL 0")
		write(dir, "aer_dev_fatal", "TOTAL_ERR_FATAL 0")
	}
	if err := os.MkdirAll(filepath.Join(root, "bus", "pci", "devices"), 0o755); err != nil {
		t.Fatal(err)
	}
	for addr, dir := range map[string]string{"0000:00:00.0": port, "0000:01:00.0": card} {
		if err := os.Symlink(dir, filepath.Join(root, "bus", "pci", "devices", addr)); err != nil {
			t.Fatal(err)
		}
	}
	for _, phy := range []string{"phy0", "phy1"} {
		d := filepath.Join(root, "class", "ieee80211", phy)
		write(filepath.Join(d, "hwmon2"), "temp1_input", tempMilli)
		write(filepath.Join(d, "hwmon2"), "temp1_max", "120000")
		write(filepath.Join(d, "hwmon2"), "temp1_crit", "110000")
		write(filepath.Join(d, "hwmon2"), "throttle1", throttle)
		write(filepath.Join(d, "cooling_device"), "cur_state", "0")
		write(filepath.Join(d, "cooling_device"), "max_state", "99")
		if err := os.Symlink(card, filepath.Join(d, "device")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func hwDaemon(t *testing.T, root string) (*Daemon, *hardwareReconciler) {
	t.Helper()
	db, err := hwdb.Load()
	if err != nil {
		t.Fatal(err)
	}
	mt := db.Lookup("pci", "14c3:7906", "mt7915e")
	dev := sysfs.Device{Bus: sysfs.BusPCI, Port: "pci:0000:01:00.0", VendorID: "14c3", ProductID: "7906", Driver: "mt7915e"}
	d := &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), opts: Options{SysRoot: root}, facts: &Facts{}}
	d.facts.Radios = []*radio.Radio{
		{Fingerprint: "pci:0000:01:00.0#2g", Phy: "phy0", Device: dev, HWDB: mt, Caps: radio.Caps{Bands: []string{"2g"}}},
		{Fingerprint: "pci:0000:01:00.0#5g", Phy: "phy1", Device: dev, HWDB: mt, Caps: radio.Caps{Bands: []string{"5g"}}},
	}
	r := newHardwareReconciler(d)
	r.reportPath = filepath.Join(t.TempDir(), "links.json")
	return d, r
}

func codes(res reconcile.Result) string {
	var out []string
	for _, w := range res.Warnings {
		out = append(out, w.Level+":"+w.Code)
	}
	return strings.Join(out, " ")
}

func TestHardwareHealthyGen1(t *testing.T) {
	d, r := hwDaemon(t, hwTree(t, "2.5 GT/s PCIe", "54000", "100"))
	started := 0
	r.startUnit = func(context.Context) error { started++; return nil }
	res, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Gen1 on this board with this card is what Trellis sets: no notice.
	if len(res.Warnings) != 0 {
		t.Errorf("warnings: %+v", res.Warnings)
	}
	if started != 1 {
		t.Errorf("the watch was started %d times", started)
	}
	// Not asked for again at once.
	_, _ = r.Reconcile(context.Background())
	if started != 1 {
		t.Errorf("the watch was started again at once")
	}
	hw := d.facts.Hardware
	a, b := hw.PCIe["pci:0000:01:00.0#2g"], hw.PCIe["pci:0000:01:00.0#5g"]
	if a == nil || a != b {
		t.Fatalf("the card's two radios do not share one link: %v %v", a, b)
	}
	if a.Gen != 1 || a.MaxGen != 2 || a.Health != pcie.HealthUnknown || !a.Counted {
		t.Errorf("link = %+v", a)
	}
	th := hw.Thermal["pci:0000:01:00.0#5g"]
	if th == nil || th.TempC != 54 || th.ThrottleAtC != 120 || th.DutyPct != 100 || th.Throttling {
		t.Errorf("thermal = %+v", th)
	}
}

func TestHardwareWarnings(t *testing.T) {
	d, r := hwDaemon(t, hwTree(t, "5.0 GT/s PCIe", "121000", "50"))
	r.startUnit = func(context.Context) error { t.Error("the watch was started with a fresh report"); return nil }
	rep := pcie.Report{Time: time.Now(), Links: []pcie.Health{{
		Device: "0000:01:00.0", Port: "0000:00:00.0", Time: time.Now(), Mode: pcie.ModeRegisters,
		Windows: 100, WindowMS: 10, ReplayWindows: 14, PortErrors: []string{"BadTLP", "Rollover"}, PeakReplayPct: 70,
	}}}
	b, _ := json.Marshal(rep)
	if err := os.WriteFile(r.reportPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := codes(res)
	// One link warning for the card, one throttling warning per radio.
	want := "warning:radio-throttling warning:pcie-link-marginal warning:pcie-gen2 warning:radio-throttling"
	if got != want {
		t.Errorf("warnings = %s\nwant       %s", got, want)
	}
	for _, w := range res.Warnings {
		if w.Code == "pcie-link-marginal" && !strings.Contains(w.Message, "14% of 10 ms windows") {
			t.Errorf("marginal: %s", w.Message)
		}
		if w.Code == "radio-throttling" && !strings.Contains(w.Message, "50%") {
			t.Errorf("throttling: %s", w.Message)
		}
	}
	l := d.facts.Hardware.PCIe["pci:0000:01:00.0#2g"]
	if l.Health != pcie.HealthMarginal || l.ReplayPct != 14 || l.PeakReplayPct != 70 || l.Gen != 2 {
		t.Errorf("link = %+v", l)
	}
	// A lab never starts units on its host.
	d2, r2 := hwDaemon(t, hwTree(t, "2.5 GT/s PCIe", "40000", "100"))
	d2.opts.NoHostConfig = true
	r2.startUnit = func(context.Context) error { t.Error("a lab started the watch"); return nil }
	if _, err := r2.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A hot radio that is not throttled yet is a notice, and a radio with no
// sensor says nothing.
func TestHardwareHotAndNoSensor(t *testing.T) {
	root := hwTree(t, "2.5 GT/s PCIe", "112000", "100")
	d, r := hwDaemon(t, root)
	r.startUnit = func(context.Context) error { return nil }
	d.facts.Radios = append(d.facts.Radios, &radio.Radio{Fingerprint: "usb", Phy: "phy9", Device: sysfs.Device{Bus: sysfs.BusUSB}})
	res, _ := r.Reconcile(context.Background())
	if got := codes(res); got != "info:radio-hot info:radio-hot" {
		t.Errorf("warnings = %s", got)
	}
	if _, ok := d.facts.Hardware.Thermal["usb"]; ok {
		t.Error("a radio without a sensor has a temperature")
	}
	// The cooling device alone says throttling too.
	if err := os.WriteFile(filepath.Join(root, "class", "ieee80211", "phy0", "cooling_device", "cur_state"), []byte("25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	th, ok := sysfs.RadioThermal(root, "phy0")
	if !ok || th.DutyPct != 75 || !th.Throttling() {
		t.Errorf("cooling state 25 of 99: %+v", th)
	}
	// A phy that has not started reads throttle1 0: not a duty.
	idle := hwTree(t, "2.5 GT/s PCIe", "61400", "0")
	if th, ok := sysfs.RadioThermal(idle, "phy0"); !ok || th.DutyPct != 100 || th.Throttling() {
		t.Errorf("idle phy, throttle1 0: %+v", th)
	}
}

// The gossip record carries no temperature, only the radios that are
// degraded and why; a healthy node lists none.
func TestGossipDegraded(t *testing.T) {
	d, r := hwDaemon(t, hwTree(t, "2.5 GT/s PCIe", "61400", "100"))
	r.startUnit = func(context.Context) error { return nil }
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g := d.gossipDegraded(); g != nil {
		t.Fatalf("degraded on a healthy node: %+v", g)
	}

	// Throttling, and at Gen2 on the CM4: both radios, both reasons.
	d, r = hwDaemon(t, hwTree(t, "5.0 GT/s PCIe", "121000", "50"))
	r.startUnit = func(context.Context) error { return nil }
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d.gossipDegraded())
	want := `[{"r":"2g","w":"pcie"},{"r":"2g","w":"thermal"},{"r":"5g","w":"pcie"},{"r":"5g","w":"thermal"}]`
	if string(b) != want {
		t.Errorf("degraded = %s\nwant       %s", b, want)
	}
	// An ignored radio is not reported.
	d.facts.Radios[0].Ignored = true
	if g := d.gossipDegraded(); len(g) != 2 || g[0].Radio != "5g" {
		t.Errorf("with 2g ignored: %+v", g)
	}
	a := &gossip.Record{V: 1, Degraded: d.gossipDegraded()}
	if gossip.Same(a, &gossip.Record{V: 1}) {
		t.Error("a degraded radio did not make a new record")
	}
}

// The temperature is read once a minute, not on every pass.
func TestHardwareThermalReadSpaced(t *testing.T) {
	root := hwTree(t, "2.5 GT/s PCIe", "50000", "100")
	d, r := hwDaemon(t, root)
	r.startUnit = func(context.Context) error { return nil }
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	temp := func() float64 {
		if _, err := r.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		return d.facts.Hardware.Thermal["pci:0000:01:00.0#5g"].TempC
	}
	if got := temp(); got != 50 {
		t.Fatalf("first read: %v", got)
	}
	for _, phy := range []string{"phy0", "phy1"} {
		if err := os.WriteFile(filepath.Join(root, "class", "ieee80211", phy, "hwmon2", "temp1_input"), []byte("70000\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(30 * time.Second)
	if got := temp(); got != 50 {
		t.Errorf("read again after 30 s: %v", got)
	}
	now = now.Add(31 * time.Second)
	if got := temp(); got != 70 {
		t.Errorf("after a minute: %v", got)
	}
}
