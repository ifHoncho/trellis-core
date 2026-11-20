package crashlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The records below are made up in the shape the kernel writes them.
const serrorDmesg = `Panic#1 Part1
<6>[    0.000000] Booting Linux on physical CPU 0x0000000000 [0x410fd083]
<6>[    0.023671] pstore: Registered ramoops as persistent store backend
<4>[   61.120000] hwmon hwmon1: Undervoltage detected!
<6>[   67.480000] hwmon hwmon1: Voltage normalised
<2>[  163.856978] SError Interrupt on CPU2, code 0x00000000bf000002 -- SError
<4>[  163.857018] pc : el1_interrupt+0x24/0x60
<0>[  163.857098] Kernel panic - not syncing: Asynchronous SError Interrupt
<4>[  163.857113] Call trace:
<4>[  163.857137]  vpanic+0x100/0x2d8
<4>[  163.857153]  arm64_serror_panic+0x78/0x90
<4>[  163.857159]  do_serror+0x44/0x80
<4>[  163.857185]  mt76_mmio_rr+0x28/0xd0 [mt76] (P)
<4>[  163.857229]  mt76_mmio_rmw+0x30/0x80 [mt76]
<4>[  163.857291]  mt7915_wr+0x80/0xc0 [mt7915e]
<4>[  163.857362]  tpt_trig_timer+0xd4/0x100 [mac80211]
<0>[  163.857650] Kernel Offset: 0x2774e00000 from 0xffffffc080000000
`

const serrorConsole = `[  163.856978] SError Interrupt on CPU2, code 0x00000000bf000002 -- SError
[  163.857098] Kernel panic - not syncing: Asynchronous SError Interrupt
[  163.857113] Call trace:
[  163.857185]  mt76_mmio_rr+0x28/0xd0 [mt76] (P)
[  163.857291]  mt7915_wr+0x80/0xc0 [mt7915e]
[  163.863164] ---[ end Kernel panic - not syncing: Asynchronous SError Interrupt ]---
`

const floodDmesg = `Panic#1 Part1
<3>[  301.100000] irq 27: nobody cared (try booting with the "irqpoll" option)
<4>[  301.100100] handlers:
<4>[  301.100200] [<00000000a1b2c3d4>] pcie_pme_irq
<4>[  301.100300] [<00000000b2c3d4e5>] aer_irq
<0>[  301.100400] Disabling IRQ #27
<2>[  302.500000] SError Interrupt on CPU0, code 0x00000000bf000002 -- SError
<0>[  302.500100] Kernel panic - not syncing: Asynchronous SError Interrupt
<4>[  302.500200] Call trace:
<4>[  302.500300]  mt76_mmio_rr+0x28/0xd0 [mt76] (P)
`

const sysrqConsole = `[ 5120.000000] IPv6: ADDRCONF(NETDEV_CHANGE): br-mesh: link becomes ready
[ 5123.400000] sysrq: Resetting
`

const cleanConsole = `[ 9000.000000] systemd-shutdown[1]: Syncing filesystems and block devices.
[ 9000.100000] reboot: Restarting system
`

const oopsDmesg = `Oops#1 Part1
<1>[   42.000000] Unable to handle kernel NULL pointer dereference at virtual address 0000000000000010
<4>[   42.000100] Call trace:
<4>[   42.000200]  some_fn+0x10/0x20 [somedrv]
<0>[   42.000300] Kernel panic - not syncing: Oops: Fatal exception
`

func TestClassify(t *testing.T) {
	cases := []struct {
		name, text, verdict string
		uptime              float64
		in                  []string
	}{
		{"serror", serrorDmesg, CardStopped, 163.856978, []string{"in mt76, mt7915e", "1 undervoltage event"}},
		{"serror console", serrorConsole, CardStopped, 163.856978, []string{"in mt76, mt7915e"}},
		{"flood", floodDmesg, PCIeLink, 302.5, []string{"PCIe error flood", "PCIe link"}},
		{"sysrq", sysrqConsole, SysRq, 5123.4, []string{"serial console"}},
		{"oops", oopsDmesg, Panic, 42, []string{"in somedrv", "Oops: Fatal exception"}},
	}
	for _, c := range cases {
		e, ok := Classify(c.text)
		if !ok || e.Verdict != c.verdict || e.UptimeS != c.uptime {
			t.Errorf("%s: %+v ok=%v", c.name, e, ok)
			continue
		}
		for _, want := range c.in {
			if !strings.Contains(e.Summary, want) {
				t.Errorf("%s: summary %q lacks %q", c.name, e.Summary, want)
			}
		}
	}
	if e, ok := Classify(cleanConsole); ok {
		t.Errorf("a clean shutdown got a verdict: %+v", e)
	}
	if e, ok := Classify(nonFatalOops); ok {
		t.Errorf("an oops the kernel carried on from got a verdict: %+v", e)
	}
}

