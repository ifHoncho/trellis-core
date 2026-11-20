package prepare

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/crashlog"
)

type exitErr int

func (e exitErr) Error() string { return "exit status " + string(rune('0'+int(e)%10)) }
func (e exitErr) ExitCode() int { return int(e) }

// fakeHost is a node: files, and commands answered from a table. Running a
// command may change files, as loading a module does.
type fakeHost struct {
	files map[string]string
	cmds  map[string]func(h *fakeHost) (out string, code int)
	ran   []string
}

func newHost() *fakeHost {
	return &fakeHost{files: map[string]string{
		DefaultDir + "/scripts/install-regulatory.sh": "#!/bin/sh",
		DefaultDir + "/packaging/dkms/source":         "BATADV_VERSION=2026.3\nBATADV_SHA256=x\n",
	}, cmds: map[string]func(*fakeHost) (string, int){}}
}

func (h *fakeHost) answer(cmd string, out string, code int) {
	h.cmds[cmd] = func(*fakeHost) (string, int) { return out, code }
}

func (h *fakeHost) exec(name string, args []string) (string, error) {
	line := strings.TrimSpace(strings.TrimPrefix(name, DefaultDir+"/scripts/") + " " + strings.Join(args, " "))
	h.ran = append(h.ran, line)
	f, ok := h.cmds[line]
	if !ok {
		return "", exitErr(1)
	}
	out, code := f(h)
	if code != 0 {
		return out, exitErr(code)
	}
	return out, nil
}

func (h *fakeHost) Run(ctx context.Context, out io.Writer, env []string, name string, args ...string) error {
	s, err := h.exec(name, args)
	io.WriteString(out, s)
	return err
}

func (h *fakeHost) Output(ctx context.Context, name string, args ...string) (string, error) {
	return h.exec(name, args)
}

func (h *fakeHost) ReadFile(path string) ([]byte, error) {
	s, ok := h.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(s), nil
}

func (h *fakeHost) WriteFile(path string, data []byte, perm os.FileMode) error {
	h.files[path] = string(data)
	return nil
}