// An oops the kernel carried on from: ramoops keeps it (dump_oops), and
// the console log of the boot holds it too.
const nonFatalOops = `Oops#1 Part1
<1>[   80.000000] BUG: kernel NULL pointer dereference, address: 0000000000000010
<4>[   80.000100] Call trace:
<4>[   80.000200]  some_fn+0x10/0x20 [somedrv]
`

// No reset is made of an oops the kernel carried on from, or of a
// console log after a clean shutdown; a crash record whose panic line was
// cut off still gives its oops.
func TestOopsIsNotAReset(t *testing.T) {
	state, pstore := t.TempDir(), t.TempDir()
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	opts := func(boot string, now time.Time) Options {
		return Options{StateDir: state, BootID: boot, Now: now, Dirs: []string{pstore}, Backend: true}
	}
	l, _, _ := Open(opts("boot-1", t0))
	_ = l.Stopped(true)
	write(t, pstore, "dmesg-ramoops-0", nonFatalOops, t0.Add(time.Minute))
	write(t, pstore, "console-ramoops-0", strings.TrimPrefix(nonFatalOops, "Oops#1 Part1\n")+cleanConsole, t0.Add(9*time.Minute))
	_, added, _ := Open(opts("boot-2", t0.Add(10*time.Minute)))
	if len(added) != 0 {
		t.Fatalf("after an oops and a clean shutdown: %+v", added)
	}

	// The same oops, then power lost while meshd ran: a power loss, not
	// an oops.
	write(t, pstore, "console-ramoops-0", strings.TrimPrefix(nonFatalOops, "Oops#1 Part1\n"), t0.Add(19*time.Minute))
	_, added, _ = Open(opts("boot-3", t0.Add(20*time.Minute)))
	if len(added) != 1 || added[0].Verdict != PowerLoss {
		t.Fatalf("after an oops and a power cut: %+v", added)
	}

	// A panic record with its panic line cut off: the oops it holds.
	write(t, pstore, "dmesg-ramoops-1", "Panic#1 Part1\n"+strings.TrimPrefix(nonFatalOops, "Oops#1 Part1\n"), t0.Add(29*time.Minute))
	_, added, _ = Open(opts("boot-4", t0.Add(30*time.Minute)))
	if len(added) != 1 || added[0].Verdict != Panic || !strings.Contains(added[0].Summary, "kernel oops in somedrv") {
		t.Fatalf("a cut-off panic record: %+v", added)
	}
}

func write(t *testing.T, dir, name, text string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLogAcrossBoots(t *testing.T) {
	state, pstore := t.TempDir(), t.TempDir()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	opts := func(boot string, now time.Time) Options {
		return Options{StateDir: state, BootID: boot, Now: now, Dirs: []string{pstore, filepath.Join(pstore, "none")}, Backend: true}
	}

	// First start ever: nothing recorded, no boot before to judge.
	l, added, err := Open(opts("boot-1", t0))
	if err != nil || len(added) != 0 || len(l.Entries) != 0 {
		t.Fatalf("first start: %+v %v", added, err)
	}
	// The clock is set a minute in: the boot started at t0.
	if err := l.ClockSet(t0.Add(time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	// meshd restarted in the same boot: still nothing.
	if _, added, _ = Open(opts("boot-1", t0.Add(time.Minute))); len(added) != 0 {
		t.Fatalf("restart in one boot: %+v", added)
	}

	// The node panics 163.86 s into its boot; the next boot finds the
	// kernel log and the console log of the one crash, and dates it by
	// the boot's start, not by the files' times.
	crash := t0.Add(163856978 * time.Microsecond)
	write(t, pstore, "dmesg-ramoops-0", serrorDmesg, t0.Add(-48*time.Hour))
	write(t, pstore, "console-ramoops-0", serrorConsole, t0.Add(-48*time.Hour))
	l, added, err = Open(opts("boot-2", t0.Add(11*time.Minute)))
	if err != nil || len(added) != 1 || added[0].Verdict != CardStopped || !added[0].Time.Equal(crash) || added[0].TimePending || added[0].Record != "dmesg-ramoops-0" {
		t.Fatalf("after the panic: %+v %v", added, err)
	}
	// A restart of meshd, or a later boot with the same records, adds nothing.
	if err := l.Stopped(true); err != nil {
		t.Fatal(err)
	}
	if l, added, _ = Open(opts("boot-3", t0.Add(20*time.Minute))); len(added) != 0 {
		t.Fatalf("the same records again: %+v", added)
	}

	// Power is cut while meshd runs: nothing recorded. Boot-3's clock
	// was set: the detail says when it started.
	_ = l.ClockSet(t0.Add(20*time.Minute), 0)
	l, added, _ = Open(opts("boot-4", t0.Add(30*time.Minute)))
	if len(added) != 1 || added[0].Verdict != PowerLoss || !strings.Contains(added[0].Detail, "2026-11-19 12:20") {
		t.Fatalf("after the power cut: %+v", added)
	}

	// meshd is stopped by hand and the node rebooted: no verdict, nobody
	// knows how that boot ended.
	_ = l.Stopped(false)
	l, added, _ = Open(opts("boot-5", t0.Add(40*time.Minute)))
	if len(added) != 0 {
		t.Fatalf("after a stop by hand: %+v", added)
	}

	// A serial console reset: only the console log says so. The crash
	// before it left its kernel log in place, already read.
	write(t, pstore, "console-ramoops-0", sysrqConsole, t0.Add(45*time.Minute))
	l, added, _ = Open(opts("boot-6", t0.Add(46*time.Minute)))
	if len(added) != 1 || added[0].Verdict != SysRq {
		t.Fatalf("after the sysrq reset: %+v", added)
	}

	// A clean shutdown leaves a console log too: no verdict.
	_ = l.Stopped(true)
	write(t, pstore, "console-ramoops-0", cleanConsole, t0.Add(50*time.Minute))
	l, added, _ = Open(opts("boot-7", t0.Add(51*time.Minute)))
	if len(added) != 0 {
		t.Fatalf("after a clean shutdown: %+v", added)
	}

	// On a board that keeps no records, a reset says so.
	o := opts("boot-8", t0.Add(60*time.Minute))
	o.Backend = false
	l, added, _ = Open(o)
	if len(added) != 1 || added[0].Verdict != Unrecorded {
		t.Fatalf("without a backend: %+v", added)
	}

	if got := len(l.Entries); got != 4 {
		t.Fatalf("%d entries, want 4: %+v", got, l.Entries)
	}
	// Before boot-8's clock is set, only what boot-8 found is recent.
	if recent := l.Recent(t0.Add(61*time.Minute), 20*time.Minute); len(recent) != 1 || recent[0].Verdict != Unrecorded {
		t.Errorf("recent before the clock is set = %+v", recent)
	}
	// Boot-6 never set its clock: its reset keeps boot-6's clock, marked
	// unknown.
	_ = l.ClockSet(t0.Add(60*time.Minute), 0)
	recent := l.Recent(t0.Add(61*time.Minute), 20*time.Minute)
	if len(recent) != 2 || recent[0].Verdict != Unrecorded || recent[0].TimeUnknown || recent[1].Verdict != SysRq || !recent[1].TimeUnknown || recent[1].Boot != "boot-6" {
		t.Errorf("recent = %+v", recent)
	}
	// The file holds it all for the next start.
	again, err := Load(state)
	if err != nil || len(again.Entries) != 4 || again.BootID != "boot-8" {
		t.Errorf("reloaded: %+v %v", again, err)
	}
}

// meshd stopped for a driver reload (a trial): a crash in the reload is a
// reset; a reload that ends well leaves the log as meshd left it.
func TestResetDuringReload(t *testing.T) {
	state := t.TempDir()
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	opts := func(boot string, now time.Time) Options {
		return Options{StateDir: state, BootID: boot, Now: now, Dirs: []string{t.TempDir()}}
	}
	l, _, _ := Open(opts("boot-1", t0))
	_ = l.ClockSet(t0, 0)
	_ = l.Stopped(false)
	// Another boot's mark, or none: nothing changes.
	Reloading(state, "boot-0")()
	if l, _ := Load(state); l.State != stateStopped {
		t.Fatalf("marked for another boot: %s", l.State)
	}
	// The reload ends well: stopped again.
	Reloading(state, "boot-1")()
	if l, _ := Load(state); l.State != stateStopped {
		t.Fatalf("after a good reload: %s", l.State)
	}
	// The kernel crashes in the reload; the board keeps no records.
	_ = Reloading(state, "boot-1")
	_, added, _ := Open(opts("boot-2", t0.Add(time.Minute)))
	if len(added) != 1 || added[0].Verdict != Unrecorded || !strings.HasPrefix(added[0].Detail, "during a driver module reload; the boot before started") {
		t.Fatalf("after a crash in the reload: %+v", added)
	}
	// A clean shutdown is not marked.
	l, _, _ = Open(opts("boot-2", t0.Add(2*time.Minute)))
	_ = l.Stopped(true)
	Reloading(state, "boot-2")
	if l, _ := Load(state); l.State != stateShutdown {
		t.Errorf("marked after a shutdown: %s", l.State)
	}
}

// A board without an RTC: every boot starts at the same stale time and
// chrony sets the clock a few seconds in. Crashes are dated by the boot's
// start as read once the clock is set, plus the uptime in the record.
func TestNoRTC(t *testing.T) {
	state, pstore := t.TempDir(), t.TempDir()
	stale := time.Date(2026, 11, 14, 0, 44, 8, 0, time.UTC) // what each boot starts at
	real := time.Date(2026, 11, 20, 4, 5, 0, 0, time.UTC)   // when boot-1 really started
	opts := func(boot string, up time.Duration) Options {
		return Options{StateDir: state, BootID: boot, Now: stale.Add(up), Uptime: up, Dirs: []string{pstore}, Backend: true}
	}
	l, _, _ := Open(opts("boot-1", 9*time.Second))
	// meshd's status before the clock is set: the start is not known.
	if l.BootStartKnown {
		t.Fatal("start known before the clock was set")
	}
	_ = l.ClockSet(real.Add(20*time.Second), 20*time.Second)

	// Boot-1 crashes 163.86 s in. Boot-2 starts at the stale time again; the
	// record file has the stale time too.
	write(t, pstore, "dmesg-ramoops-0", serrorDmesg, stale.Add(3*time.Second))
	l, added, _ := Open(opts("boot-2", 9*time.Second))
	want := real.Add(163856978 * time.Microsecond)
	if len(added) != 1 || !added[0].Time.Equal(want) || added[0].TimePending {
		t.Fatalf("crash dated %+v, want %s", added, want)
	}
	// Boot-2's clock is never set, and it loses power: the reset is dated
	// by boot-3's start, once boot-3's clock is set. No "started" detail:
	// boot-2's start was never known.
	l, added, _ = Open(opts("boot-3", 9*time.Second))
	if len(added) != 1 || added[0].Verdict != PowerLoss || !added[0].TimePending || added[0].Detail != "" {
		t.Fatalf("power loss: %+v", added)
	}
	// Before the clock is set, only the reset this boot found is recent:
	// the crash before it, dated days ahead of the stale clock, is not.
	if r := l.Recent(stale.Add(time.Minute), 24*time.Hour); len(r) != 1 || r[0].Verdict != PowerLoss {
		t.Fatalf("recent before the clock is set: %+v", r)
	}
	boot3 := real.Add(3 * time.Hour)
	_ = l.ClockSet(boot3.Add(18*time.Second), 18*time.Second)
	if r := l.Recent(boot3.Add(time.Minute), 24*time.Hour); len(r) != 2 || !r[0].Time.Equal(boot3) || r[0].TimePending || !r[1].Time.Equal(want) {
		t.Fatalf("recent once the clock is set: %+v", r)
	}
	// Kept: the next start reads the same.
	again, _ := Load(state)
	if !again.BootStartKnown || !again.BootStarted.Equal(boot3) || !again.Entries[1].Time.Equal(boot3) {
		t.Errorf("reloaded: %+v", again)
	}
	// Once a boot: a later step of the clock does not move them.
	_ = l.ClockSet(boot3.Add(time.Hour), time.Minute)
	if r := l.Recent(time.Time{}, 0); !r[0].Time.Equal(boot3) {
		t.Errorf("moved by a second ClockSet: %+v", r[0])
	}
}

func TestBrokenLogStartsAgain(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, FileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, added, err := Open(Options{StateDir: state, BootID: "b", Now: time.Now(), Dirs: []string{t.TempDir()}})
	if err == nil || l == nil || len(added) != 0 {
		t.Fatalf("broken log: %+v %v", l, err)
	}
	if again, err := Load(state); err != nil || again.BootID != "b" {
		t.Errorf("not rewritten: %+v %v", again, err)
	}
}

func TestBackend(t *testing.T) {
	root := t.TempDir()
	if Backend(root) {
		t.Error("a backend without pstore")
	}
	write(t, filepath.Join(root, "module", "pstore", "parameters"), "backend", "(null)\n", time.Time{})
	if Backend(root) {
		t.Error("(null) is no backend")
	}
	write(t, filepath.Join(root, "module", "pstore", "parameters"), "backend", "ramoops\n", time.Time{})
	if !Backend(root) {
		t.Error("ramoops not seen")
	}
}