func (h *fakeHost) Glob(pattern string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for p := range h.files {
		for _, cand := range []string{p, filepath.Dir(p), filepath.Dir(filepath.Dir(p))} {
			if ok, _ := filepath.Match(pattern, cand); ok && !seen[cand] {
				seen[cand] = true
				out = append(out, cand)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

func (h *fakeHost) did(cmd string) bool { return slices.Contains(h.ran, cmd) }

// A node with everything in place: nothing to do, nothing stopped.
func ready() *fakeHost {
	h := newHost()
	h.files["/sys/module/batman_adv/version"] = "2026.3\n"
	h.answer("dpkg-query -W -f=${Version} trellis-batman-adv-dkms", "2026.3-1", 0)
	h.answer("modinfo -F version batman-adv", "2026.3", 0)
	h.answer("modinfo -n batman-adv", "/lib/modules/6.1.0/updates/dkms/batman-adv.ko", 0)
	h.files["/sys/module/batman_adv/srcversion"] = "AAAA1111\n"
	h.answer("modinfo -F srcversion batman-adv", "AAAA1111", 0)
	h.answer("systemctl is-active --quiet trellis-meshd", "", 0)
	h.answer("install-regulatory.sh", "-- in effect\n", 0)
	return h
}

func prepare(t *testing.T, h *fakeHost, opts ...string) (*Result, string) {
	t.Helper()
	var out bytes.Buffer
	res, err := Run(context.Background(), Options{Host: h, Out: &out, DriverOptions: opts})
	if err != nil {
		t.Fatal(err)
	}
	return res, out.String()
}

func TestNothingToDo(t *testing.T) {
	h := ready()
	res, out := prepare(t, h)
	if len(res.Problems) > 0 || len(res.Reboot) > 0 || h.did("systemctl stop trellis-meshd") {
		t.Fatalf("%+v\n%s", res, out)
	}
	if !strings.Contains(out, "2026.3 (DKMS build) is loaded") || !strings.Contains(out, "no MT7915/MT7916 card") {
		t.Errorf("output:\n%s", out)
	}
	if _, ok := h.files["/run/reboot-required"]; ok {
		t.Error("no reboot needed, none marked")
	}
}

// The DKMS build is installed but the kernel's own module is loaded: meshd
// stops, the module is swapped, meshd starts again.
func TestBatmanReload(t *testing.T) {
	h := ready()
	h.files["/sys/module/batman_adv/version"] = "2022.3"
	h.answer("systemctl stop trellis-meshd", "", 0)
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("modprobe -r batman-adv", "", 0)
	h.cmds["modprobe batman-adv"] = func(h *fakeHost) (string, int) {
		h.files["/sys/module/batman_adv/version"] = "2026.3"
		return "", 0
	}
	res, out := prepare(t, h)
	if len(res.Problems) > 0 {
		t.Fatalf("%+v\n%s", res, out)
	}
	order := strings.Join(h.ran, "|")
	stop, load, start := strings.Index(order, "systemctl stop"), strings.Index(order, "modprobe batman-adv"), strings.Index(order, "systemctl start")
	if stop < 0 || load < stop || start < load {
		t.Errorf("stop, load, start in that order: %v", h.ran)
	}
	if !strings.Contains(out, "loaded: 2026.3") {
		t.Errorf("output:\n%s", out)
	}
}

// While meshd is stopped for the module to load, its reset history is
// marked: a crash in the load is a reset. Taken back after.
func TestReloadMarked(t *testing.T) {
	state := t.TempDir()
	l, _, err := crashlog.Open(crashlog.Options{StateDir: state, BootID: "boot-1", Now: time.Now(), Dirs: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	h := ready()
	h.files["/sys/module/batman_adv/version"] = "2022.3"
	h.cmds["systemctl stop trellis-meshd"] = func(*fakeHost) (string, int) {
		_ = l.Stopped(false) // what meshd does at the stop
		return "", 0
	}
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("modprobe -r batman-adv", "", 0)
	during := ""
	h.cmds["modprobe batman-adv"] = func(h *fakeHost) (string, int) {
		if l, err := crashlog.Load(state); err == nil {
			during = l.State
		}
		h.files["/sys/module/batman_adv/version"] = "2026.3"
		return "", 0
	}
	var out bytes.Buffer
	if _, err := Run(context.Background(), Options{Host: h, Out: &out, StateDir: state, BootID: "boot-1"}); err != nil {
		t.Fatal(err)
	}
	after, _ := crashlog.Load(state)
	if during != "reload" || after.State != "stopped" {
		t.Errorf("history during the load %q, after %q", during, after.State)
	}
}

// Interrupted in a step: the rest is skipped, meshd is started again,
// and the run says it stopped early.
func TestInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := ready()
	h.files["/sys/module/batman_adv/version"] = "2022.3"
	h.answer("systemctl stop trellis-meshd", "", 0)
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("modprobe -r batman-adv", "", 0)
	h.cmds["modprobe batman-adv"] = func(*fakeHost) (string, int) {
		cancel() // Ctrl-C
		return "", 130
	}
	var out bytes.Buffer
	_, err := Run(ctx, Options{Host: h, Out: &out})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v", err)
	}
	if !h.did("systemctl start trellis-meshd") || h.did("install-regulatory.sh") {
		t.Errorf("ran %v", h.ran)
	}
}

// The same release, rebuilt with new patches (a new packaging revision):
// the loaded module is not the installed file, so it is reloaded.
func TestBatmanNewPatches(t *testing.T) {
	h := ready()
	h.files["/sys/module/batman_adv/srcversion"] = "OLD00000"
	h.answer("systemctl stop trellis-meshd", "", 0)
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("modprobe -r batman-adv", "", 0)
	h.cmds["modprobe batman-adv"] = func(h *fakeHost) (string, int) {
		h.files["/sys/module/batman_adv/srcversion"] = "AAAA1111"
		return "", 0
	}
	res, out := prepare(t, h)
	if len(res.Problems) > 0 || !h.did("modprobe batman-adv") {
		t.Fatalf("not reloaded: %+v\n%s", res, out)
	}
}

// Not built for this kernel: the script builds it, with the package's
// release; a failed build is a problem, and the other steps still run.
func TestBatmanBuild(t *testing.T) {
	h := ready()
	delete(h.files, "/sys/module/batman_adv/version")
	h.answer("modinfo -F version batman-adv", "2022.3", 0)
	h.answer("modinfo -n batman-adv", "/lib/modules/6.1.0/kernel/net/batman-adv/batman-adv.ko", 0)
	h.answer("systemctl stop trellis-meshd", "", 0)
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("install-batman-adv-dkms.sh", "!! no headers\n", 1)
	res, _ := prepare(t, h)
	if !h.did("install-batman-adv-dkms.sh") || len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "batman-adv 2026.3") {
		t.Fatalf("%+v %v", res, h.ran)
	}
	if !h.did("install-regulatory.sh") || !h.did("systemctl start trellis-meshd") {
		t.Errorf("the rest still runs, and meshd comes back: %v", h.ran)
	}
}

// Without the DKMS package the pin file says which release.
func TestPinFromFile(t *testing.T) {
	h := ready()
	delete(h.cmds, "dpkg-query -W -f=${Version} trellis-batman-adv-dkms")
	_, out := prepare(t, h)
	if !strings.Contains(out, "== batman-adv 2026.3") {
		t.Errorf("output:\n%s", out)
	}
}

// An MT7916 without a driver: the script builds it and asks for a reboot,
// which is reported and marked.
func TestMT7915eAndReboot(t *testing.T) {
	h := ready()
	h.files["/sys/bus/pci/devices/0000:01:00.0/vendor"] = "0x14c3\n"
	h.files["/sys/bus/pci/devices/0000:01:00.0/device"] = "0x7906\n"
	h.files["/sys/bus/pci/devices/0000:00:00.0/vendor"] = "0x14e4\n"
	h.answer("install-mt7915e-dkms.sh", "-- built\n== REBOOT NEEDED: the driver is installed but not loaded; it starts after a reboot\n", 0)
	res, out := prepare(t, h)
	if !h.did("install-mt7915e-dkms.sh") || len(res.Reboot) != 1 || !strings.HasPrefix(res.Reboot[0], "REBOOT NEEDED") {
		t.Fatalf("%+v\n%s", res, out)
	}
	if h.files["/run/reboot-required"] == "" || !strings.Contains(h.files["/run/reboot-required.pkgs"], "trellis") {
		t.Error("the reboot is marked for other tools")
	}
	// Again: the mark is not repeated.
	prepare(t, h)
	if strings.Count(h.files["/run/reboot-required.pkgs"], "trellis") != 1 {
		t.Errorf("pkgs: %q", h.files["/run/reboot-required.pkgs"])
	}
}

func TestDriverOptions(t *testing.T) {
	h := ready()
	h.files["/sys/module/mt76x2u/parameters/disable_usb_sg"] = "N"
	h.files["/sys/module/ath9k_htc/parameters/nohwcrypt"] = "0"
	h.answer("systemctl stop trellis-meshd", "", 0)
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("modprobe -r mt76x2u", "", 0)
	h.answer("modprobe mt76x2u", "", 0)
	res, out := prepare(t, h,
		"options mt76x2u disable_usb_sg=1",
		"options ath9k_htc nohwcrypt=1",
		"options rtl8xxxu ht40_2g=1") // not loaded
	if !h.did("modprobe -r mt76x2u") || !h.did("modprobe mt76x2u") {
		t.Errorf("mt76x2u reloaded: %v", h.ran)
	}
	if h.did("modprobe -r ath9k_htc") || len(res.Advice) != 1 || !strings.Contains(res.Advice[0], "replug") {
		t.Errorf("ath9k_htc only advised: %+v %v", res, h.ran)
	}
	if !strings.Contains(out, "rtl8xxxu: not loaded") {
		t.Errorf("output:\n%s", out)
	}
}

// A kernel that cannot ask userspace (exit 10): the fallback database.
func TestRegulatoryFallback(t *testing.T) {
	h := ready()
	h.answer("install-regulatory.sh", "", 10)
	h.answer("install-regdb-fallback.sh", "-- installed\n", 0)
	res, _ := prepare(t, h)
	if !h.did("install-regdb-fallback.sh") || len(res.Problems) > 0 {
		t.Errorf("%+v %v", res, h.ran)
	}
	h = ready()
	h.answer("install-regulatory.sh", "!! takes effect at the next boot: the kernel keeps the database it loaded at start\n", 0)
	if res, _ := prepare(t, h); len(res.Reboot) != 1 {
		t.Errorf("the next boot: %+v", res)
	}
}

func TestNeedsThePackage(t *testing.T) {
	h := ready()
	delete(h.files, DefaultDir+"/scripts/install-regulatory.sh")
	if _, err := Run(context.Background(), Options{Host: h, Out: io.Discard}); err == nil || !strings.Contains(err.Error(), "install the trellis package") {
		t.Errorf("err: %v", err)
	}
}

// An MT7916 on a kernel whose own mt7915e predates the band selection:
// meshd is stopped and the script builds the replacement.
func TestMT7916OldDriver(t *testing.T) {
	h := ready()
	h.files["/sys/bus/pci/devices/0000:01:00.0/vendor"] = "0x14c3\n"
	h.files["/sys/bus/pci/devices/0000:01:00.0/device"] = "0x7906\n"
	h.answer("modinfo -n mt7915e", "/lib/modules/6.1.0/kernel/drivers/net/wireless/mediatek/mt76/mt7915/mt7915e.ko\n", 0)
	h.answer("modinfo -F parm mt7915e", "", 0)
	h.answer("systemctl is-active --quiet trellis-meshd", "", 0)
	h.answer("systemctl stop trellis-meshd", "", 0)
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("install-mt7915e-dkms.sh", "== OK\n", 0)
	res, out := prepare(t, h)
	if !h.did("install-mt7915e-dkms.sh") || !h.did("systemctl stop trellis-meshd") || !h.did("systemctl start trellis-meshd") || len(res.Problems) != 0 {
		t.Fatalf("%+v\n%s\nran: %v", res, out, h.ran)
	}
	if !strings.Contains(out, "predates Linux 6.14") {
		t.Errorf("output: %s", out)
	}
}

// This package's build is installed and has no band selection (the
// kernel's source would not take it): not built again, meshd not stopped.
func TestMT7916BuildWithoutBandSelect(t *testing.T) {
	h := ready()
	h.files["/sys/bus/pci/devices/0000:01:00.0/vendor"] = "0x14c3\n"
	h.files["/sys/bus/pci/devices/0000:01:00.0/device"] = "0x7906\n"
	h.files["/proc/sys/kernel/osrelease"] = "6.6.51+rpt-rpi-v8\n"
	h.files[DefaultDir+"/scripts/install-mt7915e-dkms.sh"] = "#!/bin/sh\nset -eu\n\nPKG_VERSION=2   # bump when packaging/dkms-mt7915e changes\n"
	h.answer("modinfo -n mt7915e", "/lib/modules/6.6.51+rpt-rpi-v8/updates/dkms/mt7915e.ko.xz\n", 0)
	h.answer("modinfo -F parm mt7915e", "", 0)
	h.answer("dkms status -m trellis-mt7915e -v 2 -k 6.6.51+rpt-rpi-v8", "trellis-mt7915e/2, 6.6.51+rpt-rpi-v8, aarch64: installed\n", 0)
	res, out := prepare(t, h)
	if h.did("install-mt7915e-dkms.sh") || h.did("systemctl stop trellis-meshd") || len(res.Problems) != 0 || len(res.Advice) != 1 {
		t.Fatalf("%+v\n%s\nran: %v", res, out, h.ran)
	}
	// An earlier version of the package's build is replaced.
	h = ready()
	h.files["/sys/bus/pci/devices/0000:01:00.0/vendor"] = "0x14c3\n"
	h.files["/sys/bus/pci/devices/0000:01:00.0/device"] = "0x7906\n"
	h.files["/proc/sys/kernel/osrelease"] = "6.6.51+rpt-rpi-v8\n"
	h.files[DefaultDir+"/scripts/install-mt7915e-dkms.sh"] = "PKG_VERSION=3\n"
	h.answer("modinfo -n mt7915e", "/lib/modules/6.6.51+rpt-rpi-v8/updates/dkms/mt7915e.ko.xz\n", 0)
	h.answer("modinfo -F parm mt7915e", "", 0)
	h.answer("dkms status -m trellis-mt7915e -v 3 -k 6.6.51+rpt-rpi-v8", "", 0)
	h.answer("systemctl is-active --quiet trellis-meshd", "", 0)
	h.answer("systemctl stop trellis-meshd", "", 0)
	h.answer("systemctl start trellis-meshd", "", 0)
	h.answer("install-mt7915e-dkms.sh", "== OK\n", 0)
	if res, out = prepare(t, h); !h.did("install-mt7915e-dkms.sh") || len(res.Advice) != 0 {
		t.Fatalf("earlier build: %+v\n%s", res, out)
	}
}

// A driver with the band selection, and an MT7915 (which has no such
// radio) on an old driver, are left alone.
func TestMT7915eCurrentOrNotMT7916(t *testing.T) {
	for _, c := range []struct{ device, parm string }{
		{"0x7906", "enable_6ghz:Enable 6 GHz instead of 5 GHz on hardware that supports both (bool)\n"},
		{"0x7915", ""},
	} {
		h := ready()
		h.files["/sys/bus/pci/devices/0000:01:00.0/vendor"] = "0x14c3\n"
		h.files["/sys/bus/pci/devices/0000:01:00.0/device"] = c.device + "\n"
		h.answer("modinfo -n mt7915e", "/lib/modules/x/updates/dkms/mt7915e.ko\n", 0)
		h.answer("modinfo -F parm mt7915e", c.parm, 0)
		res, out := prepare(t, h)
		if h.did("install-mt7915e-dkms.sh") || h.did("systemctl stop trellis-meshd") || len(res.Problems) != 0 || !strings.Contains(out, "mt7915e is available") {
			t.Errorf("%s: %+v\n%s", c.device, res, out)
		}
	}
}
